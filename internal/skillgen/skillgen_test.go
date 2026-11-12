package skillgen

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/testutil/fakemcp"
)

func TestMain(m *testing.M) {
	fakemcp.MaybeRun()
	os.Exit(m.Run())
}

func sample() []Connector {
	schema := map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []any{"text"}}
	return []Connector{{
		Name: "word-live", Description: "Live edit of documents open in Word", Aliases: []string{"word"},
		Instructions: "Use get_text first.", Source: "live",
		Tools: []Tool{
			{Name: "get_text", Summary: "Get all text.", Description: "Get all text, paragraph by paragraph.", Required: []string{"text"}, Schema: schema, Class: "read"},
			{Name: "delete_text", Summary: "Delete text.", Description: "Delete text.", Schema: schema, Class: "destructive", Approval: true},
		},
		Direct: &Direct{Transport: "stdio", Command: "uv", Args: []string{"run", "server.py"}, Env: map[string]string{"KEY": "${KEY}"}, Vars: []string{"KEY"}},
	}}
}

func files(t *testing.T, conns []Connector, o Options) map[string]string {
	t.Helper()
	fs, err := Build(conns, o)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, f := range fs {
		if _, dup := m[f.Path]; dup {
			t.Errorf("%s written twice", f.Path)
		}
		m[f.Path] = string(f.Data)
		if strings.Contains(m[f.Path], "{{assets}}") || strings.Contains(m[f.Path], "{{table}}") {
			t.Errorf("%s has an unfilled placeholder", f.Path)
		}
	}
	return m
}

func TestLayouts(t *testing.T) {
	for _, c := range []struct {
		format string
		want   []string // paths that must exist
		main   string   // the file that holds the skill body
		link   string   // how it links a tool file
	}{
		{"claude", []string{".claude/skills/broker-word-live/SKILL.md", ".claude/skills/broker-word-live/tools/get_text.md", ".claude/skills/mcp-broker-api/SKILL.md"}, ".claude/skills/broker-word-live/SKILL.md", "(tools/get_text.md)"},
		{"codex", []string{".codex/skills/broker-word-live/SKILL.md", ".codex/skills/mcp-broker-api/SKILL.md"}, ".codex/skills/broker-word-live/SKILL.md", "(tools/get_text.md)"},
		{"cursor", []string{".cursor/rules/broker-word-live.mdc", "mcp-broker/broker-word-live/tools/get_text.md", ".cursor/rules/mcp-broker-api.mdc"}, ".cursor/rules/broker-word-live.mdc", "(mcp-broker/broker-word-live/tools/get_text.md)"},
		{"windsurf", []string{".windsurf/rules/broker-word-live.md", "mcp-broker/broker-word-live/tools/get_text.md"}, ".windsurf/rules/broker-word-live.md", "(mcp-broker/broker-word-live/tools/get_text.md)"},
		{"copilot", []string{".github/instructions/broker-word-live.instructions.md", "mcp-broker/broker-word-live/SKILL.md"}, "mcp-broker/broker-word-live/SKILL.md", "(mcp-broker/broker-word-live/tools/get_text.md)"},
		{"agents-md", []string{"AGENTS.md", "mcp-broker/broker-word-live/SKILL.md", "mcp-broker/mcp-broker-api/SKILL.md"}, "mcp-broker/broker-word-live/SKILL.md", "(mcp-broker/broker-word-live/tools/get_text.md)"},
		{"gemini", []string{"GEMINI.md", "mcp-broker/broker-word-live/SKILL.md"}, "mcp-broker/broker-word-live/SKILL.md", "(mcp-broker/broker-word-live/tools/get_text.md)"},
	} {
		m := files(t, sample(), Options{Format: c.format, Meta: true, Agent: "bot", APIBase: "http://office:8765/api/v1"})
		for _, p := range c.want {
			if _, ok := m[p]; !ok {
				t.Errorf("%s: no %s (have %v)", c.format, p, keys(m))
			}
		}
		body := m[c.main]
		for _, want := range []string{c.link, "${MCP_BROKER_API:-http://office:8765/api/v1}", "delete_text`](", "destructive, **approval**", "agent `bot`"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: %s lacks %q:\n%s", c.format, c.main, want, body)
			}
		}
		if !strings.Contains(m["README.md"], "| broker-word-live | word-live | 2 | live |") {
			t.Errorf("%s README:\n%s", c.format, m["README.md"])
		}
	}
}

