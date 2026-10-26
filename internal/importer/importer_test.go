package importer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/redact"
)

const desktopJSON = `{
  "globalShortcut": "Ctrl+Space",
  "mcpServers": {
    "imap": {"command": "node", "args": ["C:\\imap\\index.js"], "env": {"IMAP_PASSWORD": "pa$$word", "IMAP_HOST": "mail.example"}},
    "excel-live": {"command": "uv", "args": ["--directory", "C:/x", "run", "excel.py"]},
    "remote": {"type": "http", "url": "https://h.example/mcp", "headers": {"Authorization": "Bearer abc123"}},
    "old": {"type": "sse", "url": "https://h.example/sse"},
    "broker": {"command": "C:\\Tools\\mcp-broker.exe", "args": ["connect", "--agent", "claude-desktop"]},
    "envref": {"command": "tool", "env": {"GITHUB_TOKEN": "${GH_TOKEN}"}}
  },
  "preferences": {"x": 1}
}`

const brokerYAML = `agents:
  claude-desktop: {role: owner}
  lan: {role: viewer}
roles:
  owner: {connectors: ["*"], classes: {read: allow}}
  viewer: {connectors: ["*", "!tag:mail"], classes: {read: allow}}
connectors:
  excel-live:
    command: uv
    args: [--directory, C:/x, run, excel.py]
`

type fixture struct {
	dir, desktop string
	cfg          *config.Config
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{dir: dir, desktop: filepath.Join(dir, "claude_desktop_config.json")}
	os.WriteFile(f.desktop, []byte(desktopJSON), 0o600)
	path := filepath.Join(dir, "broker.yaml")
	os.WriteFile(path, []byte(brokerYAML), 0o600)
	var err error
	if f.cfg, err = config.Load(path); err != nil {
		t.Fatal(err)
	}
	return f
}

func getenv(name string) string {
	return map[string]string{"GH_TOKEN": "gh-secret"}[name]
}

func (f *fixture) plan(t *testing.T, o Options) map[string]Item {
	t.Helper()
	servers, err := Read(SourceFor(f.desktop))
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{servers[0].Name, servers[1].Name}; got[0] != "imap" || got[1] != "excel-live" {
		t.Fatalf("file order lost: %v", got)
	}
	o.Getenv = getenv
	out := map[string]Item{}
	for _, it := range Plan(f.cfg, servers, o) {
		out[it.Name] = it
	}
	return out
}

func TestPlan(t *testing.T) {
	f := setup(t)
	items := f.plan(t, Options{})

	imap := items["imap"]
	if !imap.Imported() || imap.Entry.Env["IMAP_HOST"] != "mail.example" || imap.Entry.Env["IMAP_PASSWORD"] != "" {
		t.Errorf("imap env: %+v", imap.Entry.Env)
	}
	if imap.Secrets["IMAP_PASSWORD"] != "pa$$$$word" { // escaped for the broker's expansion
		t.Errorf("imap secret: %q", imap.Secrets["IMAP_PASSWORD"])
	}
	if !slices.Contains(imap.Entry.Tags, "mail") || slices.Contains(imap.Visible, "lan") {
		t.Errorf("imap should be tagged mail and hidden from lan: tags %v, visible %v", imap.Entry.Tags, imap.Visible)
	}
	if !items["excel-live"].Existing {
		t.Error("identical excel-live should be recognised as already present")
	}
	if r := items["remote"]; r.Entry.Headers["Authorization"] != "Bearer ${HEADER_AUTHORIZATION}" || r.Secrets["HEADER_AUTHORIZATION"] != "abc123" {
		t.Errorf("remote headers: %v / %v", r.Entry.Headers, r.Secrets)
	}
	if items["old"].Skip == "" || items["broker"].Skip == "" {
		t.Error("sse and the shim must be skipped")
	}
	if e := items["envref"]; e.Secrets["GITHUB_TOKEN"] != "${GH_TOKEN}" || e.Secrets["GH_TOKEN"] != "gh-secret" {
		t.Errorf("env reference not carried: %v", e.Secrets)
	}
}

func TestPlanConflictAndOverwrite(t *testing.T) {
	f := setup(t)
	cn := f.cfg.Connectors["excel-live"]
	cn.Args = []string{"other"}
	f.cfg.Connectors["excel-live"] = cn
	if it := f.plan(t, Options{})["excel-live"]; it.Skip == "" {
		t.Fatal("a different existing connector must not be replaced without --overwrite")
	}
	if it := f.plan(t, Options{Overwrite: true})["excel-live"]; !it.Imported() {
		t.Fatalf("overwrite: %+v", it)
	}
}

