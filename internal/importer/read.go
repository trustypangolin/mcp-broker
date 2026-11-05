// Package importer turns MCP server entries from client configs (Claude Desktop, Claude
// Code, VS Code, Codex, Cursor, Gemini CLI, Windsurf, .mcp.json files) into broker
// connectors, moving secrets into env files, and can switch some of those clients over to
// the broker's shim.
package importer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// Client identifies which kind of config a file is, which decides the agent its shim
// entry uses and how (or whether) the file can be rewritten.
type Client string

const (
	ClaudeDesktop Client = "claude-desktop" // claude_desktop_config.json
	ClaudeCode    Client = "claude-code"    // ~/.claude.json (user scope) or a project .mcp.json
	VSCode        Client = "vscode"         // VS Code mcp.json ("servers")
	Codex         Client = "codex"          // ~/.codex/config.toml ([mcp_servers.*])
	Cursor        Client = "cursor"         // .cursor/mcp.json
	Gemini        Client = "gemini"         // .gemini/settings.json
	Windsurf      Client = "windsurf"       // mcp_config.json
	Generic       Client = "mcp-client"     // anything else with an mcpServers block
)

// Source is one client config file.
type Source struct {
	Client Client
	Path   string // a file path, or the name an uploaded file came with
	// Key is the JSON object holding the servers: "mcpServers" or VS Code's "servers".
	Key string
}

// Server is one MCP server entry as a client config declares it.
type Server struct {
	Name   string
	Source Source
	// Scope is the project a server belongs to, for configs that hold servers per project
	// (~/.claude.json); empty for the file's own servers.
	Scope    string
	Type     string // stdio, http or sse
	Command  string
	Args     []string
	Env      map[string]string
	Cwd      string
	URL      string
	Headers  map[string]string
	Disabled bool
}

