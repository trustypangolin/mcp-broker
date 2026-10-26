package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trustypangolin/mcp-broker/internal/server"
	"github.com/trustypangolin/mcp-broker/internal/testutil/fakemcp"
)

func TestMain(m *testing.M) {
	fakemcp.MaybeRun()
	os.Exit(m.Run())
}

func configText(role, description, adminFrom string) string {
	exe, _ := json.Marshal(os.Args[0])
	return `
server: {listen: 127.0.0.1:0}
dashboard: {admin_from: [` + adminFrom + `]}
agents:
  a: {role: ` + role + `}
roles:
  all: {connectors: ["*"], classes: {read: allow}}
  none: {connectors: []}
connectors:
  fake:
    description: ` + description + `
    command: ` + string(exe) + `
    env: {FAKEMCP: current}
`
}

type fixture struct {
	t     *testing.T
	path  string
	b     *Broker
	token string
}

func start(t *testing.T, text string, opts Options) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{t: t, path: filepath.Join(dir, "broker.yaml")}
	f.write(text)
	opts.ConfigPath = f.path
	b, err := Start(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Close)
	f.b = b
	if f.token, _, err = b.Tokens().Create("a", ""); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) write(text string) {
	f.t.Helper()
	if err := os.WriteFile(f.path, []byte(text), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func (f *fixture) session() *mcp.ClientSession {
	f.t.Helper()
	tr := &mcp.StreamableClientTransport{Endpoint: f.b.BaseURL() + "/mcp", HTTPClient: &http.Client{Transport: bearer{f.token}}, MaxRetries: -1}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := mcp.NewClient(&mcp.Implementation{Name: "t"}, nil).Connect(ctx, tr, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { s.Close() })
	return s
}

func call(t *testing.T, s *mcp.ClientSession, tool string, args map[string]any) string {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res.Content[0].(*mcp.TextContent).Text
}

func TestReloadChangesWhatAnOpenSessionSees(t *testing.T) {
	f := start(t, configText("none", "Fake", `"127.0.0.1/32"`), Options{})
	s := f.session()
	if out := call(t, s, "list_connectors", nil); strings.Contains(out, "fake") {
		t.Fatalf("before reload: %s", out)
	}
	f.write(configText("all", "Fake", `"127.0.0.1/32"`))
	if _, err := f.b.Reload(); err != nil {
		t.Fatal(err)
	}
	if out := call(t, s, "list_connectors", nil); !strings.Contains(out, `"fake"`) {
		t.Fatalf("after reload: %s", out)
	}
}

func TestReloadKeepsRunningConfigWhenFileIsInvalid(t *testing.T) {
	f := start(t, configText("all", "Fake", `"127.0.0.1/32"`), Options{})
	f.write("agents: {a: {role: missing}}\n")
	if _, err := f.b.Reload(); err == nil {
		t.Fatal("reload of an invalid config succeeded")
	}
	if f.b.Config().Agents["a"].Role != "all" {
		t.Fatalf("running config replaced: %+v", f.b.Config().Agents)
	}
}

func TestReloadUnloadsOnlyChangedConnectors(t *testing.T) {
	f := start(t, configText("all", "Fake", `"127.0.0.1/32"`), Options{})
	s := f.session()
	call(t, s, "load_connector", map[string]any{"name": "fake"})

	// Same definition (an agent edit only): the connector keeps running.
	res, err := f.b.Reload()
	if err != nil || len(res.Unloaded) != 0 {
		t.Fatalf("unchanged reload: %+v, %v", res, err)
	}
	f.write(configText("all", "Changed", `"127.0.0.1/32"`))
	if res, err = f.b.Reload(); err != nil || len(res.Unloaded) != 1 || res.Unloaded[0] != "fake" {
		t.Fatalf("changed reload: %+v, %v", res, err)
	}
	if out := call(t, s, "call_tool", map[string]any{"connector": "fake", "tool": "get_echo", "arguments": map[string]any{"text": "hi"}}); out != "echo: hi" {
		t.Fatalf("after restart: %s", out)
	}
}

func TestReloadReportsRestartOnlySettings(t *testing.T) {
	f := start(t, configText("all", "Fake", `"127.0.0.1/32"`), Options{})
	f.write(strings.Replace(configText("all", "Fake", `"127.0.0.1/32"`), "listen: 127.0.0.1:0", "listen: 127.0.0.1:1", 1))
	res, err := f.b.Reload()
	if err != nil || len(res.Restart) != 1 || res.Restart[0] != "server.listen" {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestWatchReloadsOnEdit(t *testing.T) {
	defer func(d time.Duration) { watchInterval = d }(watchInterval)
	watchInterval = 50 * time.Millisecond
	got := make(chan error, 4)
	f := start(t, configText("none", "Fake", `"127.0.0.1/32"`), Options{
		Watch:    true,
		OnReload: func(_ server.Reloaded, err error) { got <- err },
	})
	time.Sleep(20 * time.Millisecond) // a different mod time on coarse filesystems
	f.write(configText("all", "Fake", `"127.0.0.1/32"`) + "\n# edited\n")
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watcher did not reload")
	}
	if f.b.Config().Agents["a"].Role != "all" {
		t.Fatal("edit not applied")
	}
}

func (f *fixture) admin(method, path, token string) int {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.b.BaseURL()+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestAdminAPI(t *testing.T) {
	f := start(t, configText("all", "Fake", `"127.0.0.1/32"`), Options{})
	adm := f.b.AdminToken()
	for _, c := range []struct {
		method, path, token string
		want                int
	}{
		{"GET", "/admin/status", "", http.StatusUnauthorized},
		{"GET", "/admin/status", f.token, http.StatusUnauthorized}, // an agent token is not the admin token
		{"GET", "/admin/status", adm, http.StatusOK},
		{"POST", "/admin/reload", adm, http.StatusOK},
		{"POST", "/admin/connectors/fake/unload", adm, http.StatusOK},
	} {
		if got := f.admin(c.method, c.path, c.token); got != c.want {
			t.Errorf("%s %s: got %d, want %d", c.method, c.path, got, c.want)
		}
	}

	// Outside admin_from: status and approvals still work, changes do not.
	f.write(configText("all", "Fake", `"10.0.0.0/8"`))
	if _, err := f.b.Reload(); err != nil {
		t.Fatal(err)
	}
	if got := f.admin("GET", "/admin/status", adm); got != http.StatusOK {
		t.Errorf("status outside admin_from: %d", got)
	}
	if got := f.admin("POST", "/admin/reload", adm); got != http.StatusForbidden {
		t.Errorf("reload outside admin_from: %d", got)
	}
}

func TestStatus(t *testing.T) {
	f := start(t, configText("all", "Fake", `"127.0.0.1/32"`), Options{})
	call(t, f.session(), "load_connector", map[string]any{"name": "fake"})
	st := f.b.Status()
	if len(st.Connectors) != 1 || !st.Connectors[0].Loaded || st.Connectors[0].Tools == 0 || st.Connectors[0].PID == 0 {
		t.Fatalf("status: %+v", st.Connectors)
	}
	if !f.b.Unload("fake") || f.b.Status().Connectors[0].Loaded {
		t.Fatal("unload did not take")
	}
}
