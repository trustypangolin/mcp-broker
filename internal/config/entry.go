package config

import (
	"encoding/json"
	"maps"
	"net/url"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

// ConnectorEntry is a connector as the dashboard and import write it to broker.yaml: empty
// fields are left out, durations are written the way people type them.
type ConnectorEntry struct {
	Description string            `json:"description" yaml:"description,omitempty"`
	Group       string            `json:"group" yaml:"group,omitempty"`
	Aliases     []string          `json:"aliases" yaml:"aliases,omitempty,flow"`
	Tags        []string          `json:"tags" yaml:"tags,omitempty,flow"`
	Command     string            `json:"command" yaml:"command,omitempty"`
	Args        []string          `json:"args" yaml:"args,omitempty"`
	Cwd         string            `json:"cwd" yaml:"cwd,omitempty"`
	Env         map[string]string `json:"env" yaml:"env,omitempty"`
	EnvFile     string            `json:"env_file" yaml:"env_file,omitempty"`
	URL         string            `json:"url" yaml:"url,omitempty"`
	Headers     map[string]string `json:"headers" yaml:"headers,omitempty"`
	OAuth       *OAuth            `json:"oauth,omitempty" yaml:"oauth,omitempty"`
	IdleUnload  string            `json:"idle_unload" yaml:"idle_unload,omitempty"`
	Instance    string            `json:"instance" yaml:"instance,omitempty"`
	ToolClasses map[string]string `json:"tool_classes" yaml:"tool_classes,omitempty"`
}

// Entry returns the connector in the form it is written back to broker.yaml.
func (c Connector) Entry() ConnectorEntry {
	e := ConnectorEntry{
		Description: c.Description, Group: c.Group, Aliases: c.Aliases, Tags: c.Tags, Command: c.Command, Args: c.Args, Cwd: c.Cwd,
		Env: c.Env, EnvFile: c.EnvFile, URL: c.URL, Headers: c.Headers, OAuth: c.OAuth, IdleUnload: FormatDuration(c.IdleUnload),
	}
	if c.Instance != "shared" {
		e.Instance = c.Instance
	}
	if len(c.ToolClasses) > 0 {
		e.ToolClasses = map[string]string{}
		for k, v := range c.ToolClasses {
			e.ToolClasses[k] = string(v)
		}
	}
	return e
}

// LaunchKey identifies the server an entry starts or reaches: two entries with the same
// key are the same MCP server under different names. Labels and policy (name,
// description, tags, group, tool classes, idle unload, instance) don't count. Paths and
// hosts compare case-insensitively on Windows, where they are.
func (e ConnectorEntry) LaunchKey() string {
	norm := func(s string) string {
		s = strings.TrimSpace(s)
		if runtime.GOOS == "windows" {
			s = strings.ToLower(filepath.Clean(strings.ReplaceAll(s, "/", `\`)))
			s = strings.TrimSuffix(s, ".exe")
		}
		return s
	}
	k := struct {
		Command, Cwd, EnvFile, URL string
		Args                       []string
		Env, Headers               map[string]string
		Client                     string
	}{Args: e.Args, Env: e.Env, Headers: e.Headers}
	if e.URL != "" {
		k.URL = strings.TrimRight(strings.TrimSpace(e.URL), "/")
		if u, err := url.Parse(k.URL); err == nil {
			u.Host = strings.ToLower(u.Host)
			u.Scheme = strings.ToLower(u.Scheme)
			k.URL = u.String()
		}
		if e.OAuth != nil {
			k.Client = e.OAuth.ClientID
		}
	} else {
		k.Command, k.Cwd = norm(e.Command), e.Cwd
		if k.Cwd != "" {
			k.Cwd = norm(k.Cwd)
		}
	}
	if e.EnvFile != "" {
		k.EnvFile = norm(e.EnvFile)
	}
	if len(k.Args) == 0 {
		k.Args = nil
	}
	if len(k.Env) == 0 {
		k.Env = nil
	}
	if len(k.Headers) == 0 {
		k.Headers = nil
	}
	b, _ := json.Marshal(k) // maps marshal with sorted keys
	return string(b)
}

// SameServerAs returns the name of a connector in cfg, other than except, that launches
// the same server as e, or "".
func (cfg *Config) SameServerAs(e ConnectorEntry, except string) string {
	key := e.LaunchKey()
	for _, name := range slices.Sorted(maps.Keys(cfg.Connectors)) {
		if name != except && cfg.Connectors[name].Entry().LaunchKey() == key {
			return name
		}
	}
	return ""
}

// FormatDuration prints a duration the way people write it in YAML: 30m, not 30m0s.
func FormatDuration(d time.Duration) string {
	if d == 0 {
		return ""
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
