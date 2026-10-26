package gateway

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trustypangolin/mcp-broker/internal/approval"
	"github.com/trustypangolin/mcp-broker/internal/audit"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/identity"
	"github.com/trustypangolin/mcp-broker/internal/policy"
	"github.com/trustypangolin/mcp-broker/internal/redact"
	"github.com/trustypangolin/mcp-broker/internal/testutil/alive"
	"github.com/trustypangolin/mcp-broker/internal/testutil/fakemcp"
)

func TestMain(m *testing.M) {
	fakemcp.MaybeRun()
	os.Exit(m.Run())
}

const testConfig = `
agents:
  owner: {role: owner, approvals: [elicitation, tray, dashboard]}
  coder: {role: coder, approvals: [tray, dashboard]}
  outsider: {role: outsider}
roles:
  owner:
    connectors: ["*"]
    classes: {read: allow, write: allow, destructive: approve, external: approve}
  coder:
    connectors: [fake]
    classes: {read: allow, write: allow, destructive: approve, external: deny}
  outsider:
    connectors: []
connectors:
  fake:
    description: Fake downstream
    command: placeholder
`

type harness struct {
	t      *testing.T
	gw     *Gateway
	q      *approval.Queue
	cfg    *config.Config
	audit  string
	agent  atomic.Value // string
	elicit func(*mcp.ElicitParams) *mcp.ElicitResult

	progress atomic.Int32
	session  *mcp.ClientSession
}

func newHarness(t *testing.T, mode string, tweak func(*config.Config)) *harness {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(testConfig), dir)
	if err != nil {
		t.Fatal(err)
	}
	cn := cfg.Connectors["fake"]
	cn.Command = os.Args[0]
	cn.Env = fakemcp.Env(mode)
	cfg.Connectors["fake"] = cn
	if tweak != nil {
		tweak(cfg)
	}

	h := &harness{t: t, cfg: cfg, audit: filepath.Join(dir, "audit.jsonl")}
	h.agent.Store("owner")
	red := redact.New()
	aud, err := audit.Open(h.audit, red)
	if err != nil {
		t.Fatal(err)
	}
	h.q = approval.NewQueue(5 * time.Second)
	h.gw = New(Options{
		Config: cfg, Policy: policy.New(cfg), Approvals: h.q, Audit: aud, Redactor: red,
		LogDir:       filepath.Join(dir, "logs"),
		ApprovalWait: 300 * time.Millisecond,
		Identity: func(*mcp.CallToolRequest) identity.Identity {
			a := h.agent.Load().(string)
			return identity.Identity{Agent: a, Role: cfg.Agents[a].Role}
		},
	})

	srvT, cliT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := h.gw.Server().Connect(ctx, srvT, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent"}, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			if h.elicit == nil {
				return &mcp.ElicitResult{Action: "decline"}, nil
			}
			return h.elicit(req.Params), nil
		},
		ProgressNotificationHandler: func(context.Context, *mcp.ProgressNotificationClientRequest) { h.progress.Add(1) },
	})
	h.session, err = client.Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		h.session.Close()
		h.gw.Close()
		aud.Close()
	})
	return h
}

func (h *harness) as(agent string) *harness { h.agent.Store(agent); return h }

func (h *harness) call(tool string, args map[string]any) (string, bool) {
	h.t.Helper()
	return h.callWith(&mcp.CallToolParams{Name: tool, Arguments: args})
}

func (h *harness) callWith(p *mcp.CallToolParams) (string, bool) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	res, err := h.session.CallTool(ctx, p)
	if err != nil {
		h.t.Fatalf("%s: %v", p.Name, err)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String(), res.IsError
}

func (h *harness) callFake(tool string, args map[string]any) (string, bool) {
	h.t.Helper()
	return h.call("call_tool", map[string]any{"connector": "fake", "tool": tool, "arguments": args})
}

func TestInitialToolListIsMetaToolsOnly(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, nil)
	res, err := h.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	if got := strings.Join(names, ","); got != "approval_status,call_tool,list_connectors,load_connector,unload_connector" {
		t.Errorf("tools = %s", got)
	}
}

func TestVisibility(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, nil)
	out, _ := h.call("list_connectors", nil)
	if !strings.Contains(out, `"fake"`) || !strings.Contains(out, `"loaded": false`) {
		t.Errorf("owner list = %s", out)
	}

	out, _ = h.as("outsider").call("list_connectors", nil)
	if strings.Contains(out, "fake") {
		t.Errorf("outsider sees fake: %s", out)
	}
	out, isErr := h.call("load_connector", map[string]any{"name": "fake"})
	if !isErr || !strings.Contains(out, `unknown connector "fake"`) {
		t.Errorf("outsider load = %s", out)
	}
	out, isErr = h.callFake("get_echo", map[string]any{"text": "hi"})
	if !isErr || !strings.Contains(out, "unknown connector") {
		t.Errorf("outsider call = %s", out)
	}
}

