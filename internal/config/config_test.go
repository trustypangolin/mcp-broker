package config

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/redact"
)

func TestExampleConfigLoads(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "examples", "broker.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "0.0.0.0:8765" {
		t.Errorf("listen = %q", cfg.Server.Listen)
	}
	if cfg.Approvals.Timeout != 120*time.Second {
		t.Errorf("timeout = %v", cfg.Approvals.Timeout)
	}
	if got := cfg.Roles["owner"].Classes["destructive"]; got != Approve {
		t.Errorf("owner destructive = %q", got)
	}
	if cfg.Connectors["comfy"].ToolClasses["partner_generate"] != Spend {
		t.Error("comfy partner_generate should be spend")
	}
	if !filepath.IsAbs(cfg.Audit.Path) {
		t.Errorf("audit path not resolved: %q", cfg.Audit.Path)
	}

	viewer := cfg.Roles["viewer"]
	if viewer.Sees("imap", cfg.Connectors["imap"].Tags) {
		t.Error("viewer must not see mail")
	}
	if !viewer.Sees("excel-live", cfg.Connectors["excel-live"].Tags) {
		t.Error("viewer should see excel-live")
	}
	if cfg.Roles["sandbox"].Sees("browser-control", cfg.Connectors["browser-control"].Tags) {
		t.Error("sandbox must not see the browser")
	}
	if cfg.Roles[AnonymousRole].Sees("excel-live", nil) {
		t.Error("anonymous must see nothing")
	}
}

func TestEmptyConfigDefaults(t *testing.T) {
	cfg, err := Parse(nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:8765" {
		t.Errorf("default listen = %q, want loopback", cfg.Server.Listen)
	}
	if !ContainsAddr(cfg.Server.AllowedSources, netip.MustParseAddr("127.0.0.1")) ||
		ContainsAddr(cfg.Server.AllowedSources, netip.MustParseAddr("192.168.1.5")) {
		t.Error("default allowed_sources should be loopback only")
	}
	if _, ok := cfg.Roles[AnonymousRole]; !ok {
		t.Error("anonymous role missing")
	}
}

func TestValidateReportsEverything(t *testing.T) {
	bad := `
server:
  listen: nope
agents:
  anonymous: {role: owner}
  vscode: {role: missing}
roles:
  owner:
    connectors: [ghost, "tag:nothing"]
    classes: {reading: allow}
    profiles: {launch: allow}
    rules:
      - {match: notool, action: allow}
connectors:
  a: {}
  b: {command: x, url: "http://h/mcp"}
  c:
    url: http://h/mcp
    headers: {Authorization: "Bearer literal"}
  d:
    command: x
    env: {API_KEY: hunter2hunter2}
`
	_, err := Parse([]byte(bad), t.TempDir())
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{
		"server.listen",
		"agents.anonymous",
		`agents.vscode.role: unknown role "missing"`,
		`unknown class "reading"`,
		`unknown profile action "launch"`,
		"rules[0].match",
		"connectors.a: needs command",
		"connectors.b: set command or url",
		"connectors.c.headers.Authorization",
		"connectors.d.env.API_KEY",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "literal") {
		t.Error("validation error leaked a secret value")
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	_, err := Parse([]byte("server:\n  lisen: 0.0.0.0:1\n"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "lisen") {
		t.Fatalf("typo not rejected: %v", err)
	}
}

func TestBadActionRejected(t *testing.T) {
	_, err := Parse([]byte("policy:\n  floors:\n    - {match: a/b, action: maybe}\n"), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "maybe") {
		t.Fatalf("bad action not rejected: %v", err)
	}
}

func TestStricter(t *testing.T) {
	if Stricter(Allow, Approve) != Approve || Stricter(Deny, Allow) != Deny || Stricter(Approve, Allow) != Approve {
		t.Error("Stricter ordering wrong")
	}
}

func TestReadEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("# comment\n\nexport A=1\nB = \"two words\"\nC='x=y'\n"), 0o600)
	env, err := ReadEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if env["A"] != "1" || env["B"] != "two words" || env["C"] != "x=y" {
		t.Errorf("env = %v", env)
	}

	os.WriteFile(p, []byte("novalue\n"), 0o600)
	if _, err := ReadEnvFile(p); err == nil {
		t.Error("expected parse error")
	}
}

