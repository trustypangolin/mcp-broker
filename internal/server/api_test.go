package server

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/approval"
	"github.com/trustypangolin/mcp-broker/internal/audit"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/gateway"
	"github.com/trustypangolin/mcp-broker/internal/identity"
	"github.com/trustypangolin/mcp-broker/internal/policy"
	"github.com/trustypangolin/mcp-broker/internal/redact"
	"github.com/trustypangolin/mcp-broker/internal/testutil/fakemcp"
	"github.com/trustypangolin/mcp-broker/skills"
)

const apiConfig = `
agents:
  bot: {role: bot, approvals: [dashboard]}
  apionly: {role: bot, approvals: [dashboard], mcp: false}
  nobody: {role: none}
roles:
  bot:
    connectors: ["*"]
    classes: {read: allow, write: allow, destructive: approve, external: deny}
  none: {connectors: []}
connectors:
  fake: {description: Fake downstream, command: placeholder}
`

type apiFixture struct {
	url    string
	tokens map[string]string
	q      *approval.Queue
	audit  string
}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(apiConfig), dir)
	if err != nil {
		t.Fatal(err)
	}
	cn := cfg.Connectors["fake"]
	cn.Command, cn.Env = os.Args[0], fakemcp.Env(fakemcp.ModeCurrent)
	cfg.Connectors["fake"] = cn
	store, err := identity.OpenStore(cfg.Server.TokensFile)
	if err != nil {
		t.Fatal(err)
	}
	f := &apiFixture{tokens: map[string]string{}, q: approval.NewQueue(time.Minute), audit: filepath.Join(dir, "audit.jsonl")}
	for _, a := range []string{"bot", "apionly", "nobody"} {
		if f.tokens[a], _, err = store.Create(a, ""); err != nil {
			t.Fatalf("token for %s: %v", a, err)
		}
	}
	red := redact.New()
	aud, _ := audit.Open(f.audit, red)
	gw := gateway.New(gateway.Options{Config: cfg, Policy: policy.New(cfg), Approvals: f.q, Audit: aud, Redactor: red, ApprovalWait: 300 * time.Millisecond})
	ts := httptest.NewServer(Handler(Options{Identifier: identity.New(cfg, store), Gateway: gw, Audit: aud}))
	t.Cleanup(func() {
		ts.Close()
		gw.Close()
		aud.Close()
	})
	f.url = ts.URL
	return f
}

// do sends one API request and decodes a JSON answer into a generic map.
func (f *apiFixture) do(t *testing.T, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, f.url+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("User-Agent", "api-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") && ct != "application/json; charset=utf-8" {
		t.Errorf("%s %s: Content-Type %q, want an explicit utf-8 charset", method, path, ct)
	}
	if strings.HasPrefix(ct, "application/json") {
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("%s %s: bad JSON %q", method, path, b)
		}
	}
	return resp.StatusCode, out
}

func text(t *testing.T, res map[string]any) string {
	t.Helper()
	c, _ := res["content"].([]any)
	if len(c) == 0 {
		t.Fatalf("no content in %v", res)
	}
	s, _ := c[0].(map[string]any)["text"].(string)
	return s
}

