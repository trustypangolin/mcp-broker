package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/approval"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/identity"
	"github.com/trustypangolin/mcp-broker/internal/server"
)

const base = `# my broker
agents:
  vscode: {role: coder}   # VS Code
roles:
  coder: {connectors: ["*"], classes: {read: allow}}
connectors:
  fake: {command: fake.exe}
audit: {path: audit.jsonl}
`

// backend is a Backend over a config file, without the gateway.
type backend struct {
	path    string
	mu      sync.Mutex
	cfg     *config.Config
	q       *approval.Queue
	tokens  *identity.Store
	reloads int
}

func (b *backend) Config() *config.Config { b.mu.Lock(); defer b.mu.Unlock(); return b.cfg }
func (b *backend) Reload() (server.Reloaded, error) {
	cfg, err := config.Load(b.path)
	if err != nil {
		return server.Reloaded{}, err
	}
	b.mu.Lock()
	b.cfg = cfg
	b.reloads++
	b.mu.Unlock()
	return server.Reloaded{}, nil
}
func (b *backend) Status() server.Status      { return server.Status{Version: "test"} }
func (b *backend) Unload(string) bool         { return false }
func (b *backend) Approvals() *approval.Queue { return b.q }
func (b *backend) Tokens() *identity.Store    { return b.tokens }
func (b *backend) AdminToken() string         { return "admintoken" }
func (b *backend) Logger() *slog.Logger       { return slog.New(slog.DiscardHandler) }

type fixture struct {
	t      *testing.T
	b      *backend
	d      *Dashboard
	url    string
	client *http.Client
}

func newFixture(t *testing.T, text string) *fixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "broker.yaml")
	os.WriteFile(path, []byte(text), 0o600)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := identity.OpenStore(cfg.Server.TokensFile)
	b := &backend{path: path, cfg: cfg, q: approval.NewQueue(time.Minute), tokens: store}
	d := New(b)
	mux := http.NewServeMux()
	d.Mount(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return &fixture{t: t, b: b, d: d, url: ts.URL, client: &http.Client{Jar: jar}}
}

func (f *fixture) signIn() {
	f.t.Helper()
	resp, err := f.client.Get(f.url + "/ui/login?code=" + f.d.LoginCode())
	if err != nil {
		f.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path != "/ui/" || resp.StatusCode != 200 {
		f.t.Fatalf("sign-in ended at %s (%d)", resp.Request.URL, resp.StatusCode)
	}
}

func (f *fixture) do(method, path string, body any, hdr map[string]string) (int, map[string]any) {
	f.t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, f.url+path, r)
	req.Header.Set("X-Mcpb", "1")
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestSignIn(t *testing.T) {
	f := newFixture(t, base)
	resp, _ := f.client.Get(f.url + "/ui/")
	resp.Body.Close()
	if resp.Request.URL.Path != "/ui/login" {
		t.Fatalf("unauthenticated index went to %s", resp.Request.URL)
	}
	if code, _ := f.do("GET", "/ui/api/status", nil, nil); code != http.StatusUnauthorized {
		t.Fatalf("api without session: %d", code)
	}

	code := f.d.LoginCode()
	resp, _ = f.client.Get(f.url + "/ui/login?code=" + code)
	resp.Body.Close()
	if resp.Request.URL.Path != "/ui/" {
		t.Fatalf("code sign-in went to %s", resp.Request.URL)
	}
	if st, out := f.do("GET", "/ui/api/status", nil, nil); st != 200 || out["admin"] != true {
		t.Fatalf("status: %d %v", st, out)
	}

	// Codes work once.
	other := newFixture(t, base)
	other.d = f.d
	other.url = f.url
	resp, _ = other.client.Get(f.url + "/ui/login?code=" + code)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "expired or was already used") {
		t.Fatal("login code reused")
	}

	// Or the admin token.
	resp, _ = other.client.PostForm(f.url+"/ui/login", url.Values{"token": {"wrong"}})
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "not the admin token") {
		t.Fatal("wrong token accepted")
	}
	resp, _ = other.client.PostForm(f.url+"/ui/login", url.Values{"token": {"admintoken"}})
	resp.Body.Close()
	if st, _ := other.do("GET", "/ui/api/status", nil, nil); st != 200 {
		t.Fatalf("token sign-in: %d", st)
	}
}

