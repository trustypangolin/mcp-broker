package dashboard

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/approval"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/gateway"
	"github.com/trustypangolin/mcp-broker/internal/identity"
	"github.com/trustypangolin/mcp-broker/internal/server"
	"github.com/trustypangolin/mcp-broker/internal/skillgen"
	"github.com/trustypangolin/mcp-broker/internal/supervisor"
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
	sup     *supervisor.Supervisor
	tools   map[string]gateway.ToolList
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
	if b.sup != nil {
		b.sup.SetConfig(cfg)
	}
	b.mu.Unlock()
	return server.Reloaded{}, nil
}
func (b *backend) Status() server.Status      { return server.Status{Version: "test"} }
func (b *backend) Unload(string) bool         { return false }
func (b *backend) Approvals() *approval.Queue { return b.q }
func (b *backend) Tokens() *identity.Store    { return b.tokens }
func (b *backend) AdminToken() string         { return "admintoken" }
func (b *backend) Logger() *slog.Logger       { return slog.New(slog.DiscardHandler) }
func (b *backend) Discover(_ context.Context, name, _ string) (gateway.ToolList, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	l := b.tools[name]
	if len(l.Tools) == 0 {
		return l, fmt.Errorf("%s failed to start", name)
	}
	return l, nil
}
func (b *backend) SignIn(name string) (string, error) {
	if cn, ok := b.Config().Connectors[name]; !ok || !cn.Remote() {
		return "", fmt.Errorf("connector %s is local", name)
	}
	return "https://as.example/authorize?client_id=dyn", nil
}
func (b *backend) SignOut(string) error          { return nil }
func (b *backend) BrokerContextBytes(string) int { return 6000 }
func (b *backend) AgentSkills(_ context.Context, agent string, o gateway.SkillOptions) ([]skillgen.Connector, []gateway.SkillConnector) {
	c := skillgen.Connector{Name: "fake", Description: "for " + agent, Source: "live", Tools: []skillgen.Tool{{Name: "get_echo", Class: "read"}}}
	if o.Direct {
		c.Direct = &skillgen.Direct{Transport: "stdio", Command: "fake.exe"}
	}
	return []skillgen.Connector{c}, []gateway.SkillConnector{{Connector: "fake", Tools: 1, Source: "live"}}
}
func (b *backend) AgentDescription(name string) string {
	if cn := b.Config().Connectors[name]; !cn.NeedsDescription() {
		return cn.Description
	}
	return "made up for " + name
}
func (b *backend) Tools(name string) gateway.ToolList {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tools[name]
}
func (b *backend) Supervisor() *supervisor.Supervisor {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sup == nil {
		b.sup = supervisor.New(supervisor.Options{Config: b.cfg})
	}
	return b.sup
}

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

	// MCP off: the form writes mcp: false, lists it back, and a later save keeps it only
	// if the form sends it (the checkbox).
	off := false
	if st, out := f.do("PUT", "/ui/api/agents/lan", agentForm{Role: "coder", Approvals: []string{"dashboard"}, MCP: &off}, nil); st != 200 {
		t.Fatalf("put agent mcp off: %d %v", st, out)
	}
	if !f.b.Config().Agents["lan"].MCPOff() {
		t.Fatal("mcp: false not applied")
	}
	if b, _ := os.ReadFile(f.b.path); !strings.Contains(string(b), "mcp: false") {
		t.Fatalf("broker.yaml lacks mcp: false:\n%s", b)
	}
	_, list := f.do("GET", "/ui/api/agents", nil, nil)
	for _, a := range list["agents"].([]any) {
		if a := a.(map[string]any); a["name"] == "lan" && a["def"].(map[string]any)["mcp"] != false {
			t.Errorf("agent list doesn't show mcp off: %v", a["def"])
		}
	}
	if st, _ := f.do("PUT", "/ui/api/agents/lan", agentForm{Role: "coder", Approvals: []string{"dashboard"}}, nil); st != 200 || f.b.Config().Agents["lan"].MCPOff() {
		t.Fatal("turning MCP back on")
	}
}