func TestLoadHidesDeniedTools(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, nil)
	out, isErr := h.as("coder").call("load_connector", map[string]any{"name": "fake"})
	if isErr {
		t.Fatal(out)
	}
	var got struct {
		Tools []toolInfo `json:"tools"`
	}
	json.Unmarshal([]byte(out), &got)
	byName := map[string]toolInfo{}
	for _, ti := range got.Tools {
		byName[ti.Name] = ti
	}
	if _, ok := byName["send_message"]; ok {
		t.Error("coder sees send_message (external: deny)")
	}
	if ti := byName["delete_thing"]; ti.Class != "destructive" || !ti.Approval {
		t.Errorf("delete_thing = %+v", ti)
	}
	if ti := byName["get_echo"]; ti.Class != "read" || ti.Approval || ti.InputSchema == nil {
		t.Errorf("get_echo = %+v", ti)
	}

	out, isErr = h.callFake("send_message", map[string]any{"text": "x"})
	if !isErr || !strings.Contains(out, "unknown tool") {
		t.Errorf("coder send_message = %s", out)
	}
}

func TestAllowedCall(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, nil)
	out, isErr := h.callFake("get_echo", map[string]any{"text": "hello"})
	if isErr || out != "echo: hello" {
		t.Fatalf("got %q err=%v", out, isErr)
	}
	out, _ = h.call("list_connectors", nil)
	if !strings.Contains(out, `"loaded": true`) {
		t.Errorf("not loaded after call: %s", out)
	}
}

func TestApprovalByElicitation(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, nil)
	var prompt string
	h.elicit = func(p *mcp.ElicitParams) *mcp.ElicitResult {
		prompt = p.Message
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}
	}
	out, isErr := h.callFake("delete_thing", map[string]any{"text": "file.txt"})
	if isErr || out != "deleted file.txt" {
		t.Fatalf("got %q err=%v", out, isErr)
	}
	if !strings.Contains(prompt, "fake/delete_thing") || !strings.Contains(prompt, "file.txt") {
		t.Errorf("prompt = %q", prompt)
	}

	h.elicit = func(*mcp.ElicitParams) *mcp.ElicitResult {
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": false}}
	}
	out, isErr = h.callFake("delete_thing", map[string]any{"text": "file.txt"})
	if !isErr || !strings.Contains(out, "not approved (denied)") {
		t.Fatalf("declined call = %q", out)
	}
}

func TestApprovalByDashboard(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, func(c *config.Config) {})
	h.gw.opts.ApprovalWait = 10 * time.Second
	h.elicit = func(*mcp.ElicitParams) *mcp.ElicitResult {
		t.Error("coder must never be asked by elicitation")
		return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"approve": true}}
	}
	go func() {
		for i := 0; i < 200; i++ {
			for _, e := range h.q.List(true) {
				h.q.Decide(e.ID, true, config.ChanDashboard)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	out, isErr := h.as("coder").callFake("delete_thing", map[string]any{"text": "a"})
	if isErr || out != "deleted a" {
		t.Fatalf("got %q err=%v", out, isErr)
	}
}

func TestPendingApprovalThenRetry(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, nil)
	h.as("coder")
	args := map[string]any{"text": "b"}
	out, isErr := h.callFake("delete_thing", args)
	if isErr || !strings.Contains(out, "pending_approval") {
		t.Fatalf("expected pending, got %q", out)
	}
	pending := h.q.List(true)
	if len(pending) != 1 {
		t.Fatalf("pending = %d", len(pending))
	}
	id := pending[0].ID
	if !strings.Contains(out, id) {
		t.Fatalf("result lacks approval id: %q", out)
	}

	retry := map[string]any{"connector": "fake", "tool": "delete_thing", "arguments": args, "approval_id": id}
	out, _ = h.call("call_tool", retry)
	if !strings.Contains(out, "pending_approval") {
		t.Fatalf("retry before decision = %q", out)
	}
	status, _ := h.call("approval_status", map[string]any{"approval_id": id})
	if !strings.Contains(status, `"pending"`) {
		t.Errorf("status = %s", status)
	}

	h.q.Decide(id, true, config.ChanTray)

	// The approval is bound to these exact arguments.
	other := map[string]any{"connector": "fake", "tool": "delete_thing", "arguments": map[string]any{"text": "c"}, "approval_id": id}
	if out, isErr := h.call("call_tool", other); !isErr || !strings.Contains(out, "does not match") {
		t.Fatalf("approval spent on other args: %q", out)
	}
	if out, isErr := h.call("call_tool", retry); isErr || out != "deleted b" {
		t.Fatalf("approved retry = %q", out)
	}
	if out, isErr := h.call("call_tool", retry); !isErr || !strings.Contains(out, "already used") {
		t.Fatalf("approval reused: %q", out)
	}
	// Another agent cannot even see the approval.
	if out, isErr := h.as("owner").call("approval_status", map[string]any{"approval_id": id}); !isErr {
		t.Errorf("owner read coder's approval: %s", out)
	}
}

func TestDownstreamElicitationPassthrough(t *testing.T) {
	for _, mode := range []string{fakemcp.ModeCurrent, fakemcp.ModeLegacy} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, mode, nil)
			var asked atomic.Int32
			h.elicit = func(p *mcp.ElicitParams) *mcp.ElicitResult {
				asked.Add(1)
				if p.Message != "Pick a colour" {
					t.Errorf("message = %q", p.Message)
				}
				return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"colour": "teal"}}
			}
			out, isErr := h.callFake("ask_colour", nil)
			if isErr || out != "colour: teal" {
				t.Fatalf("got %q err=%v", out, isErr)
			}
			if asked.Load() != 1 {
				t.Errorf("asked %d times", asked.Load())
			}
		})
	}
}

