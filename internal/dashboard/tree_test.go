package dashboard

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trustypangolin/mcp-broker/internal/approval"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/gateway"
)

const treeBase = `agents:
  desktop: {role: owner, approvals: [dashboard]}
  code: {role: owner}
  lan: {role: viewer}
roles:
  owner:
    connectors: ["*"]
    classes: {read: allow, write: allow, destructive: approve}
  viewer: {connectors: ["!aws"], classes: {read: allow}}
policy:
  floors:
    - match: "aws/delete_*"
      action: approve
connectors:
  aws: {command: aws.exe}
  fake: {command: fake.exe}
audit: {path: audit.jsonl}
`

// toolsOf finds a connector's tools in a tree response.
func toolsOf(t *testing.T, out map[string]any, conn string) map[string]map[string]any {
	t.Helper()
	conns, _ := out["connectors"].([]any)
	for _, c := range conns {
		c := c.(map[string]any)
		if c["name"] == conn {
			m := map[string]map[string]any{}
			for _, tl := range c["tools"].([]any) {
				tl := tl.(map[string]any)
				m[tl["name"].(string)] = tl
			}
			return m
		}
	}
	t.Fatalf("no connector %s in %v", conn, out)
	return nil
}

func TestToolTree(t *testing.T) {
	f := newFixture(t, treeBase)
	f.b.tools = map[string]gateway.ToolList{"aws": {Tools: []gateway.ToolMeta{
		{Name: "run_script"}, {Name: "list_regions"}, {Name: "delete_stack"},
	}}}
	os.WriteFile(filepath.Join(filepath.Dir(f.b.path), "audit.jsonl"), []byte(
		`{"ts":"2026-11-04T01:00:00Z","kind":"tool_call","agent":"desktop","connector":"aws","tool":"run_script","decision":"approve","detail":"approved"}
{"ts":"2026-11-04T02:00:00Z","kind":"tool_call","agent":"desktop","connector":"aws","tool":"run_script","decision":"approve","detail":"approved"}
{"ts":"2026-11-04T03:00:00Z","kind":"tool_call","agent":"code","connector":"aws","tool":"run_script","decision":"approve","detail":"approved"}
`), 0o600)
	f.signIn()

	st, out := f.do("GET", "/ui/api/tree?agent=desktop", nil, nil)
	if st != 200 {
		t.Fatalf("tree: %d %v", st, out)
	}
	tools := toolsOf(t, out, "aws")
	rs := tools["run_script"]
	// run_script: a write verb by name, so allow; history is desktop's own.
	if rs["approved"] != float64(2) || rs["asked"] != float64(2) {
		t.Fatalf("run_script history = %v", rs)
	}
	if tools["delete_stack"]["action"] != "approve" || tools["delete_stack"]["floor"] != "approve" {
		t.Fatalf("delete_stack = %v", tools["delete_stack"])
	}
	if a := out["agents"].([]any)[1].(map[string]any); a["name"] != "desktop" || fmt.Sprint(a["shares"]) != "[code]" {
		t.Fatalf("agents = %v", out["agents"])
	}

	// A tool rule changes the decision for the whole role; inherit removes it.
	if st, out := f.do("POST", "/ui/api/rules", map[string]any{"role": "owner", "connector": "aws", "tool": "list_regions", "action": "deny"}, nil); st != 200 {
		t.Fatalf("set rule: %d %v", st, out)
	}
	_, out = f.do("GET", "/ui/api/tree?agent=code", nil, nil)
	if lr := toolsOf(t, out, "aws")["list_regions"]; lr["action"] != "deny" || lr["rule"] != "deny" {
		t.Fatalf("after rule: %v", lr)
	}
	// Allow can't loosen a floor.
	f.do("POST", "/ui/api/rules", map[string]any{"role": "owner", "connector": "aws", "tool": "*", "action": "allow"}, nil)
	_, out = f.do("GET", "/ui/api/tree?agent=desktop", nil, nil)
	if ds := toolsOf(t, out, "aws")["delete_stack"]; ds["action"] != "approve" {
		t.Fatalf("floor loosened: %v", ds)
	}
	f.do("POST", "/ui/api/rules", map[string]any{"role": "owner", "connector": "aws", "tool": "list_regions", "action": ""}, nil)
	if rules := f.b.Config().Roles["owner"].Rules; len(rules) != 1 || rules[0].Match != "aws/*" {
		t.Fatalf("rules = %+v", rules)
	}

	for _, bad := range []map[string]any{
		{"role": "ghost", "connector": "aws", "tool": "x", "action": "allow"},
		{"role": "owner", "connector": "ghost", "tool": "x", "action": "allow"},
		{"role": "owner", "connector": "aws", "tool": "a*", "action": "allow"},
		{"role": "owner", "connector": "aws", "tool": "x", "action": "maybe"},
	} {
		if st, _ := f.do("POST", "/ui/api/rules", bad, nil); st == 200 {
			t.Errorf("accepted %v", bad)
		}
	}
	if st, out := f.do("POST", "/ui/api/tree/aws/load?agent=desktop", nil, nil); st != 200 || out["tools"] != float64(3) {
		t.Fatalf("load: %d %v", st, out)
	}
	if st, _ := f.do("POST", "/ui/api/tree/fake/load", nil, nil); st != http.StatusBadGateway {
		t.Fatalf("failed load: %d", st)
	}
	// The viewer sees no aws.
	_, out = f.do("GET", "/ui/api/tree?agent=lan", nil, nil)
	for _, c := range out["connectors"].([]any) {
		if c.(map[string]any)["name"] == "aws" {
			t.Fatal("lan sees aws")
		}
	}
}

