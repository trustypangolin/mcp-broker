package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trustypangolin/mcp-broker/internal/approval"
	"github.com/trustypangolin/mcp-broker/internal/audit"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/gateway"
	"github.com/trustypangolin/mcp-broker/internal/identity"
	"github.com/trustypangolin/mcp-broker/internal/policy"
	"github.com/trustypangolin/mcp-broker/internal/proc"
	"github.com/trustypangolin/mcp-broker/internal/redact"
	"github.com/trustypangolin/mcp-broker/internal/testutil/fakemcp"
)

func TestMain(m *testing.M) {
	proc.RunHelper()
	fakemcp.MaybeRun()
	os.Exit(m.Run())
}

const testConfig = `
agents:
  alpha: {role: all}
  beta: {role: none}
  pinned:
    role: all
    match:
      process: ["*\\definitely-not-this.exe"]
roles:
  all: {connectors: ["*"], classes: {read: allow}}
  none: {connectors: []}
connectors:
  fake: {command: placeholder}
`

type fixture struct {
	url    string
	tokens map[string]string
	audit  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Parse([]byte(testConfig), dir)
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
	f := &fixture{tokens: map[string]string{}, audit: filepath.Join(dir, "audit.jsonl")}
	for _, a := range []string{"alpha", "beta", "pinned"} {
		f.tokens[a], _, _ = store.Create(a, "")
	}
	red := redact.New()
	aud, _ := audit.Open(f.audit, red)
	gw := gateway.New(gateway.Options{Config: cfg, Policy: policy.New(cfg), Approvals: approval.NewQueue(time.Minute), Audit: aud, Redactor: red})
	ts := httptest.NewServer(Handler(Options{Identifier: identity.New(cfg, store), Gateway: gw, Audit: aud}))
	t.Cleanup(func() {
		ts.Close()
		gw.Close()
		aud.Close()
	})
	f.url = ts.URL
	return f
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func (f *fixture) connect(t *testing.T, token string) (*mcp.ClientSession, error) {
	t.Helper()
	tr := &mcp.StreamableClientTransport{Endpoint: f.url + "/mcp", HTTPClient: &http.Client{Transport: bearer{token}}, MaxRetries: -1}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return mcp.NewClient(&mcp.Implementation{Name: "t"}, nil).Connect(ctx, tr, nil)
}

func listConnectors(t *testing.T, s *mcp.ClientSession) string {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_connectors"})
	if err != nil {
		t.Fatal(err)
	}
	return res.Content[0].(*mcp.TextContent).Text
}

func TestTokenSelectsAgent(t *testing.T) {
	f := newFixture(t)
	a, err := f.connect(t, f.tokens["alpha"])
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if out := listConnectors(t, a); !strings.Contains(out, `"fake"`) {
		t.Errorf("alpha sees %s", out)
	}
	b, err := f.connect(t, f.tokens["beta"])
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if out := listConnectors(t, b); strings.Contains(out, "fake") {
		t.Errorf("beta sees %s", out)
	}
}

func post(t *testing.T, url, token, session, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Protocol-Version", "2025-11-25")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"raw","version":"1"}}}`

func TestRefusals(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name, token string
		want        int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"garbage", "mcpb_alpha_nope", http.StatusUnauthorized},
		{"process mismatch", f.tokens["pinned"], http.StatusForbidden},
	}
	for _, c := range cases {
		if resp := post(t, f.url+"/mcp", c.token, "", initBody); resp.StatusCode != c.want {
			t.Errorf("%s: status %d, want %d", c.name, resp.StatusCode, c.want)
		}
	}
	data, _ := os.ReadFile(f.audit)
	if !strings.Contains(string(data), `"kind":"connection"`) {
		t.Error("refusals not audited")
	}
}

func TestNonLocalhostHostAndBrowserOrigin(t *testing.T) {
	f := newFixture(t)
	// wslc sends Host: host.wslc.internal over loopback; that must work.
	req, _ := http.NewRequest(http.MethodPost, f.url+"/mcp", strings.NewReader(initBody))
	req.Host = "host.wslc.internal:8765"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+f.tokens["alpha"])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("Host host.wslc.internal: %d", resp.StatusCode)
	}

	// A browser page, even holding a token, is refused.
	req, _ = http.NewRequest(http.MethodPost, f.url+"/mcp", strings.NewReader(initBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.tokens["alpha"])
	req.Header.Set("Origin", "https://evil.example")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("browser Origin: %d, want 403", resp.StatusCode)
	}
}

func TestSessionBoundToToken(t *testing.T) {
	f := newFixture(t)
	resp := post(t, f.url+"/mcp", f.tokens["alpha"], "", initBody)
	sid := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode != http.StatusOK || sid == "" {
		t.Fatalf("initialize: %d session %q", resp.StatusCode, sid)
	}
	list := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	if r := post(t, f.url+"/mcp", f.tokens["beta"], sid, list); r.StatusCode != http.StatusForbidden {
		t.Errorf("beta used alpha's session: %d", r.StatusCode)
	}
}

func TestHealthz(t *testing.T) {
	f := newFixture(t)
	resp, err := http.Get(f.url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %d", resp.StatusCode)
	}
}
