package clientcfg

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const exe = `C:\Program Files\mcp-broker\mcp-broker.exe`

func TestRenderEveryHarness(t *testing.T) {
	for _, h := range Harnesses() {
		for _, http := range []bool{false, true} {
			s, err := Render(h, Target{Exe: exe, URL: "http://127.0.0.1:8765/mcp", HTTP: http})
			if http && !h.HTTP {
				if err == nil {
					t.Errorf("%s: HTTP snippet for a stdio-only client", h.ID)
				}
				continue
			}
			if err != nil {
				t.Fatalf("%s http=%v: %v", h.ID, http, err)
			}
			if s.Agent != h.Agent {
				t.Errorf("%s: agent %q", h.ID, s.Agent)
			}
			if s.Format == "json" {
				var v map[string]any
				if err := json.Unmarshal([]byte(s.Text), &v); err != nil {
					t.Fatalf("%s http=%v: invalid JSON: %v\n%s", h.ID, http, err, s.Text)
				}
			}
			want := "connect --agent " + h.Agent
			if http {
				want = "127.0.0.1:8765/mcp"
			}
			flat := strings.NewReplacer(`"`, "", "'", "", ",", "", "\n", " ").Replace(s.Text)
			if !strings.Contains(strings.Join(strings.Fields(flat), " "), want) && !strings.Contains(s.Text, want) {
				t.Errorf("%s http=%v: %q missing\n%s", h.ID, http, want, s.Text)
			}
			if http && !strings.Contains(s.Text, TokenEnv) && !strings.Contains(s.Text, "${input:") {
				t.Errorf("%s: HTTP snippet doesn't read the token from the environment\n%s", h.ID, s.Text)
			}
		}
	}
}

func TestRenderShapes(t *testing.T) {
	get := func(id string, http bool) Snippet {
		h, _ := Find(id)
		s, err := Render(h, Target{Exe: exe, Agent: "dev", URL: "http://127.0.0.1:8765/mcp", HTTP: http})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	var vs struct {
		Servers map[string]struct {
			Type, Command string
			Args          []string
		}
	}
	json.Unmarshal([]byte(get("vscode", false).Text), &vs)
	if b := vs.Servers[ServerName]; b.Type != "stdio" || b.Command != exe || strings.Join(b.Args, " ") != "connect --agent dev" {
		t.Errorf("vscode = %+v", vs)
	}
	if s := get("gemini", true).Text; !strings.Contains(s, `"httpUrl"`) {
		t.Errorf("gemini http uses httpUrl:\n%s", s)
	}
	if s := get("windsurf", true).Text; !strings.Contains(s, `"serverUrl"`) || !strings.Contains(s, "${env:"+TokenEnv+"}") {
		t.Errorf("windsurf http:\n%s", s)
	}
	codex := get("codex", false)
	if codex.Format != "toml" || !strings.Contains(codex.Text, `command = '`+exe+`'`) || !strings.Contains(codex.Text, `args = ['connect', '--agent', 'dev']`) {
		t.Errorf("codex:\n%s", codex.Text)
	}
	if s := get("codex", true).Text; !strings.Contains(s, "bearer_token_env_var = '"+TokenEnv+"'") {
		t.Errorf("codex http:\n%s", s)
	}
	if c := get("claude-code", false).Command; c != `claude mcp add --scope user broker -- "`+exe+`" connect --agent dev` {
		t.Errorf("claude command = %s", c)
	}
	if !strings.Contains(get("vscode", true).Text, `"password": true`) {
		t.Error("vscode token input isn't a password")
	}
}

func TestShimNextTo(t *testing.T) {
	// The dashboard runs in the tray; snippets must name the CLI next to it.
	for in, want := range map[string]string{
		filepath.Join("dist", "mcp-broker-tray.exe"): filepath.Join("dist", "mcp-broker.exe"),
		filepath.Join("dist", "mcp-broker.exe"):      filepath.Join("dist", "mcp-broker.exe"),
		filepath.Join("bin", "mcp-broker-tray"):      filepath.Join("bin", "mcp-broker"),
	} {
		if got := ShimNextTo(in); got != want {
			t.Errorf("ShimNextTo(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestWSLPath(t *testing.T) {
	if got := WSLPath(`G:\Ai\MCP\mcp-broker\dist\mcp-broker.exe`); got != "/mnt/g/Ai/MCP/mcp-broker/dist/mcp-broker.exe" {
		t.Error(got)
	}
	if got := WSLPath("/usr/bin/x"); got != "/usr/bin/x" {
		t.Error(got)
	}
}