func TestApproveAlwaysAllow(t *testing.T) {
	f := newFixture(t, treeBase)
	f.signIn()
	e := f.b.q.Submit(approval.Plan{Kind: "tool.call", Agent: "desktop", Connector: "aws", Tool: "run_script",
		Summary: "x", Channels: []config.Channel{config.ChanDashboard}})
	if st, out := f.do("POST", "/ui/api/approvals/"+e.ID, map[string]any{"approve": true, "always": true}, nil); st != 200 {
		t.Fatalf("always: %d %v", st, out)
	}
	if got, _ := f.b.q.Get(e.ID); got.Status != approval.Approved {
		t.Fatalf("status = %v", got.Status)
	}
	rules := f.b.Config().Roles["owner"].Rules
	if len(rules) != 1 || rules[0] != (config.Rule{Match: "aws/run_script", Action: config.Allow}) {
		t.Fatalf("rules = %+v", rules)
	}
	// Always with a deny makes no sense.
	e = f.b.q.Submit(approval.Plan{Kind: "tool.call", Agent: "desktop", Connector: "aws", Tool: "x",
		Summary: "x", Channels: []config.Channel{config.ChanDashboard}})
	if st, _ := f.do("POST", "/ui/api/approvals/"+e.ID, map[string]any{"approve": false, "always": true}, nil); st != http.StatusForbidden {
		t.Fatalf("deny+always: %d", st)
	}
}

func TestClientConfig(t *testing.T) {
	f := newFixture(t, treeBase)
	f.signIn()
	st, out := f.do("GET", "/ui/api/agents/desktop/client-config?harness=codex", nil, nil)
	if st != 200 {
		t.Fatalf("codex: %d %v", st, out)
	}
	sn := out["snippet"].(map[string]any)
	if sn["format"] != "toml" || !strings.Contains(sn["text"].(string), "'--agent', 'desktop'") {
		t.Fatalf("snippet = %v", sn)
	}
	if st, _ := f.do("GET", "/ui/api/agents/desktop/client-config?harness=claude-desktop&http=1", nil, nil); st != http.StatusBadRequest {
		t.Fatalf("desktop over http: %d", st)
	}
	if st, _ := f.do("GET", "/ui/api/agents/ghost/client-config?harness=codex", nil, nil); st != http.StatusNotFound {
		t.Fatalf("unknown agent: %d", st)
	}
	if _, out := f.do("GET", "/ui/api/agents/desktop/client-config", nil, nil); len(out["harnesses"].([]any)) < 7 {
		t.Fatalf("harnesses = %v", out)
	}
}