func TestWritesNeedCSRFHeaderAndSameSite(t *testing.T) {
	f := newFixture(t, base)
	f.signIn()
	if st, _ := f.do("POST", "/ui/api/reload", nil, map[string]string{"X-Mcpb": ""}); st != http.StatusForbidden {
		t.Errorf("without header: %d", st)
	}
	if st, _ := f.do("POST", "/ui/api/reload", nil, map[string]string{"Sec-Fetch-Site": "cross-site"}); st != http.StatusForbidden {
		t.Errorf("cross-site: %d", st)
	}
	if st, _ := f.do("POST", "/ui/api/reload", nil, map[string]string{"Origin": "http://evil.example"}); st != http.StatusForbidden {
		t.Errorf("foreign origin: %d", st)
	}
	if st, _ := f.do("POST", "/ui/api/reload", nil, nil); st != 200 {
		t.Errorf("same origin: %d", st)
	}
}

func TestApproveFromDashboard(t *testing.T) {
	f := newFixture(t, base)
	f.signIn()
	e := f.b.q.Submit(approval.Plan{Kind: "tool.call", Agent: "vscode", Summary: "x", Channels: []config.Channel{config.ChanDashboard}})
	if st, out := f.do("POST", "/ui/api/approvals/"+e.ID, map[string]bool{"approve": true}, nil); st != 200 || out["status"] != "approved" || out["channel"] != "dashboard" {
		t.Fatalf("decide: %d %v", st, out)
	}
	if st, _ := f.do("POST", "/ui/api/approvals/"+e.ID, map[string]bool{"approve": false}, nil); st != http.StatusConflict {
		t.Fatalf("second decision: %d", st)
	}
}

func TestConnectorAndAgentForms(t *testing.T) {
	f := newFixture(t, base)
	f.signIn()
	st, out := f.do("PUT", "/ui/api/connectors/notes", config.ConnectorEntry{Description: "Notes", Command: "notes.exe", Args: []string{"--stdio"}, Tags: []string{"files"}}, nil)
	if st != 200 {
		t.Fatalf("put connector: %d %v", st, out)
	}
	if f.b.Config().Connectors["notes"].Command != "notes.exe" || f.b.reloads != 1 {
		t.Fatal("connector not applied")
	}
	data, _ := os.ReadFile(f.b.path)
	for _, want := range []string{"# my broker", "# VS Code", "notes:"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("file lost %q:\n%s", want, data)
		}
	}
	if st, _ := f.do("PUT", "/ui/api/connectors/bad", config.ConnectorEntry{}, nil); st != http.StatusBadRequest {
		t.Errorf("connector with neither command nor url: %d", st)
	}
	if st, _ := f.do("PUT", "/ui/api/connectors/..%2Fx", config.ConnectorEntry{Command: "x"}, nil); st != http.StatusBadRequest {
		t.Errorf("bad name: %d", st)
	}
	if st, _ := f.do("DELETE", "/ui/api/connectors/notes", nil, nil); st != 200 || len(f.b.Config().Connectors) != 1 {
		t.Fatalf("delete connector: %d", st)
	}

	// An agent with an unknown role is rejected by validation and nothing is written.
	before, _ := os.ReadFile(f.b.path)
	if st, _ := f.do("PUT", "/ui/api/agents/lan", agentForm{Role: "nope"}, nil); st != http.StatusUnprocessableEntity {
		t.Fatalf("invalid agent: %d", st)
	}
	if after, _ := os.ReadFile(f.b.path); string(after) != string(before) {
		t.Fatal("invalid edit was written")
	}
	if st, out := f.do("PUT", "/ui/api/agents/lan", agentForm{Role: "coder", Match: agentMatchForm{From: []string{"192.168.0.0/16"}}, Approvals: []string{"dashboard"}}, nil); st != 200 {
		t.Fatalf("put agent: %d %v", st, out)
	}
	if a := f.b.Config().Agents["lan"]; len(a.Match.From) != 1 || a.Approvals[0] != config.ChanDashboard {
		t.Fatalf("agent: %+v", a)
	}

	st, out = f.do("POST", "/ui/api/agents/lan/tokens", map[string]any{"save": true}, nil)
	if st != 200 || !strings.HasPrefix(out["token"].(string), identity.TokenPrefix) {
		t.Fatalf("create token: %d %v", st, out)
	}
	saved := f.b.Config().AgentTokenPath("lan")
	if b, _ := os.ReadFile(saved); strings.TrimSpace(string(b)) != out["token"] {
		t.Fatal("token not saved for the shim")
	}
	if st, _ := f.do("DELETE", "/ui/api/tokens/"+out["id"].(string), nil, nil); st != 200 {
		t.Fatalf("revoke: %d", st)
	}
	if _, err := os.Stat(saved); !os.IsNotExist(err) {
		t.Fatal("saved token left behind after revoking the agent's last token")
	}
	if _, ok := f.b.tokens.Lookup(out["token"].(string)); ok {
		t.Fatal("token still valid")
	}
}

