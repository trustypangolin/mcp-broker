// Package clientcfg renders the snippet each MCP client (agent harness) needs to reach the
// broker: the stdio shim (mcp-broker connect) or, for clients that speak Streamable HTTP,
// the broker URL with a bearer token read from the environment. Snippets never contain a
// token: the shim reads agent-tokens/<agent>.token, and HTTP snippets reference
// $MCP_BROKER_TOKEN in the client's own interpolation syntax.
package clientcfg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ServerName is the key the broker is registered under in every client.
const ServerName = "broker"

// TokenEnv is the environment variable HTTP snippets read the agent token from.
const TokenEnv = "MCP_BROKER_TOKEN"

// Harness is an MCP client the broker can write a snippet for.
type Harness struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	File  string `json:"file"`           // where the snippet goes
	HTTP  bool   `json:"http"`           // the client can reach a Streamable HTTP server directly
	Agent string `json:"default_agent"`  // the agent name used when none is given
	Note  string `json:"note,omitempty"` // shown with every snippet
}

var harnesses = []Harness{
	{ID: "claude-code", Title: "Claude Code", File: ".mcp.json in the project (or the claude mcp add command for every project)", HTTP: true, Agent: "claude-code"},
	{ID: "claude-desktop", Title: "Claude Desktop", File: `%APPDATA%\Claude\claude_desktop_config.json (Settings > Developer > Edit Config)`, Agent: "claude-desktop",
		Note: "Claude Desktop's config file only starts local (stdio) servers, so it always uses the shim."},
	{ID: "vscode", Title: "VS Code", File: ".vscode/mcp.json in the workspace (or MCP: Open User Configuration)", HTTP: true, Agent: "vscode"},
	{ID: "codex", Title: "Codex", File: "~/.codex/config.toml (or .codex/config.toml in a trusted project)", HTTP: true, Agent: "codex"},
	{ID: "cursor", Title: "Cursor", File: ".cursor/mcp.json in the project, or ~/.cursor/mcp.json", HTTP: true, Agent: "cursor"},
	{ID: "gemini", Title: "Gemini CLI", File: "~/.gemini/settings.json (or .gemini/settings.json in the project)", HTTP: true, Agent: "gemini"},
	{ID: "windsurf", Title: "Windsurf", File: "~/.codeium/windsurf/mcp_config.json (Settings > Cascade > MCP servers)", HTTP: true, Agent: "windsurf"},
}

// Harnesses lists the supported clients.
func Harnesses() []Harness { return slices.Clone(harnesses) }

// Find returns the harness with id.
func Find(id string) (Harness, bool) {
	i := slices.IndexFunc(harnesses, func(h Harness) bool { return h.ID == id })
	if i < 0 {
		return Harness{}, false
	}
	return harnesses[i], true
}

// Target is what the snippet points at.
type Target struct {
	Exe   string // the mcp-broker executable, as the client will run it
	Agent string
	URL   string // the broker's MCP endpoint, for HTTP snippets
	HTTP  bool   // connect over HTTP instead of the stdio shim
}

// Snippet is one rendered config.
type Snippet struct {
	Harness Harness  `json:"harness"`
	Agent   string   `json:"agent"`
	HTTP    bool     `json:"http"`
	Format  string   `json:"format"` // json or toml
	Text    string   `json:"text"`
	Command string   `json:"command,omitempty"` // the client's own add command, when it has one
	Notes   []string `json:"notes,omitempty"`
}