// DefaultSources lists the client configs on this machine that exist.
func DefaultSources() []Source {
	var out []Source
	add := func(c Client, path, key string) {
		if _, err := os.Stat(path); err == nil {
			out = append(out, Source{Client: c, Path: path, Key: key})
		}
	}
	if dir, err := os.UserConfigDir(); err == nil {
		add(ClaudeDesktop, claudeDesktopConfig(dir, os.Getenv("LOCALAPPDATA")), "mcpServers")
		add(VSCode, filepath.Join(dir, "Code", "User", "mcp.json"), "servers")
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(ClaudeCode, filepath.Join(home, ".claude.json"), "mcpServers")
		add(Codex, filepath.Join(home, ".codex", "config.toml"), "mcp_servers")
		add(Cursor, filepath.Join(home, ".cursor", "mcp.json"), "mcpServers")
		add(Gemini, filepath.Join(home, ".gemini", "settings.json"), "mcpServers")
		add(Windsurf, filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"), "mcpServers")
	}
	return out
}

// ScanSources is DefaultSources plus the project configs (.mcp.json, .vscode/mcp.json,
// .cursor/mcp.json) of every project Claude Code knows about in ~/.claude.json.
func ScanSources() []Source {
	out := DefaultSources()
	home, err := os.UserHomeDir()
	if err != nil {
		return out
	}
	data, err := os.ReadFile(filepath.Join(home, ".claude.json"))
	if err != nil {
		return out
	}
	top, err := parseObject(data)
	if err != nil {
		return out
	}
	raw, ok := top.get("projects")
	if !ok {
		return out
	}
	projects, err := parseObject(raw)
	if err != nil {
		return out
	}
	seen := map[string]bool{}
	for _, s := range out {
		seen[strings.ToLower(filepath.Clean(s.Path))] = true
	}
	for _, p := range projects {
		for _, rel := range []string{".mcp.json", filepath.Join(".vscode", "mcp.json"), filepath.Join(".cursor", "mcp.json")} {
			path := filepath.Join(p.Key, rel)
			if seen[strings.ToLower(filepath.Clean(path))] {
				continue
			}
			if fi, err := os.Stat(path); err == nil && !fi.IsDir() {
				seen[strings.ToLower(filepath.Clean(path))] = true
				out = append(out, SourceFor(path))
			}
		}
	}
	return out
}

// claudeDesktopConfig finds the config Claude Desktop actually reads. The Microsoft Store
// build is an MSIX package whose %APPDATA% is redirected into the package's LocalCache, so
// when that copy exists the one in roaming is stale and must not be imported or switched.
func claudeDesktopConfig(roaming, localAppData string) string {
	if localAppData != "" {
		pattern := filepath.Join(localAppData, "Packages", "Claude_*", "LocalCache", "Roaming", "Claude", "claude_desktop_config.json")
		if m, _ := filepath.Glob(pattern); len(m) > 0 {
			return m[0]
		}
	}
	return filepath.Join(roaming, "Claude", "claude_desktop_config.json")
}

// SourceFor guesses what kind of client config path is from its name.
func SourceFor(path string) Source {
	norm := strings.ToLower(filepath.ToSlash(path))
	base := filepath.Base(norm)
	dir := filepath.Base(filepath.Dir(norm))
	src := Source{Client: Generic, Path: path, Key: "mcpServers"}
	switch {
	case base == "claude_desktop_config.json":
		src.Client = ClaudeDesktop
	case base == ".claude.json" || base == ".mcp.json" || base == "claude.json":
		src.Client = ClaudeCode
	case base == "mcp.json" && (dir == ".vscode" || dir == "user"):
		src.Client, src.Key = VSCode, "servers"
	case base == "mcp.json" && dir == ".cursor":
		src.Client = Cursor
	case base == "config.toml" || strings.HasSuffix(base, ".toml"):
		src.Client, src.Key = Codex, "mcp_servers"
	case base == "settings.json" && dir == ".gemini":
		src.Client = Gemini
	case base == "mcp_config.json":
		src.Client = Windsurf
	}
	return src
}

type rawServer struct {
	Type      string            `json:"type" toml:"type"`
	Command   string            `json:"command" toml:"command"`
	Args      []string          `json:"args" toml:"args"`
	Env       map[string]string `json:"env" toml:"env"`
	Cwd       string            `json:"cwd" toml:"cwd"`
	URL       string            `json:"url" toml:"url"`
	HTTPURL   string            `json:"httpUrl" toml:"-"`   // Gemini CLI
	ServerURL string            `json:"serverUrl" toml:"-"` // Windsurf
	Headers   map[string]string `json:"headers" toml:"http_headers"`
	Disabled  bool              `json:"disabled" toml:"-"`
	Enabled   *bool             `json:"-" toml:"enabled"` // Codex
	// Codex: the environment variable holding a bearer token for url.
	BearerEnv string `json:"-" toml:"bearer_token_env_var"`
}

func (r rawServer) server(name string, src Source, scope string) Server {
	s := Server{Name: name, Source: src, Scope: scope, Type: strings.ToLower(r.Type), Command: r.Command, Args: r.Args,
		Env: r.Env, Cwd: r.Cwd, URL: r.URL, Headers: r.Headers, Disabled: r.Disabled || (r.Enabled != nil && !*r.Enabled)}
	if s.URL == "" {
		s.URL = r.HTTPURL
	}
	if s.URL == "" {
		s.URL = r.ServerURL
	}
	if r.BearerEnv != "" {
		if s.Headers == nil {
			s.Headers = map[string]string{}
		}
		s.Headers["Authorization"] = "Bearer ${" + r.BearerEnv + "}"
	}
	if s.Type == "" {
		s.Type = "stdio"
		if s.URL != "" {
			s.Type = "http"
		}
	}
	switch s.Type {
	case "streamable-http", "streamablehttp", "streamable_http", "remote":
		s.Type = "http"
	case "local":
		s.Type = "stdio"
	}
	return s
}

// Read returns the servers declared in the file src names, in file order.
func Read(src Source) ([]Server, error) {
	data, err := os.ReadFile(src.Path)
	if err != nil {
		return nil, err
	}
	servers, _, err := Parse(src, data)
	return servers, err
}

// Parse reads the servers in data, a config of any supported client. src.Path is used in
// messages and src.Client is refined from what the file holds (VS Code's "servers", a Codex
// TOML file, ~/.claude.json's per-project servers); the source the servers carry is
// returned. JSON with comments and trailing commas (VS Code's JSONC) is accepted.
func Parse(src Source, data []byte) ([]Server, Source, error) {
	trimmed := bytes.TrimSpace(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")))
	if len(trimmed) == 0 {
		return nil, src, fmt.Errorf("%s: empty", src.Path)
	}
	if trimmed[0] != '{' {
		src.Client, src.Key = Codex, "mcp_servers"
		s, err := parseTOML(src, trimmed)
		return s, src, err
	}
	top, err := parseObject(stripJSONC(trimmed))
	if err != nil {
		return nil, src, fmt.Errorf("%s: %w", src.Path, err)
	}
	var out []Server
	read := func(raw json.RawMessage, key, scope string) error {
		servers, err := parseObject(raw)
		if err != nil {
			return fmt.Errorf("%s: %s: %w", src.Path, key, err)
		}
		for _, kv := range servers {
			var r rawServer
			if err := json.Unmarshal(kv.Val, &r); err != nil {
				return fmt.Errorf("%s: %s.%s: %w", src.Path, key, kv.Key, err)
			}
			out = append(out, r.server(kv.Key, src, scope))
		}
		return nil
	}
	found := false
	for _, key := range []string{"mcpServers", "servers"} {
		if raw, ok := top.get(key); ok {
			found = true
			if key == "servers" && src.Client == Generic {
				src.Client = VSCode
			}
			src.Key = key
			if err := read(raw, key, ""); err != nil {
				return nil, src, err
			}
		}
	}
	// VS Code's settings.json: "mcp": {"servers": {...}}.
	if raw, ok := top.get("mcp"); ok && !found {
		if inner, err := parseObject(raw); err == nil {
			if raw, ok := inner.get("servers"); ok {
				found = true
				src.Client, src.Key = VSCode, "mcp.servers"
				if err := read(raw, "mcp.servers", ""); err != nil {
					return nil, src, err
				}
			}
		}
	}
	// ~/.claude.json: servers of each project, under projects.<path>.mcpServers.
	if raw, ok := top.get("projects"); ok {
		if projects, err := parseObject(raw); err == nil {
			for _, p := range projects {
				proj, err := parseObject(p.Val)
				if err != nil {
					continue
				}
				if raw, ok := proj.get("mcpServers"); ok {
					found = true
					if src.Client == Generic {
						src.Client = ClaudeCode
					}
					if err := read(raw, "projects."+p.Key+".mcpServers", p.Key); err != nil {
						return nil, src, err
					}
				}
			}
		}
	}
	// A pasted servers object on its own: {"name": {"command": ...}, ...}.
	if !found && looksLikeServers(top) {
		found = true
		if err := read(trimmedJSON(top), "servers", ""); err != nil {
			return nil, src, err
		}
	}
	if !found {
		return nil, src, fmt.Errorf("%s: no MCP servers found (looked for mcpServers, servers, mcp.servers and projects)", src.Path)
	}
	return out, src, nil
}

func looksLikeServers(o object) bool {
	if len(o) == 0 {
		return false
	}
	for _, m := range o {
		var r rawServer
		if json.Unmarshal(m.Val, &r) != nil || (r.Command == "" && r.URL == "" && r.HTTPURL == "" && r.ServerURL == "") {
			return false
		}
	}
	return true
}

func trimmedJSON(o object) json.RawMessage {
	b, _ := json.Marshal(o)
	return b
}

func parseTOML(src Source, data []byte) ([]Server, error) {
	var doc struct {
		MCPServers map[string]rawServer `toml:"mcp_servers"`
	}
	md, err := toml.Decode(string(data), &doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", src.Path, err)
	}
	if len(doc.MCPServers) == 0 {
		return nil, fmt.Errorf("%s: no [mcp_servers.*] tables", src.Path)
	}
	// File order: the TOML metadata lists keys as they appear.
	var order []string
	for _, k := range md.Keys() {
		if len(k) == 2 && k[0] == "mcp_servers" && !slices.Contains(order, k[1]) {
			order = append(order, k[1])
		}
	}
	for _, name := range slices.Sorted(maps.Keys(doc.MCPServers)) {
		if !slices.Contains(order, name) {
			order = append(order, name)
		}
	}
	var out []Server
	for _, name := range order {
		out = append(out, doc.MCPServers[name].server(name, src, ""))
	}
	return out, nil
}

// stripJSONC removes // and /* */ comments and trailing commas outside strings, so VS
// Code's JSONC files parse as JSON. Plain JSON comes back unchanged.
func stripJSONC(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inStr, esc := false, false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if inStr {
			out = append(out, c)
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch {
		case c == '"':
			inStr = true
			out = append(out, c)
		case c == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				i++
			}
			if i < len(data) {
				out = append(out, '\n')
			}
		case c == '/' && i+1 < len(data) && data[i+1] == '*':
			i += 2
			for i+1 < len(data) && (data[i] != '*' || data[i+1] != '/') {
				i++
			}
			i++
		case c == ',':
			// Drop a comma followed only by whitespace (or comments) and a closer.
			j := i + 1
		scan:
			for j < len(data) {
				switch {
				case data[j] == ' ' || data[j] == '\t' || data[j] == '\r' || data[j] == '\n':
					j++
				case data[j] == '/' && j+1 < len(data) && data[j+1] == '/':
					for j < len(data) && data[j] != '\n' {
						j++
					}
				case data[j] == '/' && j+1 < len(data) && data[j+1] == '*':
					j += 2
					for j+1 < len(data) && (data[j] != '*' || data[j+1] != '/') {
						j++
					}
					j += 2
				default:
					break scan
				}
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				continue
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

// object is a JSON object that keeps its key order, so a rewritten client config differs
// from the original only where it was changed.
type object []member

type member struct {
	Key string
	Val json.RawMessage
}

func parseObject(data []byte) (object, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("not a JSON object")
	}
	var out object
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		// A repeated key (it happens in ~/.claude.json) keeps the last value, as
		// encoding/json would.
		if i := out.index(key); i >= 0 {
			out[i].Val = v
			continue
		}
		out = append(out, member{key, v})
	}
	return out, nil
}

func (o object) index(key string) int {
	return slices.IndexFunc(o, func(m member) bool { return m.Key == key })
}

func (o object) get(key string) (json.RawMessage, bool) {
	if i := o.index(key); i >= 0 {
		return o[i].Val, true
	}
	return nil, false
}

func (o *object) set(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if i := o.index(key); i >= 0 {
		(*o)[i].Val = b
	} else {
		*o = append(*o, member{key, b})
	}
	return nil
}

func (o *object) remove(key string) {
	if i := o.index(key); i >= 0 {
		*o = slices.Delete(*o, i, i+1)
	}
}

// MarshalJSON writes the members in order.
func (o object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		k, _ := json.Marshal(m.Key)
		b.Write(k)
		b.WriteByte(':')
		b.Write(m.Val)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
