// Package policy decides, for an agent and a tool call or profile action, whether to allow,
// ask for approval, or deny. It implements docs/PERMISSIONS.md sections 3 to 6.
package policy

import (
	"fmt"
	"slices"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/glob"
)

// Decision is the outcome for one call.
type Decision struct {
	Action config.Action
	Class  config.Class // empty for profile actions
	// Visible is false when the agent may not even know the target exists. Callers report
	// invisible targets as unknown, not as forbidden.
	Visible bool
	Reason  string // human-readable trail, safe to show the user and write to the audit log
}

// Engine evaluates policy against one loaded config. Reloading the config means building a
// new Engine; an Engine never changes.
type Engine struct {
	cfg *config.Config
}

// New returns an Engine for cfg.
func New(cfg *config.Config) *Engine { return &Engine{cfg: cfg} }

// Role returns the role name and role for an agent. Unknown or empty agent names get the
// anonymous role.
func (e *Engine) Role(agent string) (string, config.Role) {
	if a, ok := e.cfg.Agents[agent]; ok {
		if r, ok := e.cfg.Roles[a.Role]; ok {
			return a.Role, r
		}
	}
	return config.AnonymousRole, e.cfg.Roles[config.AnonymousRole]
}

// Channels lists where an agent's approvals may be answered. Anonymous gets none, so any
// approve decision for it is effectively a deny.
func (e *Engine) Channels(agent string) []config.Channel {
	if a, ok := e.cfg.Agents[agent]; ok {
		return a.Approvals
	}
	return nil
}

// Visible reports whether agent may see connector at all.
func (e *Engine) Visible(agent, connector string) bool {
	cn, ok := e.cfg.Connectors[connector]
	if !ok {
		return false
	}
	_, r := e.Role(agent)
	return r.Sees(connector, cn.Tags)
}

// VisibleConnectors lists the connectors agent may see, sorted.
func (e *Engine) VisibleConnectors(agent string) []string {
	var out []string
	for name := range e.cfg.Connectors {
		if e.Visible(agent, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// Tool decides a downstream tool call.
func (e *Engine) Tool(agent, connector, tool string, ann Annotations) Decision {
	roleName, role := e.Role(agent)
	cn, ok := e.cfg.Connectors[connector]
	if !ok || !role.Sees(connector, cn.Tags) {
		return Decision{Action: config.Deny, Reason: fmt.Sprintf("role %s: connector %s not granted", roleName, connector)}
	}

	class := Classify(cn, tool, ann)
	key := connector + "/" + tool
	d := Decision{Class: class, Visible: true}
	if r, ok := firstRule(role.Rules, key); ok {
		d.Action = r.Action
		d.Reason = fmt.Sprintf("role %s: rule %s → %s", roleName, r.Match, r.Action)
	} else {
		d.Action = actionOrDeny(role.Classes, string(class))
		d.Reason = fmt.Sprintf("role %s: class %s → %s", roleName, class, d.Action)
	}
	e.applyFloors(&d, key)
	return d
}

// Profile decides a profile action ("status", "start", ...). networkExposed is the plan's
// resolved exposure, not just the config flag, so a --listen 0.0.0.0 argument counts too.
func (e *Engine) Profile(agent, profile, action string, networkExposed bool) Decision {
	roleName, role := e.Role(agent)
	if _, ok := e.cfg.Profiles[profile]; !ok {
		return Decision{Action: config.Deny, Reason: fmt.Sprintf("unknown profile %s", profile)}
	}

	key := "profile/" + profile + "/" + action
	d := Decision{Visible: profileVisible(role, profile)}
	if r, ok := firstRule(role.Rules, key); ok {
		d.Action = r.Action
		d.Reason = fmt.Sprintf("role %s: rule %s → %s", roleName, r.Match, r.Action)
	} else {
		d.Action = actionOrDeny(role.Profiles, action)
		d.Reason = fmt.Sprintf("role %s: profiles.%s → %s", roleName, action, d.Action)
	}
	e.applyFloors(&d, key)

	// Hard guards (docs/PERMISSIONS.md section 6): not configurable.
	if (action == "start" && networkExposed) || action == "kill_foreign" {
		if next := config.Stricter(d.Action, config.Approve); next != d.Action {
			d.Action = next
			d.Reason += "; hard guard → approve"
		}
	}
	return d
}

// profileVisible: an agent sees a profile when it may at least ask for its status.
func profileVisible(role config.Role, profile string) bool {
	statusKey := "profile/" + profile + "/status"
	if r, ok := firstRule(role.Rules, statusKey); ok {
		return r.Action != config.Deny
	}
	return actionOrDeny(role.Profiles, "status") != config.Deny
}

func (e *Engine) applyFloors(d *Decision, key string) {
	for _, f := range e.cfg.Policy.Floors {
		if !glob.Match(f.Match, key) {
			continue
		}
		if next := config.Stricter(d.Action, f.Action); next != d.Action {
			d.Action = next
			d.Reason += fmt.Sprintf("; floor %s → %s", f.Match, f.Action)
		}
	}
}

func firstRule(rules []config.Rule, key string) (config.Rule, bool) {
	for _, r := range rules {
		if glob.Match(r.Match, key) {
			return r, true
		}
	}
	return config.Rule{}, false
}

func actionOrDeny(m map[string]config.Action, k string) config.Action {
	if a, ok := m[k]; ok {
		return a
	}
	return config.Deny
}