func TestAPIFlow(t *testing.T) {
	f := newAPIFixture(t)
	tok := f.tokens["bot"]

	code, idx := f.do(t, "GET", "/api/v1", tok, "")
	if code != 200 || idx["agent"] != "bot" {
		t.Fatalf("index: %d %v", code, idx)
	}
	if eps, _ := json.Marshal(idx["endpoints"]); !strings.Contains(string(eps), "POST /api/v1/connectors/{name}/tools/{tool}/call") {
		t.Errorf("index endpoints: %s", eps)
	}

	code, res := f.do(t, "GET", "/api/v1/connectors", tok, "")
	if code != 200 || !strings.Contains(text(t, res), `"fake"`) {
		t.Fatalf("list: %d %v", code, res)
	}

	code, res = f.do(t, "GET", "/api/v1/connectors/fake/tools", tok, "")
	if code != 200 || !strings.Contains(text(t, res), "get_echo") || strings.Contains(text(t, res), "send_message") {
		t.Fatalf("load (external is denied, so send_message is hidden): %d %s", code, text(t, res))
	}
	code, res = f.do(t, "GET", "/api/v1/connectors/fake/tools?detail=full", tok, "")
	if code != 200 || !strings.Contains(text(t, res), "inputSchema") {
		t.Errorf("load full: %d %v", code, res)
	}
	code, res = f.do(t, "GET", "/api/v1/connectors/fake/tools?detail=bogus", tok, "")
	if code != 200 || res["isError"] != true {
		t.Errorf("load bogus detail: %d %v", code, res)
	}

	code, res = f.do(t, "GET", "/api/v1/connectors/fake/tools/get_echo", tok, "")
	if code != 200 || !strings.Contains(text(t, res), `"inputSchema"`) {
		t.Errorf("describe: %d %v", code, res)
	}

	code, res = f.do(t, "POST", "/api/v1/connectors/fake/tools/get_echo/call", tok, `{"arguments":{"text":"hi"}}`)
	if code != 200 || text(t, res) != "echo: hi" || res["isError"] == true {
		t.Fatalf("call: %d %v", code, res)
	}

	code, res = f.do(t, "POST", "/api/v1/connectors/fake/tools/get_failure/call", tok, `{"arguments":{"text":"x"}}`)
	if code != 200 || res["isError"] != true {
		t.Errorf("tool error: %d %v", code, res)
	}
	code, res = f.do(t, "POST", "/api/v1/connectors/fake/tools/send_message/call", tok, `{"arguments":{"text":"x"}}`)
	if code != 200 || res["isError"] != true || !strings.Contains(text(t, res), "unknown tool") {
		t.Errorf("denied tool: %d %v", code, res)
	}

	// The RPC form takes the meta-tool's MCP arguments verbatim.
	code, res = f.do(t, "POST", "/api/v1/rpc/call_tool", tok, `{"connector":"fake","tool":"get_echo","arguments":{"text":"rpc"}}`)
	if code != 200 || text(t, res) != "echo: rpc" {
		t.Errorf("rpc: %d %v", code, res)
	}

	code, res = f.do(t, "POST", "/api/v1/connectors/fake/unload", tok, "")
	if code != 200 || !strings.Contains(text(t, res), "unloaded fake") {
		t.Errorf("unload: %d %v", code, res)
	}

	data, _ := os.ReadFile(f.audit)
	if !strings.Contains(string(data), `"client_name":"http-api (api-test)"`) {
		t.Errorf("API calls not labelled in the audit log:\n%s", data)
	}
}

func TestAPIApproval(t *testing.T) {
	f := newAPIFixture(t)
	tok := f.tokens["bot"]
	call := `{"arguments":{"text":"x"}}`
	code, res := f.do(t, "POST", "/api/v1/connectors/fake/tools/delete_thing/call", tok, call)
	sc, _ := res["structuredContent"].(map[string]any)
	if code != http.StatusAccepted || sc["status"] != "pending_approval" {
		t.Fatalf("destructive call: %d %v", code, res)
	}
	id, _ := sc["approval_id"].(string)

	code, res = f.do(t, "GET", "/api/v1/approvals/"+id, tok, "")
	if code != 200 || res["structuredContent"].(map[string]any)["status"] != "pending" {
		t.Fatalf("status: %d %v", code, res)
	}
	// Someone else's approval looks unknown.
	if code, _ := f.do(t, "GET", "/api/v1/approvals/"+id, f.tokens["nobody"], ""); code != http.StatusNotFound {
		t.Errorf("nobody reads bot's approval: %d", code)
	}

	// Long-poll: the decision lands while the request waits.
	go func() {
		time.Sleep(200 * time.Millisecond)
		f.q.Decide(id, true, config.ChanDashboard)
	}()
	start := time.Now()
	code, res = f.do(t, "GET", "/api/v1/approvals/"+id+"?wait=10", tok, "")
	if code != 200 || res["structuredContent"].(map[string]any)["status"] != "approved" || time.Since(start) > 5*time.Second {
		t.Fatalf("wait: %d %v after %v", code, res, time.Since(start))
	}

	// Retry with the approval: runs once; the approval is spent.
	retry := `{"arguments":{"text":"x"},"approval_id":"` + id + `"}`
	code, res = f.do(t, "POST", "/api/v1/connectors/fake/tools/delete_thing/call", tok, retry)
	if code != 200 || text(t, res) != "deleted x" {
		t.Fatalf("retry: %d %v", code, res)
	}
	code, res = f.do(t, "POST", "/api/v1/connectors/fake/tools/delete_thing/call", tok, retry)
	if code != 200 || res["isError"] != true {
		t.Errorf("second retry with a spent approval: %d %v", code, res)
	}
	// An approval is bound to the exact arguments.
	code, _ = f.do(t, "POST", "/api/v1/connectors/fake/tools/delete_thing/call", tok, `{"arguments":{"text":"other"}}`)
	if code != http.StatusAccepted {
		t.Errorf("different arguments: %d, want a new approval", code)
	}
}