func TestAgentSkillsDownload(t *testing.T) {
	f := newFixture(t, base)
	f.signIn()
	get := func(query string) (*http.Response, []string) {
		t.Helper()
		req, _ := http.NewRequest("POST", f.url+"/ui/api/skills"+query, nil)
		req.Header.Set("X-Mcpb", "1")
		resp, err := f.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" {
			t.Fatalf("skills%s: %d %s", query, resp.StatusCode, body)
		}
		zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, zf := range zr.File {
			names = append(names, zf.Name)
		}
		return resp, names
	}

	resp, names := get("?agent=vscode")
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "mcp-broker-skills-vscode-claude.zip") || !strings.Contains(resp.Header.Get("X-Mcpb-Skills"), `"skill":"broker-fake"`) {
		t.Errorf("headers: %v", resp.Header)
	}
	for _, want := range []string{".claude/skills/broker-fake/SKILL.md", ".claude/skills/mcp-broker-api/SKILL.md", "README.md"} {
		if !slices.Contains(names, want) {
			t.Errorf("zip lacks %s: %v", want, names)
		}
	}
	// One connector, for Copilot, straight to its server.
	_, names = get("?agent=vscode&connectors=fake&format=copilot&target=direct")
	for _, want := range []string{".github/instructions/broker-fake.instructions.md", "mcp-broker/broker-fake/server.json", "mcp-broker/broker-fake/scripts/mcp_call.ps1"} {
		if !slices.Contains(names, want) {
			t.Errorf("direct copilot zip lacks %s: %v", want, names)
		}
	}
	// No agent: the meta-skill alone, in any layout.
	_, names = get("?format=agents-md&base=https%3A%2F%2Fbroker.corp%2Fapi%2Fv1")
	if !slices.Equal(names, []string{"AGENTS.md", "mcp-broker/mcp-broker-api/SKILL.md", "README.md"}) {
		t.Errorf("meta-only zip: %v", names)
	}

	if st, out := f.do("GET", "/ui/api/skills/formats", nil, nil); st != 200 || len(out["formats"].([]any)) != len(skillgen.Formats) {
		t.Errorf("formats: %d %v", st, out)
	}
	// Writes need the CSRF header; unknown agents and formats are refused.
	if st, _ := f.do("POST", "/ui/api/skills?agent=vscode", nil, map[string]string{"X-Mcpb": ""}); st != http.StatusForbidden {
		t.Errorf("without CSRF header: %d", st)
	}
	if st, _ := f.do("POST", "/ui/api/skills?agent=nobody", nil, nil); st != http.StatusNotFound {
		t.Errorf("unknown agent: %d", st)
	}
	if st, _ := f.do("POST", "/ui/api/skills?format=nope", nil, nil); st != http.StatusBadRequest {
		t.Errorf("unknown format: %d", st)
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

func TestSetGroup(t *testing.T) {
	f := newFixture(t, base)
	f.signIn()
	st, out := f.do("POST", "/ui/api/groups", map[string]any{"section": "connectors", "names": []string{"fake"}, "group": " Fakes "}, nil)
	if st != 200 || f.b.Config().Connectors["fake"].Group != "Fakes" {
		t.Fatalf("set group: %d %v", st, out)
	}
	data, _ := os.ReadFile(f.b.path)
	if !strings.Contains(string(data), "# VS Code") {
		t.Errorf("file lost comments:\n%s", data)
	}
	// The connector list carries it, so a form save keeps it.
	_, out = f.do("GET", "/ui/api/connectors", nil, nil)
	if g := out["connectors"].([]any)[0].(map[string]any)["def"].(map[string]any)["group"]; g != "Fakes" {
		t.Errorf("list group: %v", g)
	}
	if st, _ := f.do("POST", "/ui/api/groups", map[string]any{"section": "connectors", "names": []string{"fake"}, "group": ""}, nil); st != 200 || f.b.Config().Connectors["fake"].Group != "" {
		t.Errorf("ungroup: %d", st)
	}
	for _, body := range []map[string]any{
		{"section": "connectors", "names": []string{"missing"}, "group": "x"},
		{"section": "agents", "names": []string{"vscode"}, "group": "x"},
		{"section": "connectors", "names": []string{}, "group": "x"},
		{"section": "connectors", "names": []string{"fake"}, "group": "a\nb"},
	} {
		if st, _ := f.do("POST", "/ui/api/groups", body, nil); st == 200 {
			t.Errorf("accepted %v", body)
		}
	}
}

func TestProfileForm(t *testing.T) {
	f := newFixture(t, base+"profiles:\n  # kept\n  app:\n    command: app.exe\n    env_file: app.env\n    update:\n      - [git, pull]\n")
	f.signIn()
	_, out := f.do("GET", "/ui/api/profiles", nil, nil)
	def := out["profiles"].([]any)[0].(map[string]any)["def"].(map[string]any)
	if def["env_file"] != "app.env" || def["health"] != nil { // as written: not made absolute, no defaults
		t.Fatalf("def: %v", def)
	}

	e := config.ProfileEntry{Command: "app.exe", EnvFile: "app.env", Ports: []int{9000},
		Health: &config.HealthEntry{HTTP: "http://127.0.0.1:9000/"}, Stop: &config.StopEntry{},
		Update: []config.FlowList{{"git", "pull"}, {"git", "reset", "--hard", "${APP_VERSION}"}}}
	if st, out := f.do("PUT", "/ui/api/profiles/app", e, nil); st != 200 {
		t.Fatalf("put: %d %v", st, out)
	}
	p := f.b.Config().Profiles["app"]
	if len(p.Update) != 2 || p.Ports[0] != 9000 || p.Health.HTTP == "" {
		t.Fatalf("saved: %+v", p)
	}
	data, _ := os.ReadFile(f.b.path)
	if s := string(data); !strings.Contains(s, "# kept\n  app:") || !strings.Contains(s, "- [git, reset, --hard, '${APP_VERSION}']") || strings.Contains(s, "stop:") {
		t.Errorf("file:\n%s", s)
	}

	for _, c := range []struct {
		path string
		e    config.ProfileEntry
		want int
	}{
		{"/ui/api/profiles/app?new=1", config.ProfileEntry{Command: "x"}, http.StatusConflict},
		{"/ui/api/profiles/nocmd", config.ProfileEntry{}, http.StatusBadRequest},
		{"/ui/api/profiles/..%2Fx", config.ProfileEntry{Command: "x"}, http.StatusBadRequest},
		{"/ui/api/profiles/bad", config.ProfileEntry{Command: "x", Stop: &config.StopEntry{Timeout: "soon"}}, http.StatusUnprocessableEntity},
	} {
		if st, out := f.do("PUT", c.path, c.e, nil); st != c.want {
			t.Errorf("%s: %d %v, want %d", c.path, st, out, c.want)
		}
	}

	if st, out := f.do("PUT", "/ui/api/profiles/app-copy?new=1", config.ProfileEntry{Command: "app.exe"}, nil); st != 200 {
		t.Fatalf("add: %d %v", st, out)
	}
	if _, out := f.do("GET", "/ui/api/profiles", nil, nil); len(out["profiles"].([]any)) != 2 {
		t.Errorf("list after add: %v", out)
	}
	if st, _ := f.do("DELETE", "/ui/api/profiles/app-copy", nil, nil); st != 200 || len(f.b.Config().Profiles) != 1 {
		t.Errorf("delete: %d", st)
	}
	if st, _ := f.do("DELETE", "/ui/api/profiles/app-copy", nil, nil); st != http.StatusNotFound {
		t.Errorf("delete missing: %d", st)
	}
}

func TestAttachAWSProfile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "config"), []byte("[profile shared]\ncredential_process = signer --key k.pem --role-arn arn:aws:iam::123456789012:role/Admin\nregion = ap-southeast-2\n"), 0o600)
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(dir, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(dir, "none"))
	f := newFixture(t, base)
	f.signIn()
	if st, out := f.do("POST", "/ui/api/aws/shared/attach", map[string]any{"read_only": true}, nil); st != 200 {
		t.Fatalf("attach: %d %v", st, out)
	}
	c := f.b.Config().Connectors["aws-shared"]
	wantArgs := []string{"mcp-proxy-for-aws@latest", awsEndpoint, "--profile", "shared", "--region", "us-east-1", "--metadata", "AWS_REGION=ap-southeast-2"}
	if c.Env["AWS_PROFILE"] != "shared" || c.Group != "AWS" ||
		c.ToolClasses["aws___run_script"] != config.Read || !slices.Equal(c.Args, wantArgs) {
		t.Fatalf("connector: %+v", c)
	}
	// Agents are told the account, role and region, and can match the account number.
	if want := "AWS account 123456789012 (role Admin, region ap-southeast-2) as profile shared (process), read-only role"; c.Description != want || !slices.Equal(c.Aliases, []string{"123456789012"}) {
		t.Errorf("description %q aliases %v", c.Description, c.Aliases)
	}
	data, _ := os.ReadFile(f.b.path)
	if strings.Contains(string(data), "signer") {
		t.Error("credential_process leaked into broker.yaml")
	}
	_, out := f.do("GET", "/ui/api/aws", nil, nil)
	p := out["profiles"].([]any)[0].(map[string]any)
	if p["attached"] != "aws-shared" || p["read_only"] != true || p["kind"] != "process" || p["account_id"] != "123456789012" || p["role"] != "Admin" {
		t.Errorf("list: %v", p)
	}
	if strings.Contains(fmt.Sprint(out), "signer") {
		t.Error("credential_process leaked into the API")
	}
	// Re-attaching updates the same connector.
	if st, _ := f.do("POST", "/ui/api/aws/shared/attach", map[string]any{"read_only": false}, nil); st != 200 || f.b.Config().Connectors["aws-shared"].ToolClasses != nil {
		t.Errorf("update: %d %+v", st, f.b.Config().Connectors["aws-shared"])
	}
	if st, _ := f.do("POST", "/ui/api/aws/nope/attach", map[string]any{}, nil); st != http.StatusNotFound {
		t.Errorf("unknown profile: %d", st)
	}
	// Updating keeps a description, aliases and group a person set.
	e := f.b.Config().Connectors["aws-shared"].Entry()
	e.Description, e.Aliases, e.Group = "Shared services account", []string{"shared"}, "Cloud"
	if st, out := f.do("PUT", "/ui/api/connectors/aws-shared", e, nil); st != 200 {
		t.Fatalf("edit: %d %v", st, out)
	}
	if st, _ := f.do("POST", "/ui/api/aws/shared/attach", map[string]any{}, nil); st != 200 {
		t.Fatalf("re-attach: %d", st)
	}
	c = f.b.Config().Connectors["aws-shared"]
	if c.Description != "Shared services account" || c.Group != "Cloud" || !slices.Equal(c.Aliases, []string{"123456789012", "shared"}) {
		t.Errorf("labels lost on update: %+v", c)
	}
}

