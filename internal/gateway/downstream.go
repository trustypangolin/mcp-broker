package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/proc"
	"github.com/trustypangolin/mcp-broker/internal/version"
)

// downstream is one running connection to a connector: a spawned process speaking stdio, or
// a remote Streamable HTTP server.
type downstream struct {
	g    *Gateway
	key  string // name, or name@agent for per_agent connectors
	name string
	cn   config.Connector

	mu       sync.Mutex // guards everything below
	session  *mcp.ClientSession
	proc     *proc.Process
	exited   chan struct{}
	tools    map[string]*mcp.Tool
	order    []string
	lastUsed time.Time
	stderr   *ring

	callsMu sync.Mutex
	calls   map[*call]struct{} // in flight, for routing server-initiated elicitations
}

// call is one in-flight downstream tool call made on behalf of an upstream request.
type call struct {
	agent    string
	upstream *mcp.ServerSession
	token    any // upstream progress token, if any
}

type callKey struct{}

func withCall(ctx context.Context, c *call) context.Context {
	return context.WithValue(ctx, callKey{}, c)
}

func callFrom(ctx context.Context) *call {
	c, _ := ctx.Value(callKey{}).(*call)
	return c
}

// connectTimeout bounds process start plus MCP handshake; uv resolving an environment on
// first run is the slow case.
const connectTimeout = 2 * time.Minute

// ensure connects if not connected. The caller must not hold d.mu.
func (d *downstream) ensure(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.session != nil {
		d.lastUsed = time.Now()
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	if err := d.connectLocked(ctx); err != nil {
		d.closeLocked()
		return err
	}
	d.lastUsed = time.Now()
	return nil
}

func (d *downstream) connectLocked(ctx context.Context) error {
	r, err := d.cn.Resolve(d.g.red)
	if err != nil {
		return err
	}

	var transport mcp.Transport
	if d.cn.Remote() {
		transport = &mcp.StreamableClientTransport{
			Endpoint:   r.URL,
			HTTPClient: &http.Client{Transport: headerTransport{base: http.DefaultTransport, headers: r.Headers}},
		}
	} else {
		cmd := exec.Command(r.Command, r.Args...)
		cmd.Dir = r.Cwd
		cmd.Env = os.Environ()
		for k, v := range r.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		d.stderr = newRing(500, d.g.logFile(d.key))
		cmd.Stderr = d.stderr
		p, err := proc.Start(cmd, proc.Options{})
		if err != nil {
			return fmt.Errorf("starting %s: %w", d.name, err)
		}
		d.proc = p
		d.exited = make(chan struct{})
		go func(exited chan struct{}) {
			cmd.Wait()
			close(exited)
		}(d.exited)
		transport = &mcp.IOTransport{Reader: stdout, Writer: stdin}
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "mcp-broker", Version: version.Version}, &mcp.ClientOptions{
		Capabilities:                &mcp.ClientCapabilities{}, // no roots: the broker has none to offer
		ElicitationHandler:          d.onElicit,
		ToolListChangedHandler:      func(context.Context, *mcp.ToolListChangedRequest) { go d.refreshTools() },
		ProgressNotificationHandler: d.onProgress,
	})
	sess, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w%s", d.name, err, d.stderrTail())
	}
	d.session = sess
	tools, err := listTools(ctx, sess)
	if err != nil {
		return fmt.Errorf("listing tools of %s: %w", d.name, err)
	}
	d.setTools(tools)
	return nil
}

func listTools(ctx context.Context, sess *mcp.ClientSession) ([]*mcp.Tool, error) {
	var out []*mcp.Tool
	params := &mcp.ListToolsParams{}
	for {
		res, err := sess.ListTools(ctx, params)
		if err != nil {
			return nil, err
		}
		out = append(out, res.Tools...)
		if res.NextCursor == "" {
			return out, nil
		}
		params.Cursor = res.NextCursor
	}
}

func (d *downstream) setTools(tools []*mcp.Tool) {
	d.tools = make(map[string]*mcp.Tool, len(tools))
	d.order = d.order[:0]
	for _, t := range tools {
		d.tools[t.Name] = t
		d.order = append(d.order, t.Name)
	}
	sort.Strings(d.order)
}

func (d *downstream) refreshTools() {
	d.mu.Lock()
	sess := d.session
	d.mu.Unlock()
	if sess == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tools, err := listTools(ctx, sess)
	if err != nil {
		d.g.log.Warn("refreshing tools", "connector", d.key, "err", d.g.red.String(err.Error()))
		return
	}
	d.mu.Lock()
	if d.session == sess {
		d.setTools(tools)
	}
	d.mu.Unlock()
}