func TestAPIRefusals(t *testing.T) {
	f := newAPIFixture(t)
	if code, _ := f.do(t, "GET", "/api/v1/connectors", "", ""); code != http.StatusUnauthorized {
		t.Errorf("no token: %d", code)
	}
	if code, _ := f.do(t, "GET", "/api/v1/connectors", "mcpb_bot_nope", ""); code != http.StatusUnauthorized {
		t.Errorf("bad token: %d", code)
	}
	// nobody sees no connector, so the connector endpoints don't exist for it.
	if code, res := f.do(t, "GET", "/api/v1/connectors", f.tokens["nobody"], ""); code != http.StatusNotFound {
		t.Errorf("nobody lists connectors: %d %v", code, res)
	}
	if code, res := f.do(t, "POST", "/api/v1/rpc/call_tool", f.tokens["nobody"], `{}`); code != http.StatusNotFound {
		t.Errorf("nobody rpc call_tool: %d %v", code, res)
	}
	tok := f.tokens["bot"]
	if code, _ := f.do(t, "POST", "/api/v1/rpc/no_such_tool", tok, `{}`); code != http.StatusNotFound {
		t.Errorf("unknown meta-tool: %d", code)
	}
	if code, _ := f.do(t, "POST", "/api/v1/connectors/fake/tools/get_echo/call", tok, `not json`); code != http.StatusBadRequest {
		t.Errorf("bad body: %d", code)
	}
	if code, _ := f.do(t, "POST", "/api/v1/connectors/fake/tools/get_echo/call", tok, `{"text":"hi"}`); code != http.StatusBadRequest {
		t.Errorf("arguments not wrapped: %d", code)
	}
	if code, _ := f.do(t, "POST", "/api/v1/profiles/x/delete", tok, ""); code != http.StatusNotFound {
		t.Errorf("unknown profile action: %d", code)
	}
	if code, _ := f.do(t, "GET", "/api/v1/approvals/x?wait=soon", tok, ""); code != http.StatusBadRequest {
		t.Errorf("bad wait: %d", code)
	}
	req, _ := http.NewRequest("GET", f.url+"/api/v1/connectors", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("browser Origin: %d", resp.StatusCode)
	}
}

func TestAPISkillServed(t *testing.T) {
	f := newAPIFixture(t)
	resp, err := http.Get(f.url + "/api/v1/skill.md")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != skills.BrokerAPI {
		t.Errorf("skill.md: %d, %d bytes", resp.StatusCode, len(b))
	}
}

// TestSkillDocumentsEveryRoute keeps the skill in step with the API: every route must
// appear in SKILL.md as "METHOD path", and the skill must not document routes that don't
// exist.
func TestSkillDocumentsEveryRoute(t *testing.T) {
	routes := map[string]bool{"GET /api/v1": true, "GET /api/v1/skill.md": true, "POST /api/v1/skills": true}
	for _, rt := range apiRoutes() {
		routes[rt.method+" "+rt.path] = true
	}
	for r := range routes {
		if !strings.Contains(skills.BrokerAPI, "`"+r+"`") {
			t.Errorf("SKILL.md does not document `%s`", r)
		}
	}
	for _, m := range regexp.MustCompile("`((?:GET|POST) /api/v1[^`]*)`").FindAllStringSubmatch(skills.BrokerAPI, -1) {
		if !routes[m[1]] {
			t.Errorf("SKILL.md documents `%s`, which is not a route", m[1])
		}
	}
}