// The values a connector runs with after import must be exactly what the client config
// gave it, secrets included, even though they now come from an env file.
func TestApplyRoundTrip(t *testing.T) {
	f := setup(t)
	servers, _ := Read(SourceFor(f.desktop))
	items := Plan(f.cfg, servers, Options{Getenv: getenv})
	n, err := Apply(f.cfg, items)
	if err != nil || n != 3 {
		t.Fatalf("apply: %d, %v", n, err)
	}
	cfg, err := config.Load(f.cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(cfg.Path)
	for _, secret := range []string{"pa$$word", "abc123", "gh-secret"} {
		if strings.Contains(string(text), secret) {
			t.Errorf("secret %q written to broker.yaml", secret)
		}
	}
	r, err := cfg.Connectors["imap"].Resolve(redact.New())
	if err != nil {
		t.Fatal(err)
	}
	if r.Env["IMAP_PASSWORD"] != "pa$$word" || r.Env["IMAP_HOST"] != "mail.example" || r.Args[0] != `C:\imap\index.js` {
		t.Errorf("imap resolved to %v %v", r.Env, r.Args)
	}
	if r, _ := cfg.Connectors["remote"].Resolve(redact.New()); r.Headers["Authorization"] != "Bearer abc123" {
		t.Errorf("remote header resolved to %q", r.Headers["Authorization"])
	}
	if r, _ := cfg.Connectors["envref"].Resolve(redact.New()); r.Env["GITHUB_TOKEN"] != "gh-secret" {
		t.Errorf("envref resolved to %q", r.Env["GITHUB_TOKEN"])
	}
	if fi, _ := os.Stat(items[0].EnvFile); fi == nil {
		t.Error("env file missing")
	}

	// A second run finds everything already there.
	servers, _ = Read(SourceFor(f.desktop))
	for _, it := range Plan(cfg, servers, Options{Getenv: getenv}) {
		if it.Imported() {
			t.Errorf("%s imported twice", it.Name)
		}
	}
}

func TestSwitchRewritesClientConfig(t *testing.T) {
	f := setup(t)
	src := SourceFor(f.desktop)
	servers, _ := Read(src)
	items := Plan(f.cfg, servers, Options{Getenv: getenv})
	res, err := Switch(src, items, `C:\Tools\mcp-broker.exe`)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(res.Backup); string(b) != desktopJSON {
		t.Fatal("backup is not the original")
	}
	data, _ := os.ReadFile(f.desktop)
	var got struct {
		GlobalShortcut string
		MCPServers     map[string]struct {
			Command string
			Args    []string
		}
		Preferences map[string]int
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.GlobalShortcut != "Ctrl+Space" || got.Preferences["x"] != 1 {
		t.Error("other settings lost")
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for k := range got.MCPServers {
			if !yield(k) {
				return
			}
		}
	})
	// old (SSE) stays: the broker cannot serve it, so the client keeps it.
	if !slices.Equal(names, []string{"broker", "old"}) {
		t.Fatalf("servers after switch: %v", names)
	}
	if b := got.MCPServers["broker"]; b.Command != `C:\Tools\mcp-broker.exe` || strings.Join(b.Args, " ") != "connect --agent claude-desktop" {
		t.Errorf("broker entry: %+v", b)
	}
	if strings.Index(string(data), "globalShortcut") > strings.Index(string(data), "preferences") {
		t.Error("key order not kept")
	}
}

func TestSwitchClaudeCodeUserConfigGivesCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("USERPROFILE", home)
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".claude.json")
	os.WriteFile(path, []byte(`{"mcpServers": {"word-live": {"command": "uv"}}, "projects": {}}`), 0o600)
	src := SourceFor(path)
	servers, _ := Read(src)
	f := setup(t)
	res, err := Switch(src, Plan(f.cfg, servers, Options{}), "mcp-broker.exe")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); !strings.Contains(string(b), "word-live") || res.Backup != "" {
		t.Fatal("~/.claude.json must not be edited")
	}
	if len(res.Commands) != 2 || res.Commands[0] != "claude mcp remove -s user word-live" {
		t.Fatalf("commands: %v", res.Commands)
	}
}

func TestReadVSCodeAndDuplicates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "User")
	os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, "mcp.json")
	os.WriteFile(path, []byte(`{"servers": {"a": {"type": "stdio", "command": "x"}, "b": {"type": "http", "url": "http://h/mcp"}, "a": {"type": "stdio", "command": "y"}}}`), 0o600)
	src := SourceFor(path)
	if src.Client != VSCode {
		t.Fatalf("client: %s", src.Client)
	}
	servers, err := Read(src)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 || servers[0].Command != "y" || servers[1].Type != "http" {
		t.Fatalf("servers: %+v", servers)
	}
}

func TestBridgeBecomesRemoteConnector(t *testing.T) {
	f := setup(t)
	s := Server{Name: "tools", Source: SourceFor(f.desktop), Type: "stdio", Command: "npx",
		Args: []string{"-y", "mcp-remote", "http://127.0.0.1:27200/mcp", "--header", "Authorization: Bearer s3cret", "--allow-http"}}
	it := Plan(f.cfg, []Server{s}, Options{Getenv: getenv})[0]
	if it.Entry.URL != "http://127.0.0.1:27200/mcp" || it.Entry.Command != "" {
		t.Fatalf("entry: %+v", it.Entry)
	}
	if it.Entry.Headers["Authorization"] != "Bearer ${HEADER_AUTHORIZATION}" || it.Secrets["HEADER_AUTHORIZATION"] != "s3cret" {
		t.Fatalf("headers: %v %v", it.Entry.Headers, it.Secrets)
	}

	// An option the conversion does not understand leaves the bridge as it is, with a
	// warning about the credential on its command line.
	s.Args = append(s.Args, "--debug")
	it = Plan(f.cfg, []Server{s}, Options{Getenv: getenv})[0]
	if it.Entry.Command != "npx" || len(it.Notes) == 0 {
		t.Fatalf("unknown bridge option: %+v", it)
	}
}

func TestSameServerUnderAnotherName(t *testing.T) {
	f := setup(t)
	s := Server{Name: "excel", Source: SourceFor(f.desktop), Type: "stdio", Command: "uv", Args: []string{"--directory", "C:/x", "run", "excel.py"}}
	it := Plan(f.cfg, []Server{s}, Options{})[0]
	if !it.Existing || it.Name != "excel-live" {
		t.Fatalf("got %+v", it)
	}
}
