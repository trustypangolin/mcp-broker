package config

import (
	"strings"
	"time"
)

// ConnectorEntry is a connector as the dashboard and import write it to broker.yaml: empty
// fields are left out, durations are written the way people type them.
type ConnectorEntry struct {
	Description string            `json:"description" yaml:"description,omitempty"`
	Tags        []string          `json:"tags" yaml:"tags,omitempty,flow"`
	Command     string            `json:"command" yaml:"command,omitempty"`
	Args        []string          `json:"args" yaml:"args,omitempty"`
	Cwd         string            `json:"cwd" yaml:"cwd,omitempty"`
	Env         map[string]string `json:"env" yaml:"env,omitempty"`
	EnvFile     string            `json:"env_file" yaml:"env_file,omitempty"`
	URL         string            `json:"url" yaml:"url,omitempty"`
	Headers     map[string]string `json:"headers" yaml:"headers,omitempty"`
	IdleUnload  string            `json:"idle_unload" yaml:"idle_unload,omitempty"`
	Instance    string            `json:"instance" yaml:"instance,omitempty"`
	ToolClasses map[string]string `json:"tool_classes" yaml:"tool_classes,omitempty"`
}

// Entry returns the connector in the form it is written back to broker.yaml.
func (c Connector) Entry() ConnectorEntry {
	e := ConnectorEntry{
		Description: c.Description, Tags: c.Tags, Command: c.Command, Args: c.Args, Cwd: c.Cwd,
		Env: c.Env, EnvFile: c.EnvFile, URL: c.URL, Headers: c.Headers, IdleUnload: FormatDuration(c.IdleUnload),
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
