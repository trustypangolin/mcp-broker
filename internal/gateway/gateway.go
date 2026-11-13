// Package gateway is the MCP server agents talk to. It exposes a handful of meta-tools,
// starts downstream connectors on demand, filters what each agent sees, and runs every
// call through policy and, when required, approval.
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trustypangolin/mcp-broker/internal/approval"
	"github.com/trustypangolin/mcp-broker/internal/audit"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/identity"
	"github.com/trustypangolin/mcp-broker/internal/oauth"
	"github.com/trustypangolin/mcp-broker/internal/policy"
	"github.com/trustypangolin/mcp-broker/internal/redact"
	"github.com/trustypangolin/mcp-broker/internal/supervisor"
)

// Options wires a Gateway to the rest of the broker.
type Options struct {
	Config *config.Config
	// Policy defaults to policy.New(Config); Reload replaces it.
	Policy    *policy.Engine
	Approvals *approval.Queue
	Audit     *audit.Log
	Redactor  *redact.Redactor
	Logger    *slog.Logger
	// Supervisor runs profiles; nil means no profile tools work.
	Supervisor *supervisor.Supervisor
	// OAuth signs in to connectors that use oauth; nil means they cannot connect.
	OAuth *oauth.Manager
	// DashboardURL is shown to agents whose approval must happen out of band.
	DashboardURL string
	// LogDir receives one stderr log per local connector; empty disables.
	LogDir string
	// ToolCache is where each connector's last-seen tool list is kept, so the dashboard
	// can show tools of connectors that are not running; empty keeps it in memory only.
	ToolCache string
	// Identity extracts the caller from a request. Defaults to IdentityFromRequest.
	Identity func(*mcp.CallToolRequest) identity.Identity
	// ApprovalWait, when set, overrides approvals.wait: how long call_tool blocks for an
	// out-of-band approval before returning pending_approval. Tests use it.
	ApprovalWait time.Duration
}

// Gateway serves the meta-tools.
type Gateway struct {
	opts Options
	cur  atomic.Pointer[state]
	red  *redact.Redactor
	log  *slog.Logger

	mu    sync.Mutex
	conns map[string]*downstream

	catMu   sync.Mutex
	catalog map[string]ToolList // connector name → tools, loaded on first use

	srvs servers // per-agent MCP servers (ServerFor)

	stop chan struct{}
	done sync.WaitGroup
}

// New returns a Gateway and starts its idle-unload loop.
func New(opts Options) *Gateway {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Identity == nil {
		opts.Identity = IdentityFromRequest
	}
	if opts.Policy == nil {
		opts.Policy = policy.New(opts.Config)
	}
	g := &Gateway{opts: opts, red: opts.Redactor, log: opts.Logger, conns: map[string]*downstream{}, stop: make(chan struct{})}
	g.cur.Store(&state{cfg: opts.Config, pol: opts.Policy})
	g.done.Add(1)
	go g.idleLoop()
	return g
}

// state is the config a call runs against; Reload swaps it whole.
type state struct {
	cfg *config.Config
	pol *policy.Engine
}

func (g *Gateway) st() *state { return g.cur.Load() }

// Reload switches the gateway to cfg. Connectors that were removed or whose definition
// changed are unloaded (calls in flight on them fail) and restart on next use with the new
// definition; the others keep running. It returns the unloaded connector keys.
func (g *Gateway) Reload(cfg *config.Config) []string {
	g.cur.Store(&state{cfg: cfg, pol: policy.New(cfg)})
	g.syncServers()
	g.mu.Lock()
	var stale []*downstream
	for key, d := range g.conns {
		if cn, ok := cfg.Connectors[d.name]; !ok || !sameDef(cn, d.cn) {
			stale = append(stale, d)
			delete(g.conns, key)
		}
	}
	g.mu.Unlock()
	var keys []string
	for _, d := range stale {
		if d.loaded() {
			keys = append(keys, d.key)
		}
		d.close()
	}
	slices.Sort(keys)
	return keys
}

// sameDef reports whether two definitions start the same server: the dashboard group is
// only a label, so regrouping keeps running connectors.
func sameDef(a, b config.Connector) bool {
	a.Group, b.Group = "", ""
	return reflect.DeepEqual(a, b)
}

// Connector describes one connector instance for the dashboard and tray.
type Connector struct {
	Key    string   `json:"key"`
	Name   string   `json:"name"`
	Loaded bool     `json:"loaded"`
	Tools  int      `json:"tools"`
	PID    int      `json:"pid,omitempty"`
	Remote bool     `json:"remote,omitempty"`
	Auth   string   `json:"auth,omitempty"`   // OAuth sign-in state, for connectors that use it
	Stderr []string `json:"stderr,omitempty"` // recent lines, redacted
}