func TestGroupDescription(t *testing.T) {
	f := newFixture(t, base)
	f.signIn()
	if st, _ := f.do("PUT", "/ui/api/groups/Fakes", map[string]any{"description": "x"}, nil); st != http.StatusNotFound {
		t.Errorf("describe an empty group: %d", st)
	}
	f.do("POST", "/ui/api/groups", map[string]any{"section": "connectors", "names": []string{"fake"}, "group": "Fakes"}, nil)
	if st, out := f.do("PUT", "/ui/api/groups/Fakes", map[string]any{"description": "  Test\n servers "}, nil); st != 200 {
		t.Fatalf("describe: %d %v", st, out)
	}
	if d := f.b.Config().Groups["Fakes"].Description; d != "Test servers" {
		t.Fatalf("description = %q", d)
	}
	_, out := f.do("GET", "/ui/api/connectors", nil, nil)
	if fmt.Sprint(out["groups"]) != "map[Fakes:map[description:Test servers]]" {
		t.Errorf("list groups: %v", out["groups"])
	}
	// Renaming the group takes its description along; ungrouping drops it.
	f.do("POST", "/ui/api/groups", map[string]any{"section": "connectors", "names": []string{"fake"}, "group": "Mocks", "from": "Fakes"}, nil)
	if g := f.b.Config().Groups; len(g) != 1 || g["Mocks"].Description != "Test servers" {
		t.Errorf("after rename: %v", g)
	}
	f.do("POST", "/ui/api/groups", map[string]any{"section": "connectors", "names": []string{"fake"}, "group": "", "from": "Mocks"}, nil)
	if g := f.b.Config().Groups; len(g) != 0 {
		t.Errorf("after ungroup: %v", g)
	}
	// An empty description removes the entry.
	f.do("POST", "/ui/api/groups", map[string]any{"section": "connectors", "names": []string{"fake"}, "group": "Fakes"}, nil)
	f.do("PUT", "/ui/api/groups/Fakes", map[string]any{"description": "x"}, nil)
	if st, _ := f.do("PUT", "/ui/api/groups/Fakes", map[string]any{"description": ""}, nil); st != 200 || len(f.b.Config().Groups) != 0 {
		t.Errorf("clear: %d %v", st, f.b.Config().Groups)
	}
}

