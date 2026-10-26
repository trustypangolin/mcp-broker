package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// EnvConfigPath names the environment variable that overrides the default config location.
const EnvConfigPath = "MCP_BROKER_CONFIG"

// AnonymousRole is the built-in role for unidentified loopback connections.
const AnonymousRole = "anonymous"

// ResolvePath picks the config file: the flag value, else $MCP_BROKER_CONFIG, else
// <user config dir>/mcp-broker/broker.yaml.
func ResolvePath(flagValue string) (string, error) {
	if flagValue != "" {
		return filepath.Abs(flagValue)
	}
	if v := os.Getenv(EnvConfigPath); v != "" {
		return filepath.Abs(v)
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating user config dir: %w", err)
	}
	return filepath.Join(dir, "mcp-broker", "broker.yaml"), nil
}

// Load reads, defaults and structurally validates the config at path. Environment checks
// (files and commands exist) are separate: see Check.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg, err := Parse(data, filepath.Dir(abs))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg.Path = abs
	return cfg, nil
}

// Parse decodes config bytes; dir is the directory relative paths resolve against.
func Parse(data []byte, dir string) (*Config, error) {
	cfg := &Config{}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	// A typo in a security setting must not be silently ignored.
	dec.KnownFields(true)
	// An empty file decodes to io.EOF and means "all defaults".
	if err := dec.Decode(cfg); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	cfg.Dir = dir
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = "127.0.0.1:8765"
	}
	if len(c.Server.AllowedSources) == 0 {
		c.Server.AllowedSources = append([]Prefix(nil), Loopback...)
	}
	if c.Server.FirewallProfile == "" {
		c.Server.FirewallProfile = "private"
	}
	if c.Server.TokensFile == "" {
		c.Server.TokensFile = "tokens.yaml"
	}
	if c.Server.MaxResultBytes == 0 {
		c.Server.MaxResultBytes = 4 << 20
	}
	if len(c.Dashboard.AdminFrom) == 0 {
		c.Dashboard.AdminFrom = append([]Prefix(nil), Loopback...)
	}
	if c.Dashboard.TokenFile == "" {
		c.Dashboard.TokenFile = "dashboard.token"
	}
	if c.Approvals.Timeout == 0 {
		c.Approvals.Timeout = 120 * time.Second
	}
	if c.Audit.Path == "" {
		c.Audit.Path = "audit.jsonl"
	}

	c.Server.TokensFile = c.abs(c.Server.TokensFile)
	c.Server.TLS.CertFile = c.abs(c.Server.TLS.CertFile)
	c.Server.TLS.KeyFile = c.abs(c.Server.TLS.KeyFile)
	c.Dashboard.TokenFile = c.abs(c.Dashboard.TokenFile)
	c.Audit.Path = c.abs(c.Audit.Path)

	if c.Agents == nil {
		c.Agents = map[string]Agent{}
	}
	for name, a := range c.Agents {
		if len(a.Approvals) == 0 {
			a.Approvals = []Channel{ChanTray, ChanDashboard}
		}
		c.Agents[name] = a
	}
	if c.Roles == nil {
		c.Roles = map[string]Role{}
	}
	if _, ok := c.Roles[AnonymousRole]; !ok {
		c.Roles[AnonymousRole] = Role{}
	}
	if c.Connectors == nil {
		c.Connectors = map[string]Connector{}
	}
	for name, cn := range c.Connectors {
		if cn.Instance == "" {
			cn.Instance = "shared"
		}
		cn.EnvFile = c.abs(cn.EnvFile)
		c.Connectors[name] = cn
	}
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	for name, p := range c.Profiles {
		p.EnvFile = c.abs(p.EnvFile)
		if p.Health.Interval == 0 {
			p.Health.Interval = 3 * time.Second
		}
		if p.Health.StartTimeout == 0 {
			p.Health.StartTimeout = 2 * time.Minute
		}
		if p.Stop.Timeout == 0 {
			p.Stop.Timeout = 20 * time.Second
		}
		c.Profiles[name] = p
	}
}

// AgentTokenPath is where a saved token for the local stdio shim (`mcp-broker connect
// --agent <name>`) lives, next to broker.yaml.
func (c *Config) AgentTokenPath(agent string) string {
	return filepath.Join(c.Dir, "agent-tokens", agent+".token")
}

func (c *Config) abs(p string) string {
	if p == "" || filepath.IsAbs(p) || c.Dir == "" {
		return p
	}
	return filepath.Join(c.Dir, p)
}