func (g *Gateway) authState(name string, cn config.Connector) string {
	o, implicit := cn.OAuthSettings()
	if o == nil || g.opts.OAuth == nil {
		return ""
	}
	s := g.opts.OAuth.State(name)
	if implicit && s == "not signed in" && !g.opts.OAuth.Registered(name) {
		return "" // a URL-only connector that hasn't asked for a sign-in may need none
	}
	return s
}

// Connectors lists every configured connector, with the state of each instance that has
// been used (a per_agent connector may have several).
func (g *Gateway) Connectors() []Connector {
	cfg := g.st().cfg
	g.mu.Lock()
	byName := map[string][]*downstream{}
	for _, d := range g.conns {
		byName[d.name] = append(byName[d.name], d)
	}
	g.mu.Unlock()
	var out []Connector
	for _, name := range slices.Sorted(maps.Keys(cfg.Connectors)) {
		ds := byName[name]
		if len(ds) == 0 {
			cn := cfg.Connectors[name]
			out = append(out, Connector{Key: name, Name: name, Remote: cn.Remote(), Auth: g.authState(name, cn)})
			continue
		}
		slices.SortFunc(ds, func(a, b *downstream) int { return strings.Compare(a.key, b.key) })
		for _, d := range ds {
			c := Connector{Key: d.key, Name: name, Loaded: d.loaded(), Remote: d.cn.Remote(), Auth: g.authState(name, d.cn), Stderr: d.recentStderr(20)}
			_, tools := d.snapshot()
			c.Tools = len(tools)
			if p := d.process(); p != nil {
				c.PID = p.PID()
			}
			out = append(out, c)
		}
	}
	return out
}

// Unload stops a connector instance by key (as listed by Connectors). It reports whether
// the instance was loaded.
func (g *Gateway) Unload(key string) bool {
	g.mu.Lock()
	d, ok := g.conns[key]
	g.mu.Unlock()
	if !ok || !d.loaded() {
		return false
	}
	d.close()
	return true
}

// Close unloads every connector, stopping their processes.
func (g *Gateway) Close() {
	close(g.stop)
	g.done.Wait()
	g.mu.Lock()
	conns := g.conns
	g.conns = map[string]*downstream{}
	g.mu.Unlock()
	var wg sync.WaitGroup
	for _, d := range conns {
		wg.Add(1)
		go func() { defer wg.Done(); d.close() }()
	}
	wg.Wait()
}

// IdentityKey is the TokenInfo.Extra key holding the caller's identity.Identity.
const IdentityKey = "mcp-broker/identity"

// IdentityFromRequest reads the identity the HTTP layer attached to the request.
func IdentityFromRequest(req *mcp.CallToolRequest) identity.Identity {
	if req.Extra != nil && req.Extra.TokenInfo != nil {
		if id, ok := req.Extra.TokenInfo.Extra[IdentityKey].(identity.Identity); ok {
			return id
		}
	}
	return identity.Identity{Role: config.AnonymousRole}
}

func (g *Gateway) conn(name, agent string) (*downstream, bool) {
	cn, ok := g.st().cfg.Connectors[name]
	if !ok {
		return nil, false
	}
	key := name
	if cn.Instance == "per_agent" {
		key = name + "@" + agent
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	d, ok := g.conns[key]
	if !ok {
		d = &downstream{g: g, key: key, name: name, cn: cn}
		g.conns[key] = d
	}
	return d, true
}

func (g *Gateway) idleLoop() {
	defer g.done.Done()
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-g.stop:
			return
		case <-t.C:
		}
		g.mu.Lock()
		var idle []*downstream
		for _, d := range g.conns {
			if d.cn.IdleUnload <= 0 {
				continue
			}
			d.callsMu.Lock()
			busy := len(d.calls) > 0
			d.callsMu.Unlock()
			if last, loaded := d.idleSince(); loaded && !busy && time.Since(last) > d.cn.IdleUnload {
				idle = append(idle, d)
			}
		}
		g.mu.Unlock()
		for _, d := range idle {
			g.log.Info("idle unload", "connector", d.key)
			d.close()
			g.write(audit.Event{Kind: audit.KindStop, Connector: d.key, Detail: "idle_unload"})
		}
	}
}

func schema(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	// No additionalProperties: false. It costs every agent's context bytes on every schema,
	// and the handlers ignore fields they don't know anyway.
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func textResult(isErr bool, format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: isErr, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}}
}

func jsonResult(v any) *mcp.CallToolResult {
	b, _ := json.MarshalIndent(v, "", "  ")
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}, StructuredContent: v}
}