// snapshot returns the session and the tools in name order.
func (d *downstream) snapshot() (*mcp.ClientSession, []*mcp.Tool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tools := make([]*mcp.Tool, 0, len(d.order))
	for _, n := range d.order {
		tools = append(tools, d.tools[n])
	}
	return d.session, tools
}

func (d *downstream) tool(name string) *mcp.Tool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.tools[name]
}

func (d *downstream) loaded() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.session != nil
}

// close disconnects and, for local connectors, makes sure the process tree is gone.
func (d *downstream) close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closeLocked()
}

// gracefulExit is how long a local server gets to exit after stdin closes.
const gracefulExit = 5 * time.Second

func (d *downstream) closeLocked() {
	if d.session != nil {
		d.session.Close()
		d.session = nil
	}
	if d.proc != nil {
		select {
		case <-d.exited:
		case <-time.After(gracefulExit):
		}
		d.proc.Kill() // always: the root exiting does not mean its children did
		d.proc = nil
	}
	if d.stderr != nil {
		d.stderr.Close()
	}
	d.tools, d.order = nil, nil
}

// idleSince reports when the connector was last used, if loaded.
func (d *downstream) idleSince() (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lastUsed, d.session != nil
}

func (d *downstream) touch() {
	d.mu.Lock()
	d.lastUsed = time.Now()
	d.mu.Unlock()
}

func (d *downstream) stderrTail() string {
	if d.stderr == nil {
		return ""
	}
	if tail := d.stderr.Tail(10); len(tail) > 0 {
		return "\nlast stderr lines:\n" + d.g.red.String(joinLines(tail))
	}
	return ""
}

func (d *downstream) begin(c *call) {
	d.callsMu.Lock()
	if d.calls == nil {
		d.calls = map[*call]struct{}{}
	}
	d.calls[c] = struct{}{}
	d.callsMu.Unlock()
}

func (d *downstream) end(c *call) {
	d.callsMu.Lock()
	delete(d.calls, c)
	d.callsMu.Unlock()
}

// errAmbiguous: an old-protocol elicitation arrived while several calls were in flight on a
// shared connector, so the broker cannot tell whose user should see it.
var errAmbiguous = errors.New("elicitation cannot be attributed to a single caller")

// onElicit forwards a downstream server's elicitation to the agent whose call caused it.
// Newer servers send it inside the call (so ctx carries the call); older ones send a
// standalone request, which is attributed to the only call in flight, or declined.
func (d *downstream) onElicit(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
	c := callFrom(ctx)
	if c == nil {
		d.callsMu.Lock()
		if len(d.calls) == 1 {
			for only := range d.calls {
				c = only
			}
		}
		d.callsMu.Unlock()
	}
	if c == nil {
		d.g.auditElicit(d.key, "", "declined", errAmbiguous)
		return &mcp.ElicitResult{Action: "decline"}, nil
	}
	if c.upstream == nil {
		d.g.auditElicit(d.key, c.agent, "declined", errors.New("caller session gone"))
		return &mcp.ElicitResult{Action: "decline"}, nil
	}
	res, err := c.upstream.Elicit(ctx, req.Params)
	if err != nil {
		// The client may not support elicitation at all; declining lets the server decide.
		d.g.auditElicit(d.key, c.agent, "declined", err)
		return &mcp.ElicitResult{Action: "decline"}, nil
	}
	d.g.auditElicit(d.key, c.agent, res.Action, nil)
	return res, nil
}

// onProgress relays downstream progress to the upstream caller under the caller's token.
func (d *downstream) onProgress(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
	d.callsMu.Lock()
	var target *call
	for c := range d.calls {
		if c.token != nil && fmt.Sprint(progressTokenFor(c)) == fmt.Sprint(req.Params.ProgressToken) {
			target = c
			break
		}
	}
	d.callsMu.Unlock()
	if target == nil || target.upstream == nil {
		return
	}
	p := *req.Params
	p.ProgressToken = target.token
	target.upstream.NotifyProgress(ctx, &p)
}

// progressTokenFor is the token the broker uses downstream for a call: unique per call, so
// progress from concurrent calls on a shared connector cannot cross.
func progressTokenFor(c *call) string { return fmt.Sprintf("mcpb-%p", c) }

// headerTransport adds fixed headers (the connector's auth) to every request.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range t.headers {
		r.Header.Set(k, v)
	}
	return t.base.RoundTrip(r)
}

func (g *Gateway) logFile(key string) io.WriteCloser {
	if g.opts.LogDir == "" {
		return nil
	}
	if err := os.MkdirAll(g.opts.LogDir, 0o700); err != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(g.opts.LogDir, key+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil
	}
	return f
}
