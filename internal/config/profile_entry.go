package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// ProfileEntry is a profile as the dashboard reads and writes it in broker.yaml: exactly
// what the file says (no defaults filled in, env_file and ${NAME} as typed), empty fields
// left out, durations as people type them. It must mirror every Profile field or a
// dashboard save drops it (TestProfileEntryMirrorsProfile checks).
type ProfileEntry struct {
	Description    string            `json:"description" yaml:"description,omitempty"`
	Group          string            `json:"group" yaml:"group,omitempty"`
	Cwd            string            `json:"cwd" yaml:"cwd,omitempty"`
	Command        string            `json:"command" yaml:"command,omitempty"`
	Args           []string          `json:"args" yaml:"args,omitempty"`
	EnvFile        string            `json:"env_file" yaml:"env_file,omitempty"`
	Env            map[string]string `json:"env" yaml:"env,omitempty"`
	PathPrepend    []string          `json:"path_prepend" yaml:"path_prepend,omitempty,flow"`
	Ports          []int             `json:"ports" yaml:"ports,omitempty,flow"`
	RequiresPaths  []string          `json:"requires_paths" yaml:"requires_paths,omitempty,flow"`
	NetworkExposed bool              `json:"network_exposed" yaml:"network_exposed,omitempty"`
	Console        bool              `json:"console" yaml:"console,omitempty"`
	Detach         bool              `json:"detach" yaml:"detach,omitempty"`
	Health         *HealthEntry      `json:"health,omitempty" yaml:"health,omitempty"`
	Stop           *StopEntry        `json:"stop,omitempty" yaml:"stop,omitempty"`
	Update         []FlowList        `json:"update" yaml:"update,omitempty"`
}

type HealthEntry struct {
	HTTP          string `json:"http" yaml:"http,omitempty"`
	Interval      string `json:"interval" yaml:"interval,omitempty"`
	StartTimeout  string `json:"start_timeout" yaml:"start_timeout,omitempty"`
	LogReadyRegex string `json:"log_ready_regex" yaml:"log_ready_regex,omitempty"`
}

type StopEntry struct {
	Graceful string `json:"graceful" yaml:"graceful,omitempty"`
	Timeout  string `json:"timeout" yaml:"timeout,omitempty"`
}

// FlowList is a list written on one line: [git, fetch, --tags].
type FlowList []string

func (l FlowList) MarshalYAML() (any, error) {
	var n yaml.Node
	if err := n.Encode([]string(l)); err != nil {
		return nil, err
	}
	n.Style = yaml.FlowStyle
	return &n, nil
}

// Normalize drops empty health and stop blocks and empty update steps, so a form that sends
// every field writes only what was filled in.
func (e *ProfileEntry) Normalize() {
	if e.Health != nil && *e.Health == (HealthEntry{}) {
		e.Health = nil
	}
	if e.Stop != nil && *e.Stop == (StopEntry{}) {
		e.Stop = nil
	}
	steps := e.Update[:0]
	for _, s := range e.Update {
		if len(s) > 0 {
			steps = append(steps, s)
		}
	}
	e.Update = steps
}

// GetEntry decodes section.key of the config text into out, as written. It reports false
// when there is no such entry.
func GetEntry(data []byte, section, key string, out any) (bool, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return false, err
	}
	if doc.Kind == 0 {
		return false, nil
	}
	if doc.Content[0].Kind != yaml.MappingNode {
		return false, fmt.Errorf("config is not a mapping")
	}
	sec := lookup(doc.Content[0], section)
	if sec == nil || sec.Kind != yaml.MappingNode {
		return false, nil
	}
	e := lookup(sec, key)
	if e == nil {
		return false, nil
	}
	if e.Kind == yaml.ScalarNode && e.Tag == "!!null" {
		return true, nil
	}
	return true, e.Decode(out)
}
