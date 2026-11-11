package importer

import (
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/trustypangolin/mcp-broker/internal/config"
)

func parse(t *testing.T, name, text string) ([]Server, Source) {
	t.Helper()
	s, src, err := Parse(SourceFor(name), []byte(text))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return s, src
}

func byName(servers []Server) map[string]Server {
	out := map[string]Server{}
	for _, s := range servers {
		out[s.Scope+"/"+s.Name] = s
	}
	return out
}

func TestParseFormats(t *testing.T) {
	// Codex TOML: stdio, url with a bearer env var, disabled.
	s, src := parse(t, "config.toml", `
model = "x"
[mcp_servers.docs]
command = 'C:\tools\docs.exe'
args = ["serve", "--port", "1"]
[mcp_servers.docs.env]
LEVEL = "debug"
[mcp_servers.gh]
url = "https://api.example/mcp"
bearer_token_env_var = "GH_PAT"
[mcp_servers.off]
command = "x"
enabled = false
`)
	m := byName(s)
	if src.Client != Codex || len(s) != 3 || s[0].Name != "docs" {
		t.Fatalf("codex: %v %+v", src.Client, s)
	}
	if d := m["/docs"]; d.Command != `C:\tools\docs.exe` || d.Env["LEVEL"] != "debug" || len(d.Args) != 3 {
		t.Errorf("codex docs: %+v", d)
	}
	if g := m["/gh"]; g.Type != "http" || g.Headers["Authorization"] != "Bearer ${GH_PAT}" {
		t.Errorf("codex gh: %+v", g)
	}
	if !m["/off"].Disabled {
		t.Error("codex enabled=false not read as disabled")
	}

	// Gemini httpUrl, Windsurf serverUrl.
	s, _ = parse(t, ".gemini/settings.json", `{"mcpServers": {"a": {"httpUrl": "https://a/mcp", "headers": {"X": "1"}}}}`)
	if s[0].URL != "https://a/mcp" || s[0].Type != "http" {
		t.Errorf("gemini: %+v", s[0])
	}
	s, src = parse(t, "mcp_config.json", `{"mcpServers": {"w": {"serverUrl": "https://w/mcp"}}}`)
	if s[0].URL != "https://w/mcp" || src.Client != Windsurf {
		t.Errorf("windsurf: %+v %s", s[0], src.Client)
	}

	// VS Code JSONC: comments, trailing commas, a // inside a string.
	s, src = parse(t, ".vscode/mcp.json", `{
  // my servers
  "servers": {
    "web": {"type": "http", "url": "https://x.example//mcp", /* note */ },
    "local": {"command": "node", "args": ["a.js",],},
  },
}`)
	if src.Client != VSCode || len(s) != 2 || s[0].URL != "https://x.example//mcp" || s[1].Args[0] != "a.js" {
		t.Errorf("jsonc: %+v", s)
	}

	// VS Code settings.json with an mcp block.
	s, src = parse(t, "settings.json", `{"editor.fontSize": 12, "mcp": {"servers": {"s": {"command": "x"}}}}`)
	if src.Client != VSCode || len(s) != 1 {
		t.Errorf("vscode settings: %+v %v", s, src)
	}

	// ~/.claude.json: user servers and each project's.
	s, src = parse(t, ".claude.json", `{"mcpServers": {"u": {"command": "u"}},
  "projects": {"G:\\work\\alpha": {"mcpServers": {"db": {"command": "db", "env": {"DB": "alpha"}}}},
               "G:\\work\\beta": {"mcpServers": {}}, "G:\\work\\gamma": {"allowedTools": []}}}`)
	m = byName(s)
	if src.Client != ClaudeCode || len(s) != 2 || m[`G:\work\alpha/db`].Env["DB"] != "alpha" {
		t.Errorf("claude.json: %+v", s)
	}

	// A pasted servers object.
	s, _ = parse(t, "pasted", `{"one": {"command": "a"}, "two": {"url": "https://b/mcp"}}`)
	if len(s) != 2 || s[1].Type != "http" {
		t.Errorf("bare: %+v", s)
	}

	if _, _, err := Parse(SourceFor("x.json"), []byte(`{"name": "not mcp"}`)); err == nil || !strings.Contains(err.Error(), "no MCP servers") {
		t.Errorf("unrelated JSON: %v", err)
	}
}