func decodeArgs(req *mcp.CallToolRequest, v any) error {
	if len(req.Params.Arguments) == 0 {
		return nil
	}
	if err := json.Unmarshal(req.Params.Arguments, v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

type connectorInfo struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Aliases     []string `json:"aliases,omitempty"`
	Loaded      bool     `json:"loaded"`
}

type groupInfo struct {
	Group       string          `json:"group"`
	Description string          `json:"description,omitempty"`
	Connectors  []connectorInfo `json:"connectors"`
}

// listConnectors lists what the agent can see, grouped as on the dashboard: named groups
// first (with their descriptions), then the ungrouped connectors.
func (g *Gateway) listConnectors(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := g.opts.Identity(req)
	cfg := g.st().cfg
	ungrouped := []connectorInfo{}
	byGroup := map[string][]connectorInfo{}
	for _, name := range g.st().pol.VisibleConnectors(id.Agent) {
		d, _ := g.conn(name, id.Agent)
		cn := cfg.Connectors[name]
		info := connectorInfo{Name: name, Description: g.AgentDescription(name), Aliases: cn.Aliases, Loaded: d.loaded()}
		if grp := strings.TrimSpace(cn.Group); grp != "" {
			byGroup[grp] = append(byGroup[grp], info)
		} else {
			ungrouped = append(ungrouped, info)
		}
	}
	out := map[string]any{}
	if len(byGroup) > 0 {
		groups := []groupInfo{}
		for _, grp := range slices.Sorted(maps.Keys(byGroup)) {
			groups = append(groups, groupInfo{Group: grp, Description: cfg.Groups[grp].Description, Connectors: byGroup[grp]})
		}
		out["groups"] = groups
	}
	if len(ungrouped) > 0 || len(byGroup) == 0 {
		out["connectors"] = ungrouped
	}
	return jsonResult(out), nil
}

type toolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"inputSchema"`
	Class       string `json:"class"`
	Approval    bool   `json:"requires_approval,omitempty"`
}

// loadable resolves a connector for an agent, starting it if needed. Unknown and invisible
// connectors produce the same message so an agent cannot probe for hidden ones.
func (g *Gateway) loadable(ctx context.Context, id identity.Identity, name string) (*downstream, *mcp.CallToolResult) {
	if !g.st().pol.Visible(id.Agent, name) {
		g.write(audit.Event{Kind: audit.KindToolCall, Agent: id.Agent, Connector: name, Decision: string(config.Deny), Detail: "connector not visible"})
		return nil, textResult(true, "unknown connector %q", name)
	}
	d, _ := g.conn(name, id.Agent)
	start := time.Now()
	wasLoaded := d.loaded()
	if err := d.ensure(ctx); err != nil {
		msg := g.red.String(err.Error())
		g.write(audit.Event{Kind: audit.KindProcessStart, Agent: id.Agent, Connector: d.key, Error: msg, DurationMS: time.Since(start).Milliseconds()})
		return nil, textResult(true, "connector %s failed to start: %s", name, msg)
	}
	if !wasLoaded {
		e := audit.Event{Kind: audit.KindProcessStart, Agent: id.Agent, Connector: d.key, DurationMS: time.Since(start).Milliseconds()}
		if d.proc != nil {
			e.PID = d.proc.PID()
		}
		g.write(e)
	}
	return d, nil
}

func (g *Gateway) loadConnector(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct{ Name, Detail string }
	if err := decodeArgs(req, &a); err != nil {
		return textResult(true, "%v", err), nil
	}
	if a.Detail != "" && a.Detail != "summary" && a.Detail != "full" {
		return textResult(true, "detail is summary or full, not %q", a.Detail), nil
	}
	id := g.opts.Identity(req)
	d, bad := g.loadable(ctx, id, a.Name)
	if bad != nil {
		return bad, nil
	}
	_, tools := d.snapshot()
	if a.Detail == "full" {
		out := []toolInfo{}
		for _, t := range tools {
			dec := g.st().pol.Tool(id.Agent, a.Name, t.Name, annotations(t))
			if dec.Action == config.Deny {
				continue // denied tools are hidden, not advertised
			}
			out = append(out, toolInfo{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema, Class: string(dec.Class), Approval: dec.Action == config.Approve})
		}
		return jsonResult(map[string]any{"connector": a.Name, "tools": out}), nil
	}
	out := []toolSummary{}
	for _, t := range tools {
		dec := g.st().pol.Tool(id.Agent, a.Name, t.Name, annotations(t))
		if dec.Action == config.Deny {
			continue
		}
		out = append(out, toolSummary{Name: t.Name, Summary: summarize(t.Description), Args: requiredArgs(t.InputSchema), Class: string(dec.Class), Approval: dec.Action == config.Approve})
	}
	return summaryResult(a.Name, out), nil
}

// toolSummary is one line of load_connector's summary: enough to pick a tool, and to name
// its required arguments, which is where an agent skipping describe_tool most often guesses
// wrong.
type toolSummary struct {
	Name     string   `json:"name"`
	Summary  string   `json:"summary,omitempty"`
	Args     []string `json:"args,omitempty"`
	Class    string   `json:"class"`
	Approval bool     `json:"requires_approval,omitempty"`
}

// requiredArgs is the required property names of a tool's input schema, in the schema's
// order. The schema arrives as whatever the SDK decoded, so it is read back through JSON.
func requiredArgs(schema any) []string {
	if schema == nil {
		return nil
	}
	b, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	var s struct {
		Required []string `json:"required"`
	}
	if json.Unmarshal(b, &s) != nil {
		return nil
	}
	return s.Required
}

// summaryMax caps a summary line's description.
const summaryMax = 100

// summarize is a description's first sentence (or line), at most summaryMax characters.
func summarize(desc string) string {
	s := strings.TrimSpace(desc)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	for i := 0; i+1 < len(s); i++ {
		if (s[i] == '.' || s[i] == '!' || s[i] == '?') && s[i+1] == ' ' && !abbreviation(s, i) {
			s = s[:i+1]
			break
		}
	}
	if r := []rune(s); len(r) > summaryMax {
		s = strings.TrimRight(string(r[:summaryMax-1]), " ,;:") + "…"
	}
	return s
}

// abbreviation reports whether the full stop at s[i] ends an abbreviation such as "e.g."
// rather than a sentence: cutting "Run a command, e.g. execute {...}" at "e.g." leaves a
// summary that stops mid-thought.
func abbreviation(s string, i int) bool {
	if s[i] != '.' {
		return false
	}
	start := strings.LastIndexAny(s[:i], " ([") + 1
	switch strings.ToLower(s[start:i]) {
	case "e.g", "i.e", "eg", "ie", "vs", "approx", "incl", "cf", "no", "ca":
		return true
	}
	return false
}

// summaryNext tells the agent how to go on from a summary.
const summaryNext = "args are the required arguments; call describe_tool for a tool's full input schema."

// summaryResult renders a summary compactly, one tool per line: indentation would cost
// the agent more than the content.
func summaryResult(connector string, tools []toolSummary) *mcp.CallToolResult {
	var b strings.Builder
	name, _ := json.Marshal(connector)
	fmt.Fprintf(&b, "{\"connector\":%s,\"detail\":\"summary\",\"tools\":[", name)
	for i, t := range tools {
		line, _ := json.Marshal(t)
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("\n")
		b.Write(line)
	}
	next, _ := json.Marshal(summaryNext)
	fmt.Fprintf(&b, "\n],\"next\":%s}", next)
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.String()}},
		StructuredContent: map[string]any{"connector": connector, "detail": "summary", "tools": tools, "next": summaryNext}}
}

// describeTool returns one tool's full description and input schema, under the same
// rules as call_tool: hidden and denied tools are unknown.
func (g *Gateway) describeTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct{ Connector, Tool string }
	if err := decodeArgs(req, &a); err != nil {
		return textResult(true, "%v", err), nil
	}
	id := g.opts.Identity(req)
	d, bad := g.loadable(ctx, id, a.Connector)
	if bad != nil {
		return bad, nil
	}
	t := d.tool(a.Tool)
	if t == nil {
		return textResult(true, "unknown tool %q on connector %q", a.Tool, a.Connector), nil
	}
	dec := g.st().pol.Tool(id.Agent, a.Connector, a.Tool, annotations(t))
	if dec.Action == config.Deny {
		return textResult(true, "unknown tool %q on connector %q", a.Tool, a.Connector), nil
	}
	return jsonResult(map[string]any{"connector": a.Connector, "tool": toolInfo{Name: t.Name, Description: t.Description,
		InputSchema: t.InputSchema, Class: string(dec.Class), Approval: dec.Action == config.Approve}}), nil
}

func (g *Gateway) unloadConnector(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct{ Name string }
	if err := decodeArgs(req, &a); err != nil {
		return textResult(true, "%v", err), nil
	}
	id := g.opts.Identity(req)
	if !g.st().pol.Visible(id.Agent, a.Name) {
		return textResult(true, "unknown connector %q", a.Name), nil
	}
	d, _ := g.conn(a.Name, id.Agent)
	d.callsMu.Lock()
	busy := len(d.calls)
	d.callsMu.Unlock()
	if busy > 0 {
		return textResult(true, "connector %s has %d call(s) in flight; not unloading", a.Name, busy), nil
	}
	d.close()
	g.write(audit.Event{Kind: audit.KindStop, Agent: id.Agent, Connector: d.key, Detail: "unload_connector"})
	return textResult(false, "unloaded %s", a.Name), nil
}

func (g *Gateway) approvalStatus(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		ApprovalID string `json:"approval_id"`
		Wait       int    `json:"wait"`
	}
	if err := decodeArgs(req, &a); err != nil {
		return textResult(true, "%v", err), nil
	}
	id := g.opts.Identity(req)
	e, ok := g.opts.Approvals.Get(a.ApprovalID)
	if !ok || e.Agent != id.Agent {
		return textResult(true, "unknown approval id %q", a.ApprovalID), nil
	}
	if e.Status == approval.Pending && a.Wait > 0 {
		// Long-poll, capped at approvals.wait so the call stays inside client timeouts.
		d := min(time.Duration(a.Wait)*time.Second, g.approvalWait())
		wctx, cancel := context.WithTimeout(ctx, d)
		stop := g.keepalive(wctx, req)
		e, _ = g.opts.Approvals.Wait(wctx, e.ID)
		stop()
		cancel()
	}
	out := map[string]any{"approval_id": e.ID, "status": e.Status, "decided_by": e.Channel, "expires": e.Expires}
	if e.Status == approval.Pending {
		expiresIn, retryAfter := g.pendingTiming(e)
		out["expires_in_seconds"], out["retry_after_seconds"] = expiresIn, retryAfter
	}
	return jsonResult(out), nil
}

func annotations(t *mcp.Tool) policy.Annotations {
	if t == nil || t.Annotations == nil {
		return policy.Annotations{}
	}
	return policy.Annotations{DestructiveHint: t.Annotations.DestructiveHint}
}

type callArgs struct {
	Connector  string          `json:"connector"`
	Tool       string          `json:"tool"`
	Arguments  json.RawMessage `json:"arguments"`
	ApprovalID string          `json:"approval_id"`
}

func (g *Gateway) callTool(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a callArgs
	if err := decodeArgs(req, &a); err != nil {
		return textResult(true, "%v", err), nil
	}
	id := g.opts.Identity(req)
	start := time.Now()
	ev := audit.Event{Kind: audit.KindToolCall, Agent: id.Agent, Source: id.Source.String(), Connector: a.Connector, Tool: a.Tool, ArgsHash: audit.HashArgs(a.Arguments)}
	if ci := req.ClientInfo(); ci != nil {
		ev.ClientName = ci.Name
	}

	d, bad := g.loadable(ctx, id, a.Connector)
	if bad != nil {
		return bad, nil
	}
	t := d.tool(a.Tool)
	var dec policy.Decision
	if t != nil {
		dec = g.st().pol.Tool(id.Agent, a.Connector, a.Tool, annotations(t))
	}
	if t == nil || dec.Action == config.Deny {
		ev.Decision, ev.Detail, ev.Outcome = string(config.Deny), "unknown or denied tool", audit.OutcomeRefused
		if t != nil {
			ev.Class, ev.Detail = string(dec.Class), dec.Reason
		}
		g.write(ev)
		return textResult(true, "unknown tool %q on connector %q", a.Tool, a.Connector), nil
	}
	ev.Class, ev.Decision = string(dec.Class), string(dec.Action)

	if dec.Action == config.Approve {
		res, ch, outcome := g.approve(ctx, req, id, dec, a, ev.ArgsHash)
		ev.Channel = string(ch)
		if outcome != "" {
			ev.Detail, ev.Outcome = "not approved", outcome
			ev.DurationMS = time.Since(start).Milliseconds()
			g.write(ev)
			return res, nil
		}
		ev.Detail = "approved"
	}

	res, err := g.forward(ctx, req, id, d, a)
	ev.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		ev.Error, ev.Outcome = g.red.String(err.Error()), audit.OutcomeFailed
		g.write(ev)
		msg := fmt.Sprintf("%s/%s failed: %s", a.Connector, a.Tool, ev.Error)
		if schemaError(err.Error()) {
			msg += " " + describeHint(a)
		}
		return textResult(true, "%s", msg), nil
	}
	ev.Outcome = audit.OutcomeRan
	if res.IsError {
		// The call went through, but the tool reported a failure.
		ev.Outcome, ev.Error = audit.OutcomeFailed, g.red.String(firstText(res, 300))
	}
	g.write(ev)
	out := g.truncate(g.redactResult(res))
	if res.IsError && schemaError(firstText(res, 2000)) {
		out.Content = append(slices.Clone(out.Content), &mcp.TextContent{Text: describeHint(a)})
	}
	return out, nil
}

// schemaError reports whether a downstream failure reads as the arguments not matching the
// tool's input schema, the failure describe_tool would have prevented.
func schemaError(msg string) bool {
	m := strings.ToLower(msg)
	for _, s := range []string{"validat", "additionalproperties", "additional properties", "missing required", "required field",
		"required property", "required argument", "required parameter", "invalid params", "invalid argument", "unexpected argument", "unknown argument"} {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// describeHint points an agent whose arguments were rejected at the tool's schema.
func describeHint(a callArgs) string {
	return fmt.Sprintf("(broker: the arguments may not match the tool's schema; describe_tool connector=%q tool=%q gives it.)", a.Connector, a.Tool)
}

// firstText is the start of a result's first text content, for the audit log.
func firstText(res *mcp.CallToolResult, n int) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			if s := []rune(tc.Text); len(s) > n {
				return string(s[:n]) + "…"
			}
			return tc.Text
		}
	}
	return "the tool returned an error result"
}

// redactResult replaces known secret values in a result. Results are otherwise passed
// through untouched, but a connector echoing its own API key must not hand it to an agent.
func (g *Gateway) redactResult(res *mcp.CallToolResult) *mcp.CallToolResult {
	out := *res
	out.Content = make([]mcp.Content, len(res.Content))
	for i, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			cp := *tc
			cp.Text = g.red.String(tc.Text)
			c = &cp
		}
		out.Content[i] = c
	}
	if res.StructuredContent != nil {
		if b, err := json.Marshal(res.StructuredContent); err == nil {
			if r := g.red.String(string(b)); r != string(b) {
				var v any
				if json.Unmarshal([]byte(r), &v) == nil {
					out.StructuredContent = v
				}
			}
		}
	}
	return &out
}

// forward makes the downstream call, routing elicitations and progress back to the caller.
func (g *Gateway) forward(ctx context.Context, req *mcp.CallToolRequest, id identity.Identity, d *downstream, a callArgs) (*mcp.CallToolResult, error) {
	sess, _ := d.snapshot()
	if sess == nil {
		return nil, errors.New("connector not loaded")
	}
	c := &call{agent: id.Agent, upstream: req.Session, token: req.Params.GetProgressToken()}
	d.begin(c)
	defer d.end(c)
	defer d.touch()

	params := &mcp.CallToolParams{Name: a.Tool}
	if len(a.Arguments) > 0 && string(a.Arguments) != "null" {
		params.Arguments = a.Arguments
	}
	if c.token != nil {
		params.SetProgressToken(progressTokenFor(c))
	}
	return sess.CallTool(withCall(ctx, c), params)
}

// truncate enforces server.max_result_bytes on text content, and says so in the result.
func (g *Gateway) truncate(res *mcp.CallToolResult) *mcp.CallToolResult {
	limit := g.st().cfg.Server.MaxResultBytes
	b, err := json.Marshal(res)
	if err != nil || len(b) <= limit {
		return res
	}
	out := *res
	out.StructuredContent = nil
	out.Content = nil
	budget := limit
	for _, c := range res.Content {
		tc, ok := c.(*mcp.TextContent)
		if !ok {
			continue // binary content cannot be cut meaningfully; drop it
		}
		if budget <= 0 {
			break
		}
		txt := tc.Text
		if len(txt) > budget {
			txt = txt[:budget]
		}
		budget -= len(txt)
		out.Content = append(out.Content, &mcp.TextContent{Text: txt})
	}
	out.Content = append(out.Content, &mcp.TextContent{Text: fmt.Sprintf(
		"[mcp-broker: result was %d bytes and has been truncated to %d (server.max_result_bytes); non-text content dropped]", len(b), limit)})
	return &out
}

// approve gets a decision for an approve-class call. An empty outcome means the call may
// proceed; otherwise outcome is the audit outcome and res says why not (denied, expired, or
// pending with an approval id).
func (g *Gateway) approve(ctx context.Context, req *mcp.CallToolRequest, id identity.Identity, dec policy.Decision, a callArgs, argsHash string) (res *mcp.CallToolResult, ch config.Channel, outcome string) {
	return g.approveFor(ctx, req, id, approvalSpec{
		plan: approval.Plan{
			Kind: "tool.call", Agent: id.Agent, Connector: a.Connector, Tool: a.Tool, Class: string(dec.Class), ArgsHash: argsHash,
			Summary: g.red.String(fmt.Sprintf("%s wants to call %s/%s (%s). %s", id.Agent, a.Connector, a.Tool, dec.Class, argsPreview(a.Arguments))),
			Reason:  dec.Reason,
		},
		approvalID: a.ApprovalID,
		label:      a.Connector + "/" + a.Tool,
		retry:      "call call_tool again with the same arguments",
	})
}

// approvalSpec is one action waiting for a human: a tool call or a profile action.
type approvalSpec struct {
	plan       approval.Plan // Channels is filled in by approveFor
	approvalID string        // from the caller, to spend an earlier approval
	label      string        // what is being approved, for messages
	retry      string        // how to come back once approved
}

// outcomeOf is the audit outcome of an approval that didn't let the action through.
func outcomeOf(s approval.Status) string {
	switch s {
	case approval.Denied:
		return audit.OutcomeDenied
	case approval.Expired:
		return audit.OutcomeTimedOut
	case approval.Pending:
		return audit.OutcomePending
	}
	return audit.OutcomeRefused
}

// approveFor gets a decision for spec through the agent's channels. An empty outcome means
// approved, and the approval has been spent on this exact action; otherwise outcome is the
// audit outcome and res is the answer to return.
func (g *Gateway) approveFor(ctx context.Context, req *mcp.CallToolRequest, id identity.Identity, spec approvalSpec) (res *mcp.CallToolResult, ch config.Channel, outcome string) {
	q := g.opts.Approvals
	p := spec.plan
	binding := approval.Binding{Agent: p.Agent, Connector: p.Connector, Tool: p.Tool, Profile: p.Profile, Action: p.Action, ArgsHash: p.ArgsHash}

	if spec.approvalID != "" {
		err := q.Consume(spec.approvalID, binding)
		e, _ := q.Get(spec.approvalID)
		switch {
		case err == nil:
			return nil, e.Channel, ""
		case errors.Is(err, approval.ErrNotApproved) && e.Status == approval.Pending:
			return g.pending(e, spec.retry), "", audit.OutcomePending
		case e.Status == approval.Denied || e.Status == approval.Expired:
			return textResult(true, "approval %s cannot be used: %v", spec.approvalID, err), e.Channel, outcomeOf(e.Status)
		default:
			return textResult(true, "approval %s cannot be used: %v", spec.approvalID, err), "", audit.OutcomeRefused
		}
	}

	channels := g.st().pol.Channels(id.Agent)
	canElicit := slices.Contains(channels, config.ChanElicitation) && supportsElicitation(req)
	if !canElicit {
		channels = slices.DeleteFunc(slices.Clone(channels), func(c config.Channel) bool { return c == config.ChanElicitation })
	}
	p.Channels = channels
	e := q.Submit(p)
	g.write(audit.Event{Kind: audit.KindPlanCreated, Agent: id.Agent, Connector: p.Connector, Tool: p.Tool, Profile: p.Profile, Action: p.Action, PlanID: e.ID, ArgsHash: p.ArgsHash, Detail: p.Reason})
	if e.Status == approval.Denied {
		return textResult(true, "denied: %s has no approval channel", id.Agent), "", audit.OutcomeRefused
	}

	wait := g.approvalWait()
	if canElicit {
		wait = g.st().cfg.Approvals.Timeout
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	if canElicit {
		go g.elicitApproval(wctx, req.Session, e, len(channels) > 1)
	}
	stopKeepalive := g.keepalive(wctx, req)
	got, _ := q.Wait(wctx, e.ID)
	stopKeepalive()

	g.write(audit.Event{Kind: audit.KindApproval, Agent: id.Agent, Connector: p.Connector, Tool: p.Tool, Profile: p.Profile, Action: p.Action, PlanID: e.ID, Decision: string(got.Status), Channel: string(got.Channel)})
	switch got.Status {
	case approval.Approved:
		if err := q.Consume(e.ID, binding); err != nil {
			return textResult(true, "approval %s: %v", e.ID, err), got.Channel, audit.OutcomeRefused
		}
		return nil, got.Channel, ""
	case approval.Pending:
		if ctx.Err() != nil {
			// The agent gave up (its request was cancelled), not the wait running out.
			return g.pending(got, spec.retry), "", audit.OutcomeCancelled
		}
		return g.pending(got, spec.retry), "", audit.OutcomePending
	default:
		return textResult(true, "%s was not approved (%s)", spec.label, got.Status), got.Channel, outcomeOf(got.Status)
	}
}

// approvalWait is how long one call blocks for an answer from outside the client.
func (g *Gateway) approvalWait() time.Duration {
	if g.opts.ApprovalWait > 0 {
		return g.opts.ApprovalWait
	}
	if w := g.st().cfg.Approvals.Wait; w > 0 {
		return w
	}
	return config.DefaultApprovalWait
}

// checkEvery is how long agents are told to wait between checks on a pending approval.
func (g *Gateway) checkEvery() time.Duration {
	if c := g.st().cfg.Approvals.CheckEvery; c > 0 {
		return c
	}
	return config.DefaultCheckEvery
}

// pendingTiming is what agents are told about when to look again: seconds until the plan
// expires and until the next check, never past the expiry.
func (g *Gateway) pendingTiming(e approval.Entry) (expiresIn, retryAfter int) {
	left := time.Until(e.Expires)
	expiresIn = max(int(left.Round(time.Second)/time.Second), 0)
	retryAfter = min(max(int(g.checkEvery()/time.Second), 1), max(expiresIn, 1))
	return expiresIn, retryAfter
}

func (g *Gateway) pending(e approval.Entry, retry string) *mcp.CallToolResult {
	where := "the mcp-broker tray or dashboard"
	if g.opts.DashboardURL != "" {
		where += " (" + g.opts.DashboardURL + ")"
	}
	expiresIn, retryAfter := g.pendingTiming(e)
	wait := int(g.approvalWait() / time.Second)
	res := textResult(false, "pending_approval: approval %s is waiting for the user in %s; it expires in %ds (at %s). "+
		"Tell the user, then don't retry at once: call approval_status with approval_id %q and wait %d, which returns when the user answers; "+
		"if it is still pending, check again after %ds. Once approved, %s and approval_id %q.",
		e.ID, where, expiresIn, e.Expires.Local().Format(time.Kitchen), e.ID, wait, retryAfter, retry, e.ID)
	res.StructuredContent = map[string]any{"status": "pending_approval", "approval_id": e.ID, "expires": e.Expires,
		"expires_in_seconds": expiresIn, "retry_after_seconds": retryAfter, "wait_seconds": wait}
	return res
}

// elicitApproval asks the user in the agent's own client. Its answer is just one channel:
// a tray or dashboard decision may land first, in which case this prompt is cancelled.
//
// Only an answer to the form decides. A decline or cancel is how a client says it did
// not ask (the VS Code extension declines within milliseconds without showing anything),
// so while other channels are open it leaves the decision to them; with none, it denies
// rather than leaving the call to time out.
func (g *Gateway) elicitApproval(ctx context.Context, ss *mcp.ServerSession, e approval.Entry, others bool) {
	res, err := ss.Elicit(ctx, &mcp.ElicitParams{
		Mode:    "form",
		Message: elicitMessage(e),
		RequestedSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"approve": map[string]any{"type": "boolean", "title": "Approve this call?", "default": false},
			},
			"required": []string{"approve"},
		},
	})
	if err != nil {
		return // cancelled because another channel decided, or the client failed; the queue's timeout covers it
	}
	if res.Action != "accept" {
		g.write(audit.Event{Kind: audit.KindApproval, Agent: e.Agent, Connector: e.Connector, Tool: e.Tool, PlanID: e.ID,
			Channel: string(config.ChanElicitation), Detail: "client answered " + res.Action + " without a decision"})
		if others {
			return
		}
	}
	approve := res.Action == "accept" && res.Content["approve"] == true
	g.opts.Approvals.Decide(e.ID, approve, config.ChanElicitation)
}

func elicitMessage(e approval.Entry) string {
	m := "mcp-broker approval: " + e.Summary
	if e.Detail != "" {
		m += "\n\n" + e.Detail
	}
	return m
}

// keepalive sends progress while a call waits for approval, so clients that reset their
// tool timeout on progress do not give up.
func (g *Gateway) keepalive(ctx context.Context, req *mcp.CallToolRequest) func() {
	token := req.Params.GetProgressToken()
	if token == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		n := 0.0
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				n++
				req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{ProgressToken: token, Progress: n, Message: "waiting for approval"})
			}
		}
	}()
	return cancel
}

func supportsElicitation(req *mcp.CallToolRequest) bool {
	caps := req.ClientCapabilities()
	return caps != nil && caps.Elicitation != nil
}

// argsPreview is a short, single-line view of arguments for approval prompts. It is
// redacted by the caller; long values are cut so a prompt stays readable.
func argsPreview(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" || string(raw) == "{}" {
		return "No arguments."
	}
	s := strings.Join(strings.Fields(string(raw)), " ")
	const max = 300
	if len(s) > max {
		s = s[:max] + "…"
	}
	return "Arguments: " + s
}

func (g *Gateway) write(e audit.Event) {
	if g.opts.Audit == nil {
		return
	}
	if err := g.opts.Audit.Write(e); err != nil {
		g.log.Error("audit write failed", "err", err)
	}
}

func (g *Gateway) auditElicit(connector, agent, outcome string, err error) {
	e := audit.Event{Kind: audit.KindApproval, Agent: agent, Connector: connector, Detail: "downstream elicitation " + outcome}
	if err != nil {
		e.Error = err.Error()
	}
	g.write(e)
}
