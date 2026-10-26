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
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trustypangolin/mcp-broker/internal/approval"
	"github.com/trustypangolin/mcp-broker/internal/audit"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/identity"
	"github.com/trustypangolin/mcp-broker/internal/policy"
	"github.com/trustypangolin/mcp-broker/internal/redact"
	"github.com/trustypangolin/mcp-broker/internal/version"
)

// Options wires a Gateway to the rest of the broker.
type Options struct {
	Config    *config.Config
	Policy    *policy.Engine
	Approvals *approval.Queue
	Audit     *audit.Log
	Redactor  *redact.Redactor
	Logger    *slog.Logger
	// DashboardURL is shown to agents whose approval must happen out of band.
	DashboardURL string
	// LogDir receives one stderr log per local connector; empty disables.
	LogDir string
	// Identity extracts the caller from a request. Defaults to IdentityFromRequest.
	Identity func(*mcp.CallToolRequest) identity.Identity
	// ApprovalWait is how long call_tool blocks for an out-of-band approval before
	// returning pending_approval. Defaults to 25s, under typical client tool timeouts.
	ApprovalWait time.Duration
}

// Gateway serves the meta-tools.
type Gateway struct {
	opts Options
	cfg  *config.Config
	red  *redact.Redactor
	log  *slog.Logger

	mu    sync.Mutex
	conns map[string]*downstream

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
	if opts.ApprovalWait == 0 {
		opts.ApprovalWait = 25 * time.Second
	}
	g := &Gateway{opts: opts, cfg: opts.Config, red: opts.Redactor, log: opts.Logger, conns: map[string]*downstream{}, stop: make(chan struct{})}
	g.done.Add(1)
	go g.idleLoop()
	return g
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
	cn, ok := g.cfg.Connectors[name]
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

// Server builds the MCP server agents connect to.
func (g *Gateway) Server() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "mcp-broker", Version: version.Version}, &mcp.ServerOptions{
		Instructions: "This is mcp-broker. Call list_connectors to see what is available, load_connector to get a " +
			"connector's tools, then call_tool to use one. Some calls need the user's approval.",
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		// Sessions are required (server-initiated elicitation for approvals and passthrough),
		// and the SDK serves the 2026-07-28 protocol only statelessly, so pin the
		// session-based versions explicitly rather than rely on the HTTP layer to refuse.
		SupportedProtocolVersions: []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"},
	})
	s.AddTool(&mcp.Tool{
		Name:        "list_connectors",
		Description: "List available connectors (downstream MCP servers): name, description, whether loaded.",
		InputSchema: schema(nil),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, g.listConnectors)
	s.AddTool(&mcp.Tool{
		Name:        "load_connector",
		Description: "Start a connector if needed and return its tools with input schemas.",
		InputSchema: schema(map[string]any{"name": str("Connector name from list_connectors")}, "name"),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, g.loadConnector)
	s.AddTool(&mcp.Tool{
		Name:        "call_tool",
		Description: "Call a tool on a connector. May wait for the user's approval. If the result says approval is pending, call again with the same arguments plus approval_id once approved.",
		InputSchema: schema(map[string]any{
			"connector":   str("Connector name"),
			"tool":        str("Tool name from load_connector"),
			"arguments":   map[string]any{"type": "object", "description": "Tool arguments"},
			"approval_id": str("Approval id from an earlier pending_approval result"),
		}, "connector", "tool"),
	}, g.callTool)
	s.AddTool(&mcp.Tool{
		Name:        "unload_connector",
		Description: "Stop a connector and drop its cached tools.",
		InputSchema: schema(map[string]any{"name": str("Connector name")}, "name"),
	}, g.unloadConnector)
	s.AddTool(&mcp.Tool{
		Name:        "approval_status",
		Description: "Check a pending approval by id.",
		InputSchema: schema(map[string]any{"approval_id": str("Approval id")}, "approval_id"),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, g.approvalStatus)
	return s
}

func schema(props map[string]any, required ...string) map[string]any {
	if props == nil {
		props = map[string]any{}
	}
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
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
	Name        string `json:"name"`
	Description string `json:"description"`
	Loaded      bool   `json:"loaded"`
}

func (g *Gateway) listConnectors(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	id := g.opts.Identity(req)
	out := []connectorInfo{}
	for _, name := range g.opts.Policy.VisibleConnectors(id.Agent) {
		d, _ := g.conn(name, id.Agent)
		out = append(out, connectorInfo{Name: name, Description: g.cfg.Connectors[name].Description, Loaded: d.loaded()})
	}
	return jsonResult(map[string]any{"connectors": out}), nil
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
	if !g.opts.Policy.Visible(id.Agent, name) {
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
	var a struct{ Name string }
	if err := decodeArgs(req, &a); err != nil {
		return textResult(true, "%v", err), nil
	}
	id := g.opts.Identity(req)
	d, bad := g.loadable(ctx, id, a.Name)
	if bad != nil {
		return bad, nil
	}
	_, tools := d.snapshot()
	out := []toolInfo{}
	for _, t := range tools {
		dec := g.opts.Policy.Tool(id.Agent, a.Name, t.Name, annotations(t))
		if dec.Action == config.Deny {
			continue // denied tools are hidden, not advertised
		}
		out = append(out, toolInfo{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema, Class: string(dec.Class), Approval: dec.Action == config.Approve})
	}
	return jsonResult(map[string]any{"connector": a.Name, "tools": out}), nil
}

func (g *Gateway) unloadConnector(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct{ Name string }
	if err := decodeArgs(req, &a); err != nil {
		return textResult(true, "%v", err), nil
	}
	id := g.opts.Identity(req)
	if !g.opts.Policy.Visible(id.Agent, a.Name) {
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

func (g *Gateway) approvalStatus(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		ApprovalID string `json:"approval_id"`
	}
	if err := decodeArgs(req, &a); err != nil {
		return textResult(true, "%v", err), nil
	}
	id := g.opts.Identity(req)
	e, ok := g.opts.Approvals.Get(a.ApprovalID)
	if !ok || e.Agent != id.Agent {
		return textResult(true, "unknown approval id %q", a.ApprovalID), nil
	}
	return jsonResult(map[string]any{"approval_id": e.ID, "status": e.Status, "decided_by": e.Channel, "expires": e.Expires}), nil
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
		dec = g.opts.Policy.Tool(id.Agent, a.Connector, a.Tool, annotations(t))
	}
	if t == nil || dec.Action == config.Deny {
		ev.Decision, ev.Detail = string(config.Deny), "unknown or denied tool"
		if t != nil {
			ev.Class, ev.Detail = string(dec.Class), dec.Reason
		}
		g.write(ev)
		return textResult(true, "unknown tool %q on connector %q", a.Tool, a.Connector), nil
	}
	ev.Class, ev.Decision = string(dec.Class), string(dec.Action)

	if dec.Action == config.Approve {
		res, ch, ok := g.approve(ctx, req, id, dec, a, ev.ArgsHash)
		ev.Channel = string(ch)
		if !ok {
			ev.Detail = "not approved"
			ev.DurationMS = time.Since(start).Milliseconds()
			g.write(ev)
			return res, nil
		}
		ev.Detail = "approved"
	}

	res, err := g.forward(ctx, req, id, d, a)
	ev.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		ev.Error = g.red.String(err.Error())
		g.write(ev)
		return textResult(true, "%s/%s failed: %s", a.Connector, a.Tool, ev.Error), nil
	}
	g.write(ev)
	return g.truncate(g.redactResult(res)), nil
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
	limit := g.cfg.Server.MaxResultBytes
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

// approve gets a decision for an approve-class call. It returns ok=true once the call may
// proceed; otherwise res says why not (denied, expired, or pending with an approval id).
func (g *Gateway) approve(ctx context.Context, req *mcp.CallToolRequest, id identity.Identity, dec policy.Decision, a callArgs, argsHash string) (res *mcp.CallToolResult, ch config.Channel, ok bool) {
	q := g.opts.Approvals
	binding := approval.Binding{Agent: id.Agent, Connector: a.Connector, Tool: a.Tool, ArgsHash: argsHash}

	if a.ApprovalID != "" {
		err := q.Consume(a.ApprovalID, binding)
		e, _ := q.Get(a.ApprovalID)
		switch {
		case err == nil:
			return nil, e.Channel, true
		case errors.Is(err, approval.ErrNotApproved) && e.Status == approval.Pending:
			return g.pending(e), "", false
		default:
			return textResult(true, "approval %s cannot be used: %v", a.ApprovalID, err), "", false
		}
	}

	channels := g.opts.Policy.Channels(id.Agent)
	canElicit := slices.Contains(channels, config.ChanElicitation) && supportsElicitation(req)
	if !canElicit {
		channels = slices.DeleteFunc(slices.Clone(channels), func(c config.Channel) bool { return c == config.ChanElicitation })
	}
	summary := g.red.String(fmt.Sprintf("%s wants to call %s/%s (%s). %s", id.Agent, a.Connector, a.Tool, dec.Class, argsPreview(a.Arguments)))
	e := q.Submit(approval.Plan{
		Kind: "tool.call", Agent: id.Agent, Connector: a.Connector, Tool: a.Tool, Class: string(dec.Class),
		ArgsHash: argsHash, Summary: summary, Reason: dec.Reason, Channels: channels,
	})
	g.write(audit.Event{Kind: audit.KindPlanCreated, Agent: id.Agent, Connector: a.Connector, Tool: a.Tool, PlanID: e.ID, ArgsHash: argsHash, Detail: dec.Reason})
	if e.Status == approval.Denied {
		return textResult(true, "denied: %s has no approval channel", id.Agent), "", false
	}

	wait := g.opts.ApprovalWait
	if canElicit {
		wait = g.cfg.Approvals.Timeout
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()

	if canElicit {
		go g.elicitApproval(wctx, req.Session, e)
	}
	stopKeepalive := g.keepalive(wctx, req)
	got, _ := q.Wait(wctx, e.ID)
	stopKeepalive()

	g.write(audit.Event{Kind: audit.KindApproval, Agent: id.Agent, Connector: a.Connector, Tool: a.Tool, PlanID: e.ID, Decision: string(got.Status), Channel: string(got.Channel)})
	switch got.Status {
	case approval.Approved:
		if err := q.Consume(e.ID, binding); err != nil {
			return textResult(true, "approval %s: %v", e.ID, err), got.Channel, false
		}
		return nil, got.Channel, true
	case approval.Pending:
		return g.pending(got), "", false
	default:
		return textResult(true, "%s/%s was not approved (%s)", a.Connector, a.Tool, got.Status), got.Channel, false
	}
}

func (g *Gateway) pending(e approval.Entry) *mcp.CallToolResult {
	where := "the mcp-broker tray or dashboard"
	if g.opts.DashboardURL != "" {
		where += " (" + g.opts.DashboardURL + ")"
	}
	res := textResult(false, "pending_approval: approval %s is waiting for the user in %s. It expires at %s. "+
		"Once approved, call call_tool again with the same arguments and approval_id %q.",
		e.ID, where, e.Expires.Local().Format(time.Kitchen), e.ID)
	res.StructuredContent = map[string]any{"status": "pending_approval", "approval_id": e.ID, "expires": e.Expires}
	return res
}

// elicitApproval asks the user in the agent's own client. Its answer is just one channel:
// a tray or dashboard decision may land first, in which case this prompt is cancelled.
func (g *Gateway) elicitApproval(ctx context.Context, ss *mcp.ServerSession, e approval.Entry) {
	res, err := ss.Elicit(ctx, &mcp.ElicitParams{
		Mode:    "form",
		Message: "mcp-broker approval: " + e.Summary,
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
	approve := res.Action == "accept" && res.Content["approve"] == true
	g.opts.Approvals.Decide(e.ID, approve, config.ChanElicitation)
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