const variantYAML = `agents:
  owner-agent: {role: owner}
roles:
  owner: {connectors: ["*"], classes: {read: allow}}
connectors:
  aws:
    tags: [aws]
    command: uvx
    args: [mcp-proxy-for-aws, https://aws.example/mcp, --profile, luton-shared]
  imap:
    group: Mail
    command: node
    args: [imap.js]
    env: {IMAP_HOST: mail.one}
`

func variantConfig(t *testing.T) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "broker.yaml")
	os.WriteFile(path, []byte(variantYAML), 0o600)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func planText(t *testing.T, cfg *config.Config, name, text string) map[string]Item {
	t.Helper()
	servers, _ := parse(t, name, text)
	out := map[string]Item{}
	for _, it := range Plan(cfg, servers, Options{Getenv: func(string) string { return "" }}) {
		out[it.Name] = it
	}
	return out
}

func TestVariantsOfExisting(t *testing.T) {
	cfg := variantConfig(t)
	items := planText(t, cfg, ".mcp.json", `{"mcpServers": {
  "aws": {"command": "uvx", "args": ["mcp-proxy-for-aws", "https://aws.example/mcp", "--profile", "prod"]},
  "aws-same": {"command": "uvx", "args": ["mcp-proxy-for-aws", "https://aws.example/mcp", "--profile", "luton-shared"]},
  "imap": {"command": "node", "args": ["imap.js"], "env": {"IMAP_HOST": "mail.two"}},
  "other": {"command": "uvx", "args": ["some-other-server"]}
}}`)

	v := items["aws-prod"]
	if v.VariantOf != "aws" || v.Entry.Group != "aws" || !v.GroupExisting || !slices.Equal(v.Entry.Tags, []string{"aws"}) {
		t.Errorf("--profile variant: %+v", v)
	}
	if len(v.Diffs) != 1 || !strings.Contains(v.Diffs[0], "--profile prod (existing: luton-shared)") {
		t.Errorf("diffs: %v", v.Diffs)
	}
	if same := items["aws"]; !same.Existing || same.Server.Name != "aws-same" {
		t.Errorf("identical under another name: %+v", same)
	}
	iv := items["imap-mail.two"]
	if iv.VariantOf != "imap" || iv.Entry.Group != "Mail" || iv.GroupExisting {
		t.Errorf("env variant joins the existing group: %+v", iv)
	}
	if !strings.Contains(strings.Join(iv.Diffs, ";"), "IMAP_HOST=mail.two (existing: mail.one)") {
		t.Errorf("env diff: %v", iv.Diffs)
	}
	if o := items["other"]; !o.Imported() || o.VariantOf != "" {
		t.Errorf("another package under uvx is a new server: %+v", o)
	}

	// Applying files the ungrouped aws under the variant's group.
	var list []Item
	for _, it := range items {
		list = append(list, it)
	}
	if _, err := Apply(cfg, list); err != nil {
		t.Fatal(err)
	}
	after, err := config.Load(cfg.Path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Connectors["aws"].Group != "aws" || after.Connectors["aws-prod"].Group != "aws" || after.Connectors["imap"].Group != "Mail" {
		t.Errorf("groups: aws=%q aws-prod=%q imap=%q", after.Connectors["aws"].Group, after.Connectors["aws-prod"].Group, after.Connectors["imap"].Group)
	}
}

// A client config that sets less than the broker's connector is the same server: the
// broker's was set up more fully.
func TestSparserClientIsTheSameServer(t *testing.T) {
	cfg := variantConfig(t)
	items := planText(t, cfg, ".mcp.json", `{"mcpServers": {"imap": {"command": "node", "args": ["imap.js"]}}}`)
	it := items["imap"]
	if !it.Existing || len(it.Diffs) != 1 || !strings.Contains(it.Diffs[0], "also sets IMAP_HOST") {
		t.Errorf("sparser client: %+v", it)
	}
}

// A client naming the program bare (uvx) runs the same server as a connector naming it by
// its full path.
func TestBareCommandMatchesFullPath(t *testing.T) {
	bin := t.TempDir()
	exe := filepath.Join(bin, "tool")
	if runtime.GOOS == "windows" {
		exe += ".exe"
	}
	os.WriteFile(exe, []byte("x"), 0o755)
	t.Setenv("PATH", bin)
	path := filepath.Join(t.TempDir(), "broker.yaml")
	os.WriteFile(path, []byte("connectors:\n  tool:\n    command: '"+exe+"'\n    args: [--profile, dev]\n"), 0o600)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	items := planText(t, cfg, ".mcp.json", `{"mcpServers": {"tool": {"command": "tool", "args": ["--profile", "prod"]}}}`)
	if it := items["tool-prod"]; it.VariantOf != "tool" {
		t.Errorf("bare command: %v", slices.Collect(maps.Keys(items)))
	}
}

// Paths from a Windows config split the same on every OS.
func TestPathsFromAnyOS(t *testing.T) {
	for in, want := range map[string]string{`G:\src\imap-mcp-server`: "imap-mcp-server", "/home/a/proj/": "proj", `C:\x/y\z`: "z", "plain": "plain"} {
		if got := lastElem(in); got != want {
			t.Errorf("lastElem(%q) = %q, want %q", in, got, want)
		}
	}
	if c := SourceFor(`C:\work\app\.vscode\mcp.json`).Client; c != VSCode {
		t.Errorf("Windows VS Code path read as %s", c)
	}
	if c := SourceFor(`C:\Users\a\.cursor\mcp.json`).Client; c != Cursor {
		t.Errorf("Windows Cursor path read as %s", c)
	}
}

func TestRenameUsesProjectName(t *testing.T) {
	cfg := variantConfig(t)
	items := planText(t, cfg, ".claude.json", `{"projects": {"G:\\src\\imap-mcp-server": {"mcpServers": {"imap": {"command": "npx", "args": ["imap-mcp-server"]}}}}}`)
	if it, ok := items["imap-mcp-server"]; !ok || !it.Imported() {
		t.Errorf("want imap-mcp-server, got %v", slices.Collect(maps.Keys(items)))
	}
}

func TestProjectsInOneFile(t *testing.T) {
	cfg := variantConfig(t)
	items := planText(t, cfg, ".claude.json", `{"projects": {
  "G:\\work\\alpha": {"mcpServers": {
    "db": {"command": "dbserver", "env": {"DB_NAME": "alpha"}},
    "lint": {"command": "linter"},
    "notes": {"command": "notes-a"}}},
  "G:\\work\\beta": {"mcpServers": {
    "db": {"command": "dbserver", "env": {"DB_NAME": "beta"}},
    "lint": {"command": "linter"},
    "notes": {"command": "notes-b"}}}}}`)

	if a, b := items["db"], items["db-beta"]; !a.Imported() || b.VariantOf != "db" || a.Entry.Group != "db" || b.Entry.Group != "db" {
		t.Errorf("db variants: %+v / %+v", a, b)
	}
	if l := items["lint"]; len(l.Also) != 1 {
		t.Errorf("identical lint in both projects is one connector: %+v", l)
	}
	if _, ok := items["lint-beta"]; ok {
		t.Error("identical server imported twice")
	}
	n := items["notes-beta"]
	if !n.Imported() || !strings.Contains(strings.Join(n.Notes, " "), "renamed") || n.Entry.Description != "" {
		t.Errorf("unrelated same name gets the project's name: %+v", n)
	}
}

func TestChoose(t *testing.T) {
	cfg := variantConfig(t)
	servers, _ := parse(t, ".mcp.json", `{"mcpServers": {"a": {"command": "a"}, "b": {"command": "b", "env": {"API_KEY": "k"}}, "c": {"command": "c"}}}`)
	items := Plan(cfg, servers, Options{Getenv: func(string) string { return "" }})
	id := func(name string) string {
		i := slices.IndexFunc(items, func(it Item) bool { return it.Server.Name == name })
		return items[i].ID()
	}
	group := "Tools"
	if err := Choose(cfg, items, map[string]Choice{
		id("a"): {Skip: true},
		id("b"): {Name: "bee", Tags: []string{"office", " ", "office"}, Group: &group},
	}); err != nil {
		t.Fatal(err)
	}
	if items[0].Imported() {
		t.Error("left-out item still imported")
	}
	if b := items[1]; b.Name != "bee" || !strings.HasSuffix(b.EnvFile, "bee.env") || b.Entry.EnvFile != b.EnvFile ||
		!slices.Equal(b.Entry.Tags, []string{"office"}) || b.Entry.Group != "Tools" {
		t.Errorf("edited: %+v", b)
	}
	if err := Choose(cfg, items, map[string]Choice{id("c"): {Name: "bee"}}); err == nil {
		t.Error("two items with one name")
	}
	if err := Choose(cfg, items, map[string]Choice{id("c"): {Name: "imap"}}); err == nil {
		t.Error("renamed onto an existing connector")
	}
	if err := Choose(cfg, items, map[string]Choice{id("c"): {Name: "bad name"}}); err == nil {
		t.Error("invalid name accepted")
	}
}