func TestFrontmatter(t *testing.T) {
	m := files(t, sample(), Options{Format: "claude"})
	if s := m[".claude/skills/broker-word-live/SKILL.md"]; !strings.HasPrefix(s, "---\nname: broker-word-live\ndescription: \"Live edit of documents open in Word. Also called: word.") {
		t.Errorf("claude frontmatter:\n%s", s[:200])
	}
	m = files(t, sample(), Options{Format: "cursor"})
	if s := m[".cursor/rules/broker-word-live.mdc"]; !strings.HasPrefix(s, "---\ndescription: \"Live edit") || !strings.Contains(s, "alwaysApply: false\n---") {
		t.Errorf("cursor frontmatter:\n%s", s[:200])
	}
	m = files(t, sample(), Options{Format: "windsurf"})
	if s := m[".windsurf/rules/broker-word-live.md"]; !strings.HasPrefix(s, "---\ntrigger: model_decision\ndescription: ") {
		t.Errorf("windsurf frontmatter:\n%s", s[:200])
	}
	m = files(t, sample(), Options{Format: "copilot"})
	if s := m[".github/instructions/broker-word-live.instructions.md"]; !strings.HasPrefix(s, "---\napplyTo: \"**\"\ndescription: ") || !strings.Contains(s, "read `mcp-broker/broker-word-live/SKILL.md`") || len(s) > 1500 {
		t.Errorf("copilot instructions should be a short pointer:\n%s", s)
	}
	m = files(t, sample(), Options{Format: "agents-md", Meta: true})
	if s := m["AGENTS.md"]; !strings.Contains(s, "- **broker-word-live** (`mcp-broker/broker-word-live/SKILL.md`): Live edit") || !strings.Contains(s, "- **mcp-broker-api**") || len(s) > 3000 {
		t.Errorf("AGENTS.md should be a short index:\n%s", s)
	}
}

func TestUploadZips(t *testing.T) {
	m := files(t, sample(), Options{Format: "claude-upload", Meta: true})
	z := m["broker-word-live.zip"]
	zr, err := zip.NewReader(bytes.NewReader([]byte(z)), int64(len(z)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if !strings.Contains(strings.Join(names, " "), "broker-word-live/SKILL.md") || !strings.Contains(strings.Join(names, " "), "broker-word-live/tools/get_text.md") {
		t.Errorf("upload zip holds %v", names)
	}
	if _, ok := m["mcp-broker-api.zip"]; !ok {
		t.Error("no mcp-broker-api.zip")
	}
}

func TestBigTableMovesOutOfRules(t *testing.T) {
	c := sample()
	for i := range 200 {
		c[0].Tools = append(c[0].Tools, Tool{Name: "tool_" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+i/26)), Summary: "Does a thing to a document in a specific way.", Class: "write"})
	}
	m := files(t, c, Options{Format: "windsurf"})
	rule := m[".windsurf/rules/broker-word-live.md"]
	if !strings.Contains(rule, "mcp-broker/broker-word-live/TOOLS.md") || len(rule) > 6000 {
		t.Errorf("windsurf rule with a big table: %d bytes", len(rule))
	}
	if !strings.Contains(m["mcp-broker/broker-word-live/TOOLS.md"], "| [`get_text`](mcp-broker/broker-word-live/tools/get_text.md)") {
		t.Error("TOOLS.md missing or wrong links")
	}
	// A folder skill keeps its table inline: the agent loads SKILL.md only when it needs it.
	if m := files(t, c, Options{Format: "claude"}); !strings.Contains(m[".claude/skills/broker-word-live/SKILL.md"], "| [`get_text`](tools/get_text.md)") {
		t.Error("claude skill lost its inline table")
	}
}

func TestMetaSkill(t *testing.T) {
	m := files(t, nil, Options{Format: "claude", Meta: true, APIBase: "https://broker.corp:8765/api/v1"})
	s := m[".claude/skills/mcp-broker-api/SKILL.md"]
	if !strings.HasPrefix(s, "---\nname: mcp-broker-api\ndescription: \"Use the tools behind mcp-broker") {
		t.Errorf("meta frontmatter:\n%s", s[:300])
	}
	if strings.Contains(s, DefaultAPIBase) || !strings.Contains(s, "https://broker.corp:8765/api/v1") {
		t.Error("meta-skill not pointed at the chosen broker")
	}
	if strings.Count(s, "\n---\n") != 1 {
		t.Error("meta-skill has a second frontmatter block")
	}
	// direct skills have no broker to describe
	m = files(t, sample(), Options{Format: "claude", Meta: true, Target: TargetDirect})
	if _, ok := m[".claude/skills/mcp-broker-api/SKILL.md"]; ok {
		t.Error("direct skills include the broker meta-skill")
	}
}