// skillZip fetches POST /api/v1/skills?query as bot, returning the files and the report.
func (f *apiFixture) skillZip(t *testing.T, query string, want int) (map[string]string, []SkillReport) {
	t.Helper()
	req, _ := http.NewRequest("POST", f.url+"/api/v1/skills"+query, nil)
	req.Header.Set("Authorization", "Bearer "+f.tokens["bot"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != want {
		t.Fatalf("skills%s: %d, want %d: %s", query, resp.StatusCode, want, b)
	}
	if want != 200 {
		return nil, nil
	}
	if resp.Header.Get("Content-Type") != "application/zip" {
		t.Fatalf("skills%s: %s", query, resp.Header.Get("Content-Type"))
	}
	var report []SkillReport
	json.Unmarshal([]byte(resp.Header.Get("X-Mcpb-Skills")), &report)
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, zf := range zr.File {
		rc, _ := zf.Open()
		d, _ := io.ReadAll(rc)
		rc.Close()
		files[zf.Name] = string(d)
	}
	return files, report
}

func TestAPISkillsZip(t *testing.T) {
	f := newAPIFixture(t)
	files, report := f.skillZip(t, "", 200)
	if len(report) != 1 || report[0].Source != "live" || report[0].Skill != "broker-fake" {
		t.Errorf("report: %+v", report)
	}
	skill := files[".claude/skills/broker-fake/SKILL.md"]
	// The skill uses the URL the caller reached the broker on.
	if !strings.Contains(skill, "${MCP_BROKER_API:-"+f.url+"/api/v1}") || !strings.Contains(skill, "get_echo") {
		t.Errorf("broker-fake/SKILL.md:\n%s", skill)
	}
	if _, ok := files[".claude/skills/mcp-broker-api/SKILL.md"]; !ok {
		t.Error("no meta-skill")
	}

	// Another layout, a central broker's URL, portable (no local token file), no meta-skill.
	files, _ = f.skillZip(t, "?format=cursor&base=https%3A%2F%2Fbroker.corp%2Fapi%2Fv1&portable=1&meta=0", 200)
	rule := files[".cursor/rules/broker-fake.mdc"]
	if !strings.Contains(rule, "${MCP_BROKER_API:-https://broker.corp/api/v1}") || strings.Contains(rule, "agent-tokens") {
		t.Errorf("cursor rule:\n%s", rule)
	}
	if _, ok := files[".cursor/rules/mcp-broker-api.mdc"]; ok {
		t.Error("meta=0 still has the meta-skill")
	}

	// Direct: straight to the server, with its settings and the bundled client.
	files, _ = f.skillZip(t, "?target=direct", 200)
	if srv := files[".claude/skills/broker-fake/server.json"]; !strings.Contains(srv, `"transport": "stdio"`) {
		t.Errorf("direct server.json:\n%s", srv)
	}
	if _, ok := files[".claude/skills/broker-fake/scripts/mcp_call.py"]; !ok {
		t.Error("direct skill without its client")
	}

	f.skillZip(t, "?format=nope", http.StatusBadRequest)
	f.skillZip(t, "?target=elsewhere", http.StatusBadRequest)
	f.skillZip(t, "?connectors=none&meta=0", http.StatusBadRequest)
	if code, _ := f.do(t, "POST", "/api/v1/skills", f.tokens["nobody"], ""); code != http.StatusNotFound {
		t.Errorf("nobody's skills: %d", code)
	}
}

func TestMCPOffAgent(t *testing.T) {
	f := newAPIFixture(t)
	resp := post(t, f.url+"/mcp", f.tokens["apionly"], "", initBody)
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("mcp: false agent on /mcp: %d", resp.StatusCode)
	}
	if resp := post(t, f.url+"/mcp", f.tokens["bot"], "", initBody); resp.StatusCode != http.StatusOK {
		t.Errorf("bot on /mcp: %d", resp.StatusCode)
	}
	code, res := f.do(t, "POST", "/api/v1/connectors/fake/tools/get_echo/call", f.tokens["apionly"], `{"arguments":{"text":"still works"}}`)
	if code != 200 || text(t, res) != "echo: still works" {
		t.Errorf("mcp: false agent on the API: %d %v", code, res)
	}
	data, _ := os.ReadFile(f.audit)
	if !strings.Contains(string(data), "MCP is off for agent apionly") {
		t.Error("refusal not audited")
	}
}