// Render builds the snippet for harness h.
func Render(h Harness, t Target) (Snippet, error) {
	if t.Agent == "" {
		t.Agent = h.Agent
	}
	if t.HTTP && !h.HTTP {
		return Snippet{}, fmt.Errorf("%s can't connect over HTTP from its config file; leave out --http to use the shim", h.Title)
	}
	if t.HTTP && t.URL == "" {
		return Snippet{}, fmt.Errorf("no broker URL")
	}
	if !t.HTTP && t.Exe == "" {
		return Snippet{}, fmt.Errorf("no mcp-broker executable path")
	}
	s := Snippet{Harness: h, Agent: t.Agent, HTTP: t.HTTP, Format: "json"}
	if h.Note != "" {
		s.Notes = append(s.Notes, h.Note)
	}
	args := []string{"connect", "--agent", t.Agent}
	bearer := func(ref string) map[string]string { return map[string]string{"Authorization": "Bearer " + ref} }
	var doc any
	switch h.ID {
	case "claude-code":
		if t.HTTP {
			doc = servers("mcpServers", obj{"type", "http"}, obj{"url", t.URL}, obj{"headers", bearer("${" + TokenEnv + "}")})
		} else {
			doc = servers("mcpServers", obj{"type", "stdio"}, obj{"command", t.Exe}, obj{"args", args})
			s.Command = "claude mcp add --scope user " + ServerName + " -- " + shellQuote(t.Exe) + " " + strings.Join(args, " ")
		}
	case "claude-desktop":
		doc = servers("mcpServers", obj{"command", t.Exe}, obj{"args", args})
	case "vscode":
		if t.HTTP {
			inner := servers("servers", obj{"type", "http"}, obj{"url", t.URL}, obj{"headers", bearer("${input:mcp-broker-token}")})
			doc = ordered{
				{"inputs", []ordered{{{"type", "promptString"}, {"id", "mcp-broker-token"}, {"description", "mcp-broker agent token for " + t.Agent}, {"password", true}}}},
				inner[0],
			}
		} else {
			doc = servers("servers", obj{"type", "stdio"}, obj{"command", t.Exe}, obj{"args", args})
		}
	case "cursor":
		if t.HTTP {
			doc = servers("mcpServers", obj{"url", t.URL}, obj{"headers", bearer("${env:" + TokenEnv + "}")})
		} else {
			doc = servers("mcpServers", obj{"command", t.Exe}, obj{"args", args})
		}
	case "gemini":
		if t.HTTP {
			doc = servers("mcpServers", obj{"httpUrl", t.URL}, obj{"headers", bearer("$" + TokenEnv)})
		} else {
			doc = servers("mcpServers", obj{"command", t.Exe}, obj{"args", args})
		}
	case "windsurf":
		if t.HTTP {
			doc = servers("mcpServers", obj{"serverUrl", t.URL}, obj{"headers", bearer("${env:" + TokenEnv + "}")})
		} else {
			doc = servers("mcpServers", obj{"command", t.Exe}, obj{"args", args})
		}
	case "codex":
		s.Format = "toml"
		var b strings.Builder
		fmt.Fprintf(&b, "[mcp_servers.%s]\n", ServerName)
		if t.HTTP {
			fmt.Fprintf(&b, "url = %s\nbearer_token_env_var = %s\n", tomlString(t.URL), tomlString(TokenEnv))
			s.Command = "codex mcp add " + ServerName + " --url " + t.URL + " --bearer-token-env-var " + TokenEnv
		} else {
			quoted := make([]string, len(args))
			for i, a := range args {
				quoted[i] = tomlString(a)
			}
			fmt.Fprintf(&b, "command = %s\nargs = [%s]\n", tomlString(t.Exe), strings.Join(quoted, ", "))
			s.Command = "codex mcp add " + ServerName + " -- " + shellQuote(t.Exe) + " " + strings.Join(args, " ")
		}
		s.Text = b.String()
	default:
		return Snippet{}, fmt.Errorf("unknown harness %s", h.ID)
	}
	if doc != nil {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(doc); err != nil {
			return Snippet{}, err
		}
		s.Text = buf.String()
	}
	if t.HTTP {
		s.Notes = append(s.Notes, fmt.Sprintf("Set %s to a token for agent %s (mcp-broker token create %s) in the environment the client starts from.", TokenEnv, t.Agent, t.Agent))
		if h.ID == "vscode" {
			s.Notes[len(s.Notes)-1] = fmt.Sprintf("VS Code asks for the token once and stores it securely; create one with mcp-broker token create %s.", t.Agent)
		}
	} else {
		s.Notes = append(s.Notes, fmt.Sprintf("The shim reads agent-tokens/%s.token next to broker.yaml (mcp-broker token create %s --save).", t.Agent, t.Agent))
	}
	s.Notes = append(s.Notes, "Merge the "+ServerName+" entry into the file if it already lists other servers.")
	return s, nil
}

// obj is one key of a JSON object, kept in order.
type obj struct {
	K string
	V any
}

// ordered is a JSON object that keeps its key order, so snippets read the way the client
// docs write them.
type ordered []obj

func (o ordered) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, kv := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(kv.K)
		b.Write(k)
		b.WriteByte(':')
		var vb bytes.Buffer
		enc := json.NewEncoder(&vb)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(kv.V); err != nil {
			return nil, err
		}
		b.Write(bytes.TrimRight(vb.Bytes(), "\n"))
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// servers wraps one server entry as {key: {"broker": {...}}}.
func servers(key string, fields ...obj) ordered {
	return ordered{{key, ordered{{ServerName, ordered(fields)}}}}
}

// tomlString writes s as a TOML string: literal ('...') when it can be, so Windows paths
// keep their backslashes unescaped.
func tomlString(s string) string {
	if !strings.ContainsAny(s, "'\n\r") {
		return "'" + s + "'"
	}
	b, _ := json.Marshal(s) // JSON escapes are valid TOML basic-string escapes
	return string(b)
}

// shellQuote quotes a path for the add commands, which people paste into a shell.
func shellQuote(s string) string {
	if !strings.ContainsAny(s, " \t\"'&()$`") {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
}

// ShimExe is the mcp-broker binary the shim runs as. The dashboard runs inside the tray
// (mcp-broker-tray.exe), so it uses the mcp-broker binary installed next to the running
// one, never the running one.
func ShimExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	return ShimNextTo(exe), nil
}

// ShimNextTo is the mcp-broker binary in the same folder as exe, with exe's extension.
func ShimNextTo(exe string) string {
	ext := filepath.Ext(exe)
	return filepath.Join(filepath.Dir(exe), "mcp-broker"+ext)
}

// WSLPath turns a Windows path into the /mnt/<drive>/... path a WSL client runs it by.
func WSLPath(p string) string {
	if len(p) >= 2 && p[1] == ':' {
		return "/mnt/" + strings.ToLower(p[:1]) + strings.ReplaceAll(p[2:], `\`, "/")
	}
	return p
}