func TestBrokerTokenHandling(t *testing.T) {
	local := files(t, sample(), Options{Format: "claude", TokenFile: "C:/Users/me/agent-tokens/bot.token"})
	if s := local[".claude/skills/broker-word-live/SKILL.md"]; !strings.Contains(s, "$(cat 'C:/Users/me/agent-tokens/bot.token')") {
		t.Error("local skill doesn't read the token file")
	}
	portable := files(t, sample(), Options{Format: "claude"})
	s := portable[".claude/skills/broker-word-live/SKILL.md"]
	if strings.Contains(s, "agent-tokens") || !strings.Contains(s, "TOKEN=\"$MCP_BROKER_TOKEN\"") || !strings.Contains(s, "ask the user for the token") {
		t.Errorf("portable skill token handling:\n%s", s)
	}
	if !strings.Contains(portable["README.md"], "Set `MCP_BROKER_TOKEN`") {
		t.Error("portable README doesn't say to set the token")
	}
}

func TestDirectSkill(t *testing.T) {
	m := files(t, sample(), Options{Format: "claude", Target: TargetDirect})
	dir := ".claude/skills/broker-word-live/"
	s := m[dir+"SKILL.md"]
	for _, want := range []string{
		"straight to the word-live server", "python scripts/mcp_call.py --server server.json --call TOOL",
		"`KEY`", "--steps steps.json", "relative to the folder this SKILL.md is in", "ask the user and wait for a yes",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("direct SKILL.md lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "MCP_BROKER") || strings.Contains(s, "$API") {
		t.Error("a direct skill mentions the broker API")
	}
	var srv map[string]any
	if err := json.Unmarshal([]byte(m[dir+"server.json"]), &srv); err != nil || srv["command"] != "uv" || srv["name"] != "word-live" || srv["transport"] != "stdio" {
		t.Errorf("server.json: %v %s", err, m[dir+"server.json"])
	}
	if !strings.HasPrefix(m[dir+"scripts/mcp_call.py"], "#!/usr/bin/env python3") || !strings.Contains(m[dir+"scripts/mcp_call.ps1"], "param(") {
		t.Error("scripts not bundled")
	}
	if tool := m[dir+"tools/get_text.md"]; !strings.Contains(tool, "--call get_text") || !strings.Contains(tool, "Example arguments: `{\"text\":\"<text>\"}`") {
		t.Errorf("direct tool doc:\n%s", tool)
	}
	// Rule formats write project-root paths into the commands.
	m = files(t, sample(), Options{Format: "cursor", Target: TargetDirect})
	if s := m[".cursor/rules/broker-word-live.mdc"]; !strings.Contains(s, "python mcp-broker/broker-word-live/scripts/mcp_call.py --server mcp-broker/broker-word-live/server.json") {
		t.Errorf("cursor direct rule paths:\n%s", s)
	}
	// An OAuth server can't be reached by the bundled client: say so.
	c := sample()
	c[0].Direct.OAuth = true
	m = files(t, c, Options{Format: "claude", Target: TargetDirect})
	if !strings.Contains(m[dir+"SKILL.md"], "needs an OAuth sign-in") || !strings.Contains(m["README.md"], "needs an OAuth sign-in") {
		t.Error("OAuth not flagged")
	}
}

func TestBadOptions(t *testing.T) {
	if _, err := Build(nil, Options{Format: "nope"}); err == nil {
		t.Error("unknown format accepted")
	}
	if _, err := Build(nil, Options{Target: "elsewhere"}); err == nil {
		t.Error("unknown target accepted")
	}
}

func TestNames(t *testing.T) {
	used := map[string]bool{}
	for in, want := range map[string]string{"word-live": "broker-word-live", "aws-Ai-RA-BBAD-admin": "broker-aws-ai-ra-bbad-admin", "strapi_tshirt": "broker-strapi-tshirt", "..": "broker-connector"} {
		if got := SkillName(in, used); got != want {
			t.Errorf("SkillName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SkillName("Word Live", used); got != "broker-word-live-2" {
		t.Errorf("clash: %q", got)
	}
	tf := map[string]bool{}
	for _, c := range [][2]string{{"get_echo", "tools/get_echo.md"}, {"Get_Echo", "tools/Get_Echo_2.md"}, {"a/b:c", "tools/a_b_c.md"}, {"..", "tools/tool.md"}} {
		if got := ToolFile(c[0], tf); got != c[1] {
			t.Errorf("ToolFile(%q) = %q, want %q", c[0], got, c[1])
		}
	}
	if refs := VarRefs("a ${X} $${NOT} ${Y}\\z"); strings.Join(refs, ",") != "X,Y" {
		t.Errorf("VarRefs: %v", refs)
	}
}

// TestBundledClients runs mcp_call.py (and on Windows mcp_call.ps1) against a real MCP
// server over stdio: the fake server in this test binary.
func TestBundledClients(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"mcp_call.py", "mcp_call.ps1"} {
		os.WriteFile(filepath.Join(dir, name), script("scripts/"+name), 0o644)
	}
	env := map[string]string{}
	for k, v := range fakemcp.Env(fakemcp.ModeLegacy) {
		env[k] = v
	}
	env["GREETING_FROM"] = "${WHO}"
	srv, _ := json.Marshal(map[string]any{"name": "fake", "transport": "stdio", "command": os.Args[0], "env": env})
	os.WriteFile(filepath.Join(dir, "server.json"), srv, 0o644)
	os.WriteFile(filepath.Join(dir, "steps.json"), []byte(`[{"tool":"get_echo","arguments":{"text":"one"}},{"tool":"get_echo","arguments":{"text":"tw\u00f6"}}]`), 0o644)
	os.WriteFile(filepath.Join(dir, "args.json"), []byte(`{"text":"caf\u00e9 \u2026"}`), 0o644)

	type runner struct {
		name string
		cmd  func(args ...string) *exec.Cmd
	}
	var runners []runner
	for _, py := range []string{"python", "python3"} {
		if runtime.GOOS == "windows" && py == "python3" {
			continue // the Microsoft Store stub hangs
		}
		if p, err := exec.LookPath(py); err == nil {
			runners = append(runners, runner{"python", func(a ...string) *exec.Cmd {
				return exec.Command(p, append([]string{filepath.Join(dir, "mcp_call.py")}, a...)...)
			}})
			break
		}
	}
	if runtime.GOOS == "windows" {
		if p, err := exec.LookPath("powershell"); err == nil {
			runners = append(runners, runner{"powershell", func(a ...string) *exec.Cmd {
				// PowerShell takes -Name value; map the Python flags.
				m := map[string]string{"--server": "-Server", "--call": "-Call", "--args": "-ArgsJson", "--steps": "-Steps", "--list": "-List"}
				args := []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-File", filepath.Join(dir, "mcp_call.ps1")}
				for _, x := range a {
					if f, ok := m[x]; ok {
						x = f
					}
					args = append(args, x)
				}
				return exec.Command(p, args...)
			}})
		}
	}
	if len(runners) == 0 {
		t.Skip("neither python nor powershell is available")
	}
	server := filepath.Join(dir, "server.json")
	for _, r := range runners {
		run := func(want int, a ...string) string {
			t.Helper()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			c := r.cmd(a...)
			c.Env = append(os.Environ(), "WHO=test")
			cmd := exec.CommandContext(ctx, c.Path, c.Args[1:]...)
			cmd.Env = c.Env
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			code := 0
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if err != nil {
				t.Fatalf("%s %v: %v", r.name, a, err)
			}
			if code != want {
				t.Errorf("%s %v: exit %d, want %d\nstdout: %s\nstderr: %s", r.name, a, code, want, stdout.String(), stderr.String())
			}
			return stdout.String() + stderr.String()
		}
		if out := run(0, "--server", server, "--list"); !strings.Contains(out, `"get_echo"`) {
			t.Errorf("%s list: %s", r.name, out)
		}
		if out := run(0, "--server", server, "--call", "get_echo", "--args", "@"+filepath.Join(dir, "args.json")); !strings.Contains(out, "echo: café …") {
			t.Errorf("%s call: %s", r.name, out)
		}
		if out := run(0, "--server", server, "--steps", filepath.Join(dir, "steps.json")); !strings.Contains(out, "echo: one") || !strings.Contains(out, "echo: twö") {
			t.Errorf("%s steps: %s", r.name, out)
		}
		if out := run(2, "--server", server, "--call", "get_failure", "--args", "@"+filepath.Join(dir, "args.json")); !strings.Contains(out, "could not do") {
			t.Errorf("%s tool error: %s", r.name, out)
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