// A connector without a real description is flagged, and the list says what agents see.
func TestConnectorAgentDescription(t *testing.T) {
	f := newFixture(t, base)
	f.signIn()
	e := f.b.Config().Connectors["fake"].Entry()
	e.Description = "Imported from claude-code"
	if st, out := f.do("PUT", "/ui/api/connectors/fake", e, nil); st != 200 {
		t.Fatalf("put: %d %v", st, out)
	}
	_, out := f.do("GET", "/ui/api/connectors", nil, nil)
	c := out["connectors"].([]any)[0].(map[string]any)
	if c["needs_description"] != true || c["agent_description"] != "made up for fake" || !strings.Contains(fmt.Sprint(c["issues"]), "no description") {
		t.Errorf("connector view: %v", c)
	}
}

func TestDuplicateConnector(t *testing.T) {
	f := newFixture(t, base)
	f.signIn()
	// An exact copy of fake under a new name is refused, naming the original.
	st, out := f.do("PUT", "/ui/api/connectors/fake-copy?new=1", config.ConnectorEntry{Command: "fake.exe", Description: "copy"}, nil)
	if st != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "same server as fake") {
		t.Fatalf("exact copy: %d %v", st, out)
	}
	// A new connector never replaces an existing name.
	if st, out := f.do("PUT", "/ui/api/connectors/fake?new=1", config.ConnectorEntry{Command: "other.exe"}, nil); st != http.StatusConflict {
		t.Fatalf("new over existing: %d %v", st, out)
	}
	// A copy that differs in an argument is a different server.
	if st, out := f.do("PUT", "/ui/api/connectors/fake-2?new=1", config.ConnectorEntry{Command: "fake.exe", Args: []string{"--two"}}, nil); st != 200 {
		t.Fatalf("different copy: %d %v", st, out)
	}
	// Editing a connector in place isn't a duplicate of itself, but editing it into
	// another one is.
	if st, out := f.do("PUT", "/ui/api/connectors/fake", config.ConnectorEntry{Command: "fake.exe", Description: "edited"}, nil); st != 200 {
		t.Fatalf("edit in place: %d %v", st, out)
	}
	if st, _ := f.do("PUT", "/ui/api/connectors/fake-2", config.ConnectorEntry{Command: "fake.exe"}, nil); st != http.StatusConflict {
		t.Fatalf("edit into a duplicate: %d", st)
	}
}

func TestAWSProfileOf(t *testing.T) {
	for _, tc := range []struct {
		c    config.Connector
		want string
	}{
		{config.Connector{Args: []string{"p", "--profile", "a"}, Env: map[string]string{"AWS_PROFILE": "stale"}}, "a"},
		{config.Connector{Args: []string{"p", "--profile=b"}}, "b"},
		{config.Connector{Env: map[string]string{"AWS_PROFILE": "c"}}, "c"},
		{config.Connector{Args: []string{"p"}}, ""},
	} {
		if got := awsProfileOf(tc.c); got != tc.want {
			t.Errorf("%+v: %q, want %q", tc.c, got, tc.want)
		}
	}
}