func TestExpand(t *testing.T) {
	vars := map[string]string{"V": "1.2.3", "ROOT": `G:\Ai`}
	cases := map[string]string{
		"reset --hard ${V}": "reset --hard 1.2.3",
		`${ROOT}\venv`:      `G:\Ai\venv`,
		"cost $$5":          "cost $5",
		"plain $HOME":       "plain $HOME",
		"trailing $":        "trailing $",
	}
	for in, want := range cases {
		got, err := Expand(in, vars)
		if err != nil || got != want {
			t.Errorf("Expand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := Expand("${MISSING}", vars); err == nil {
		t.Error("undefined variable should fail")
	}
	if _, err := Expand("${OPEN", vars); err == nil {
		t.Error("unterminated should fail")
	}
}

func TestResolveRedactsSecrets(t *testing.T) {
	dir := t.TempDir()
	envFile := filepath.Join(dir, ".env")
	os.WriteFile(envFile, []byte("FAL_KEY=fal-secret-value\nCOMFYUI_VERSION=v0.3.40\nSTRAPI_TOKEN=strapi-tok-123\n"), 0o600)

	red := redact.New()
	p := Profile{
		Command: "python",
		Args:    []string{"main.py", "--version", "${COMFYUI_VERSION}"},
		EnvFile: envFile,
		Env:     map[string]string{"VIRTUAL_ENV": `G:\venv`},
	}
	r, err := p.Resolve(red)
	if err != nil {
		t.Fatal(err)
	}
	if r.Args[2] != "v0.3.40" {
		t.Errorf("args not expanded: %v", r.Args)
	}
	keys := strings.Join(r.EnvKeys(), ",")
	if keys != "COMFYUI_VERSION,FAL_KEY (redacted),STRAPI_TOKEN (redacted),VIRTUAL_ENV" {
		t.Errorf("EnvKeys = %s", keys)
	}
	if got := red.String("error: fal-secret-value rejected"); strings.Contains(got, "fal-secret") {
		t.Errorf("secret not redacted: %q", got)
	}

	c := Connector{URL: "https://h/mcp", EnvFile: envFile, Headers: map[string]string{"Authorization": "Bearer ${STRAPI_TOKEN}"}}
	rc, err := c.Resolve(red)
	if err != nil {
		t.Fatal(err)
	}
	if rc.Headers["Authorization"] != "Bearer strapi-tok-123" {
		t.Errorf("header = %q", rc.Headers["Authorization"])
	}
	if strings.Contains(red.String("sent strapi-tok-123"), "strapi-tok") {
		t.Error("header token not redacted")
	}
}

func TestRoleSeesLastMatchWins(t *testing.T) {
	r := Role{Connectors: []string{"tag:office", "!word-live", "comfy*"}}
	if !r.Sees("excel-live", []string{"office"}) || r.Sees("word-live", []string{"office"}) || !r.Sees("comfy", nil) || r.Sees("imap", []string{"mail"}) {
		t.Error("selector evaluation wrong")
	}
}

func TestDanglingSelectorsAreWarnings(t *testing.T) {
	cfg, err := Parse([]byte(`
agents: {a: {role: r}}
roles:
  r: {connectors: [ghost, "tag:nothing", "*"]}
connectors:
  x: {command: x}
`), t.TempDir())
	if err != nil {
		t.Fatalf("a role naming a removed connector must still load: %v", err)
	}
	var got []string
	for _, i := range cfg.Check() {
		if i.Warning && i.Where == "roles.r.connectors" {
			got = append(got, i.Msg)
		}
	}
	if len(got) != 2 {
		t.Fatalf("warnings: %v", got)
	}
}

func TestStarterIsValid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "broker.yaml")
	if err := WriteStarter(path); err != nil {
		t.Fatal(err)
	}
	if err := WriteStarter(path); err == nil {
		t.Fatal("starter overwrote an existing config")
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Listen != "127.0.0.1:8765" || len(cfg.Agents) == 0 || len(cfg.Connectors) != 0 {
		t.Fatalf("starter: listen %s, %d agents, %d connectors", cfg.Server.Listen, len(cfg.Agents), len(cfg.Connectors))
	}
	for _, i := range cfg.Check() {
		if !i.Warning {
			t.Errorf("starter check error: %s", i)
		}
	}
}