func TestProgressPassthrough(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, nil)
	p := &mcp.CallToolParams{Name: "call_tool", Arguments: map[string]any{"connector": "fake", "tool": "get_progress"}}
	p.SetProgressToken("tok-1")
	if out, isErr := h.callWith(p); isErr || out != "done" {
		t.Fatalf("got %q", out)
	}
	deadline := time.Now().Add(2 * time.Second)
	for h.progress.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := h.progress.Load(); n != 3 {
		t.Errorf("progress notifications = %d, want 3", n)
	}
}

func TestSecretsNeverReachAgentOrAudit(t *testing.T) {
	const secret = "sk-live-0123456789abcdef"
	h := newHarness(t, fakemcp.ModeCurrent, func(c *config.Config) {
		envFile := filepath.Join(c.Dir, ".env")
		os.WriteFile(envFile, []byte("FAKE_API_KEY="+secret+"\n"), 0o600)
		cn := c.Connectors["fake"]
		cn.EnvFile = envFile
		c.Connectors["fake"] = cn
	})
	out, _ := h.callFake("get_secret_echo", map[string]any{"text": "FAKE_API_KEY"})
	if strings.Contains(out, secret) {
		t.Fatalf("secret reached the agent: %q", out)
	}
	if out != "value: "+redact.Placeholder {
		t.Errorf("got %q", out)
	}
	h.callFake("get_echo", map[string]any{"text": secret})
	data, _ := os.ReadFile(h.audit)
	if strings.Contains(string(data), secret) {
		t.Fatal("secret in audit log")
	}
}

func TestTruncation(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, func(c *config.Config) { c.Server.MaxResultBytes = 1000 })
	out, _ := h.callFake("get_big", map[string]any{"text": "5000"})
	if len(out) > 1300 || !strings.Contains(out, "truncated to 1000") {
		t.Errorf("len %d, tail %q", len(out), out[max(0, len(out)-120):])
	}
}

func TestUnloadKillsWholeTree(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, nil)
	out, isErr := h.callFake("get_child_pid", nil)
	if isErr {
		t.Fatal(out)
	}
	pid, err := strconv.Atoi(out)
	if err != nil || !alive.PID(pid) {
		t.Fatalf("grandchild %q not running", out)
	}
	if out, isErr := h.call("unload_connector", map[string]any{"name": "fake"}); isErr {
		t.Fatal(out)
	}
	deadline := time.Now().Add(10 * time.Second)
	for alive.PID(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if alive.PID(pid) {
		t.Fatal("grandchild survived unload_connector")
	}
	// Loads again on demand.
	if out, isErr := h.callFake("get_echo", map[string]any{"text": "back"}); isErr || out != "echo: back" {
		t.Fatalf("reload: %q", out)
	}
}

func TestAuditRecordsDecisionsNotArgs(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, nil)
	h.callFake("get_echo", map[string]any{"text": "private words"})
	h.as("coder").callFake("send_message", map[string]any{"text": "x"})
	data, _ := os.ReadFile(h.audit)
	log := string(data)
	if strings.Contains(log, "private words") {
		t.Error("raw arguments in audit log")
	}
	var calls []audit.Event
	for _, line := range strings.Split(strings.TrimSpace(log), "\n") {
		var e audit.Event
		json.Unmarshal([]byte(line), &e)
		if e.Kind == audit.KindToolCall {
			calls = append(calls, e)
		}
	}
	if len(calls) != 2 || calls[0].Decision != "allow" || calls[0].ArgsHash == "" || calls[1].Decision != "deny" {
		t.Errorf("tool_call events = %+v", calls)
	}
}

func TestConcurrentAgentsShareOneProcess(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, nil)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			want := "echo: " + strconv.Itoa(i)
			if out, _ := h.callFake("get_echo", map[string]any{"text": strconv.Itoa(i)}); out != want {
				t.Errorf("got %q want %q", out, want)
			}
		}(i)
	}
	wg.Wait()
	if n := len(h.gw.conns); n != 1 {
		t.Errorf("%d downstream instances, want 1 shared", n)
	}
}
