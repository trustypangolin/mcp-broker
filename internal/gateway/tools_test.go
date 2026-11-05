package gateway

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/testutil/fakemcp"
)

func TestSummarize(t *testing.T) {
	for in, want := range map[string]string{
		"Echo text back": "Echo text back",
		"Insert text. Use the range param to pick where.":         "Insert text.",
		"First line\nmore detail":                                 "First line",
		"Version 1.2 is supported":                                "Version 1.2 is supported",
		strings.Repeat("word ", 40):                               strings.TrimSpace(strings.Repeat("word ", 20))[:99] + "…",
		"  <p>Creates a page</p>. Then\n<b>lots</b> of HTML here": "<p>Creates a page</p>.",
	} {
		got := summarize(in)
		if strings.HasSuffix(want, "…") {
			if !strings.HasSuffix(got, "…") || len([]rune(got)) > summaryMax {
				t.Errorf("summarize(long) = %q", got)
			}
			continue
		}
		if got != want {
			t.Errorf("summarize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLoadConnectorSummaryAndDescribe(t *testing.T) {
	h := newHarness(t, fakemcp.ModeCurrent, nil)
	h.as("coder")
	out, isErr := h.call("load_connector", map[string]any{"name": "fake"})
	if isErr {
		t.Fatal(out)
	}
	if strings.Contains(out, "inputSchema") || strings.Contains(out, "send_message") {
		t.Errorf("summary carries schemas or a denied tool:\n%s", out)
	}
	var got struct {
		Detail string
		Tools  []toolSummary
		Next   string
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("summary isn't JSON: %v\n%s", err, out)
	}
	byName := map[string]toolSummary{}
	for _, ts := range got.Tools {
		byName[ts.Name] = ts
	}
	if got.Detail != "summary" || !strings.Contains(got.Next, "describe_tool") ||
		byName["get_echo"].Summary != "Echo text back" || !byName["delete_thing"].Approval {
		t.Errorf("summary = %+v", got)
	}
	if n := strings.Count(out, "\n"); n != len(got.Tools)+1 {
		t.Errorf("want one line per tool, got %d newlines:\n%s", n, out)
	}

	out, isErr = h.call("describe_tool", map[string]any{"connector": "fake", "tool": "get_echo"})
	var desc struct {
		Tool toolInfo `json:"tool"`
	}
	json.Unmarshal([]byte(out), &desc)
	if isErr || desc.Tool.Name != "get_echo" || desc.Tool.InputSchema == nil || desc.Tool.Class != "read" {
		t.Errorf("describe_tool = %s", out)
	}
	if out, isErr := h.call("describe_tool", map[string]any{"connector": "fake", "tool": "send_message"}); !isErr || !strings.Contains(out, "unknown tool") {
		t.Errorf("describe a denied tool = %s", out)
	}
	if out, isErr := h.call("describe_tool", map[string]any{"connector": "nope", "tool": "x"}); !isErr || !strings.Contains(out, "unknown connector") {
		t.Errorf("describe on an unknown connector = %s", out)
	}
	if out, isErr := h.as("outsider").call("describe_tool", map[string]any{"connector": "fake", "tool": "get_echo"}); !isErr || !strings.Contains(out, "unknown connector") {
		t.Errorf("describe on a hidden connector = %s", out)
	}
	if out, isErr := h.as("coder").call("load_connector", map[string]any{"name": "fake", "detail": "everything"}); !isErr {
		t.Errorf("bad detail = %s", out)
	}
}

// agentSession connects a client to agent's own server, counting tools/list_changed.
func agentSession(t *testing.T, h *harness, agent string, changed *atomic.Int32) *mcp.ClientSession {
	t.Helper()
	srvT, cliT := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := h.gw.ServerFor(agent).Connect(ctx, srvT, nil); err != nil {
		t.Fatal(err)
	}
	c := mcp.NewClient(&mcp.Implementation{Name: "t"}, &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			if changed != nil {
				changed.Add(1)
			}
		},
	})
	cs, err := c.Connect(ctx, cliT, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func toolNames(t *testing.T, cs *mcp.ClientSession) string {
	t.Helper()
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	return strings.Join(names, ",")
}

func TestToolsPerAgent(t *testing.T) {
	h := newHarness(t, "", withProfile(false))
	const conn = "call_tool,describe_tool,list_connectors,load_connector,unload_connector"
	for agent, want := range map[string]string{
		// owner: connectors and profiles, approvals in the tray too.
		"owner": "approval_status,call_tool,describe_tool,list_connectors,list_profiles,load_connector,profile,unload_connector",
		// coder: connectors only (no profiles), approvals in the tray.
		"coder": "approval_status," + conn,
		// outsider: no connectors but a profile; approvals default to the tray and dashboard.
		"outsider": "approval_status,list_profiles,profile",
		"nobody":   "",
	} {
		if got := toolNames(t, agentSession(t, h, agent, nil)); got != want {
			t.Errorf("%s: tools %s\nwant %s", agent, got, want)
		}
	}
	// Same server for the same agent.
	if first, again := h.gw.ServerFor("coder"), h.gw.ServerFor("coder"); first != again {
		t.Error("ServerFor built a second server")
	}

	// Leaner for an agent without profiles: the dashboard's figure matches the listing.
	cs := agentSession(t, h, "coder", nil)
	res, _ := cs.ListTools(context.Background(), nil)
	b, _ := json.Marshal(res.Tools)
	listed := len(b) + len(cs.InitializeResult().Instructions)
	if got := h.gw.BrokerContextBytes("coder"); got != listed {
		t.Errorf("BrokerContextBytes(coder) = %d, the session got %d", got, listed)
	}
	// Budgets, so a description that grows back shows up here.
	if owner, coder := h.gw.BrokerContextBytes("owner"), h.gw.BrokerContextBytes("coder"); owner > 3200 || coder > 2100 || coder >= owner {
		t.Errorf("broker context: owner %d bytes (budget 3200), coder %d (budget 2100)", owner, coder)
	}
}

func TestReloadChangesToolsLive(t *testing.T) {
	h := newHarness(t, "", withProfile(false))
	var changed atomic.Int32
	cs := agentSession(t, h, "coder", &changed)
	if got := toolNames(t, cs); strings.Contains(got, "profile") {
		t.Fatalf("coder starts with profile tools: %s", got)
	}

	// Grant coder a profile: the live session hears about it and lists the tools.
	cfg := *h.cfg
	cfg.Roles = map[string]config.Role{}
	for k, v := range h.cfg.Roles {
		cfg.Roles[k] = v
	}
	r := cfg.Roles["coder"]
	r.Profiles = map[string]config.Action{"status": config.Allow}
	cfg.Roles["coder"] = r
	h.gw.Reload(&cfg)
	deadline := time.Now().Add(5 * time.Second)
	for changed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if changed.Load() == 0 {
		t.Fatal("no tools/list_changed after the reload")
	}
	if got := toolNames(t, cs); !strings.Contains(got, "list_profiles") || !strings.Contains(got, "profile") {
		t.Errorf("after granting a profile: %s", got)
	}

	// Take connectors away: the connector tools go.
	r.Connectors = nil
	cfg.Roles["coder"] = r
	h.gw.Reload(&cfg)
	deadline = time.Now().Add(5 * time.Second)
	for strings.Contains(toolNames(t, cs), "call_tool") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := toolNames(t, cs); strings.Contains(got, "call_tool") || strings.Contains(got, "load_connector") {
		t.Errorf("after removing connectors: %s", got)
	}
}
