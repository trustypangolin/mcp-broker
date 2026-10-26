// Package importer turns MCP server entries from client configs (Claude Desktop, Claude
// Code, VS Code, .mcp.json files) into broker connectors, moving secrets into env files, and
// can switch those clients over to the broker's shim.
package importer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Client identifies which kind of config a file is, which decides the agent its shim
// entry uses and how (or whether) the file can be rewritten.
type Client string

const (
	ClaudeDesktop Client = "claude-desktop" // claude_desktop_config.json
	ClaudeCode    Client = "claude-code"    // ~/.claude.json (user scope) or a project .mcp.json
	VSCode        Client = "vscode"         // VS Code mcp.json ("servers")
)

// Source is one client config file.
type Source struct {
	Client Client
	Path   string
	// Key is the JSON object holding the servers: "mcpServers" or VS Code's "servers".
	Key string
}

// Server is one MCP server entry as a client config declares it.
type Server struct {
	Name    string
	Source  Source
	Type    string // stdio, http or sse
	Command string
	Args    []string
	Env     map[string]string
	Cwd     string
	URL     string
	Headers map[string]string
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
		add(ClaudeDesktop, filepath.Join(dir, "Claude", "claude_desktop_config.json"), "mcpServers")
		add(VSCode, filepath.Join(dir, "Code", "User", "mcp.json"), "servers")
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(ClaudeCode, filepath.Join(home, ".claude.json"), "mcpServers")
	}
	return out
}

// SourceFor guesses what kind of client config path is.
func SourceFor(path string) Source {
	base := strings.ToLower(filepath.Base(path))
	switch {
	case base == "claude_desktop_config.json":
		return Source{Client: ClaudeDesktop, Path: path, Key: "mcpServers"}
	case base == "mcp.json" && strings.EqualFold(filepath.Base(filepath.Dir(path)), ".vscode"),
		base == "mcp.json" && strings.EqualFold(filepath.Base(filepath.Dir(path)), "User"):
		return Source{Client: VSCode, Path: path, Key: "servers"}
	}
	return Source{Client: ClaudeCode, Path: path, Key: "mcpServers"}
}

type rawServer struct {
	Type    string            `json:"type"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
	Cwd     string            `json:"cwd"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// Read returns the servers declared in src, in file order.
func Read(src Source) ([]Server, error) {
	data, err := os.ReadFile(src.Path)
	if err != nil {
		return nil, err
	}
	top, err := parseObject(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", src.Path, err)
	}
	raw, ok := top.get(src.Key)
	if !ok {
		return nil, nil
	}
	servers, err := parseObject(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %s: %w", src.Path, src.Key, err)
	}
	var out []Server
	for _, kv := range servers {
		var r rawServer
		if err := json.Unmarshal(kv.Val, &r); err != nil {
			return nil, fmt.Errorf("%s: %s.%s: %w", src.Path, src.Key, kv.Key, err)
		}
		s := Server{Name: kv.Key, Source: src, Type: strings.ToLower(r.Type), Command: r.Command, Args: r.Args, Env: r.Env, Cwd: r.Cwd, URL: r.URL, Headers: r.Headers}
		if s.Type == "" {
			s.Type = "stdio"
			if s.URL != "" {
				s.Type = "http"
			}
		}
		if s.Type == "streamable-http" || s.Type == "streamablehttp" {
			s.Type = "http"
		}
		out = append(out, s)
	}
	return out, nil
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