func TestRawConfig(t *testing.T) {
	f := newFixture(t, base)
	f.signIn()
	_, out := f.do("GET", "/ui/api/config", nil, nil)
	hash := out["hash"].(string)
	if st, _ := f.do("PUT", "/ui/api/config", map[string]string{"text": base + "# edit\n", "hash": "stale"}, nil); st != http.StatusConflict {
		t.Fatalf("stale: %d", st)
	}
	if st, _ := f.do("PUT", "/ui/api/config", map[string]string{"text": "agents: [", "hash": hash}, nil); st != http.StatusUnprocessableEntity {
		t.Fatalf("invalid: %d", st)
	}
	if st, _ := f.do("PUT", "/ui/api/config", map[string]string{"text": base + "# edit\n", "hash": hash}, nil); st != 200 {
		t.Fatalf("save: %d", st)
	}
	if _, out = f.do("POST", "/ui/api/config/check", map[string]string{"text": "agents: {a: {role: missing}}"}, nil); out["ok"] != false {
		t.Fatalf("check: %v", out)
	}
}

func TestAdminFromLimitsSettingsOnly(t *testing.T) {
	f := newFixture(t, base+"dashboard: {admin_from: [10.0.0.0/8]}\n")
	f.signIn()
	if st, out := f.do("GET", "/ui/api/status", nil, nil); st != 200 || out["admin"] != false {
		t.Fatalf("status: %d %v", st, out)
	}
	if st, _ := f.do("GET", "/ui/api/approvals", nil, nil); st != 200 {
		t.Fatalf("approvals: %d", st)
	}
	for _, p := range []string{"/ui/api/config", "/ui/api/agents", "/ui/api/connectors", "/ui/api/audit"} {
		if st, _ := f.do("GET", p, nil, nil); st != http.StatusForbidden {
			t.Errorf("%s outside admin_from: %d", p, st)
		}
	}
	if st, _ := f.do("POST", "/ui/api/login-code", nil, map[string]string{"Authorization": "Bearer admintoken"}); st != http.StatusForbidden {
		t.Errorf("login-code outside admin_from: %d", st)
	}
}

func TestAuditFilter(t *testing.T) {
	f := newFixture(t, base)
	f.signIn()
	lines := `{"ts":"2026-01-01T00:00:00Z","kind":"tool_call","agent":"vscode","tool":"a"}
{"ts":"2026-01-01T00:00:01Z","kind":"connection","decision":"refused"}
{"ts":"2026-01-01T00:00:02Z","kind":"tool_call","agent":"other","tool":"b"}
`
	os.WriteFile(f.b.Config().Audit.Path, []byte(lines), 0o600)
	_, out := f.do("GET", "/ui/api/audit?kind=tool_call", nil, nil)
	if ev := out["events"].([]any); len(ev) != 2 || ev[0].(map[string]any)["tool"] != "b" {
		t.Fatalf("kind filter (newest first): %v", ev)
	}
	_, out = f.do("GET", "/ui/api/audit?agent=vscode", nil, nil)
	if ev := out["events"].([]any); len(ev) != 1 {
		t.Fatalf("agent filter: %v", ev)
	}
}

func TestLoginCodeAPI(t *testing.T) {
	f := newFixture(t, base)
	if st, _ := f.do("POST", "/ui/api/login-code", nil, nil); st != http.StatusUnauthorized {
		t.Fatalf("no token: %d", st)
	}
	st, out := f.do("POST", "/ui/api/login-code", nil, map[string]string{"Authorization": "Bearer admintoken"})
	if st != 200 || !f.d.redeem(out["code"].(string)) {
		t.Fatalf("code: %d %v", st, out)
	}
}
