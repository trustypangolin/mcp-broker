package dashboard

import (
	"fmt"
	"maps"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/config"
)

// roleEntry is a role as the dashboard writes it. It must mirror every config.Role field,
// or saving a role from the Roles page drops what it leaves out.
type roleEntry struct {
	Connectors []string                 `yaml:"connectors"`
	Classes    map[string]config.Action `yaml:"classes,omitempty"`
	Profiles   map[string]config.Action `yaml:"profiles,omitempty"`
	Rules      []config.Rule            `yaml:"rules,omitempty"`
}

type roleDetail struct {
	Name       string                   `json:"name"`
	Connectors []string                 `json:"connectors"` // the selectors, as written
	Classes    map[string]config.Action `json:"classes"`
	Profiles   map[string]config.Action `json:"profiles"`
	Rules      []config.Rule            `json:"rules"`
	Agents     []string                 `json:"agents"`  // agents that use it
	Visible    []string                 `json:"visible"` // connectors it grants
	Builtin    bool                     `json:"builtin,omitempty"`
}

type roleConnector struct {
	Name        string   `json:"name"`
	Group       string   `json:"group,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Description string   `json:"description"`
}

// listRoles is everything the Roles page draws: each role with what it grants, and the
// connectors it can grant, with their groups.
func (d *Dashboard) listRoles(w http.ResponseWriter, _ *http.Request) {
	cfg := d.b.Config()
	users := map[string][]string{}
	for _, a := range slices.Sorted(maps.Keys(cfg.Agents)) {
		users[cfg.Agents[a].Role] = append(users[cfg.Agents[a].Role], a)
	}
	conns := slices.Sorted(maps.Keys(cfg.Connectors))
	roles := []roleDetail{}
	for _, name := range slices.Sorted(maps.Keys(cfg.Roles)) {
		r := cfg.Roles[name]
		v := roleDetail{Name: name, Connectors: r.Connectors, Classes: r.Classes, Profiles: r.Profiles, Rules: r.Rules,
			Agents: users[name], Visible: []string{}, Builtin: name == config.AnonymousRole}
		for _, c := range conns {
			if r.Sees(c, cfg.Connectors[c]) {
				v.Visible = append(v.Visible, c)
			}
		}
		if v.Connectors == nil {
			v.Connectors = []string{}
		}
		if v.Classes == nil {
			v.Classes = map[string]config.Action{}
		}
		if v.Profiles == nil {
			v.Profiles = map[string]config.Action{}
		}
		if v.Rules == nil {
			v.Rules = []config.Rule{}
		}
		if v.Agents == nil {
			v.Agents = []string{}
		}
		roles = append(roles, v)
	}
	cs := []roleConnector{}
	for _, c := range conns {
		cn := cfg.Connectors[c]
		cs = append(cs, roleConnector{Name: c, Group: cn.Group, Tags: cn.Tags, Description: d.b.AgentDescription(c)})
	}
	groups := cfg.Groups
	if groups == nil {
		groups = map[string]config.Group{}
	}
	writeJSON(w, map[string]any{
		"roles": roles, "connectors": cs, "groups": groups,
		"classes": config.Classes, "profile_actions": config.ProfileActions, "has_profiles": len(cfg.Profiles) > 0,
	})
}

// putRole creates (?new=1) or changes a role. visible, when sent, is the set of connectors
// the role should see; the broker works out the selectors (config.RoleSelectors), keeping
// hand-written ones. connectors replaces the selectors as given instead. Fields left out
// keep what the role has; rules are always kept (the Tools page edits them).
func (d *Dashboard) putRole(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validName.MatchString(name) {
		jsonError(w, http.StatusBadRequest, "names are letters, digits, '.', '_' and '-'")
		return
	}
	var body struct {
		Visible    *[]string                `json:"visible"`
		Connectors *[]string                `json:"connectors"`
		Classes    map[string]config.Action `json:"classes"`
		Profiles   map[string]config.Action `json:"profiles"`
	}
	if err := decode(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	cfg := d.b.Config()
	cur, exists := cfg.Roles[name]
	if exists && r.URL.Query().Get("new") != "" {
		jsonError(w, http.StatusConflict, "a role named "+name+" already exists; pick another name")
		return
	}
	e := roleEntry{Connectors: cur.Connectors, Classes: cur.Classes, Profiles: cur.Profiles, Rules: cur.Rules}
	switch {
	case body.Visible != nil:
		want := map[string]bool{}
		for _, c := range *body.Visible {
			if _, ok := cfg.Connectors[c]; !ok {
				jsonError(w, http.StatusNotFound, "no such connector: "+c)
				return
			}
			want[c] = true
		}
		e.Connectors = config.RoleSelectors(cur.Connectors, cfg.Connectors, want)
	case body.Connectors != nil:
		e.Connectors = *body.Connectors
	}
	if e.Connectors == nil {
		e.Connectors = []string{}
	}
	if body.Classes != nil {
		for k, a := range body.Classes {
			if config.Class(k).Rank() == len(config.Classes) || !validAction(a) {
				jsonError(w, http.StatusBadRequest, fmt.Sprintf("classes.%s: %q", k, a))
				return
			}
		}
		e.Classes = body.Classes
	}
	if body.Profiles != nil {
		for k, a := range body.Profiles {
			if !slices.Contains(config.ProfileActions, k) || !validAction(a) {
				jsonError(w, http.StatusBadRequest, fmt.Sprintf("profiles.%s: %q", k, a))
				return
			}
		}
		e.Profiles = body.Profiles
	}
	if len(e.Classes) == 0 {
		e.Classes = nil
	}
	if len(e.Profiles) == 0 {
		e.Profiles = nil
	}
	d.apply(w, "roles", name, e)
}

func validAction(a config.Action) bool {
	return a == config.Allow || a == config.Approve || a == config.Deny
}

// deleteRole removes a role no agent uses.
func (d *Dashboard) deleteRole(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg := d.b.Config()
	if _, ok := cfg.Roles[name]; !ok {
		jsonError(w, http.StatusNotFound, "no such role")
		return
	}
	if name == config.AnonymousRole {
		jsonError(w, http.StatusConflict, "anonymous is built in: it is what unidentified connections get")
		return
	}
	for a, ag := range cfg.Agents {
		if ag.Role == name {
			jsonError(w, http.StatusConflict, "agent "+a+" uses this role; give it another role first")
			return
		}
	}
	d.apply(w, "roles", name, nil)
}

// approvalSettings is the approvals section as the dashboard form edits it, in seconds.
type approvalSettings struct {
	Timeout    int `json:"timeout"`
	Wait       int `json:"wait"`
	CheckEvery int `json:"check_every"`
	Remind     int `json:"remind"`
}

func secs(d time.Duration) int { return int(d / time.Second) }

func (d *Dashboard) getApprovalSettings(w http.ResponseWriter, _ *http.Request) {
	a := d.b.Config().Approvals
	writeJSON(w, map[string]any{
		"settings": approvalSettings{secs(a.Timeout), secs(a.Wait), secs(a.CheckEvery), secs(a.Remind)},
		"defaults": approvalSettings{secs(config.DefaultApprovalTimeout), secs(config.DefaultApprovalWait), secs(config.DefaultCheckEvery), 0},
		"max":      approvalSettings{secs(config.MaxApprovalTimeout), secs(config.MaxApprovalWait), secs(config.MaxApprovalTimeout), secs(config.MaxApprovalTimeout)},
	})
}

// putApprovalSettings writes approvals.timeout, wait, check_every and remind; the rest of
// the section (notify, ntfy) and its comments stay as they are.
func (d *Dashboard) putApprovalSettings(w http.ResponseWriter, r *http.Request) {
	var s approvalSettings
	if err := decode(r, &s); err != nil {
		jsonError(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	switch {
	case s.Timeout < 1 || s.Timeout > secs(config.MaxApprovalTimeout):
		jsonError(w, http.StatusBadRequest, "the timeout is between 1 second and 24 hours")
		return
	case s.Wait < 1 || s.Wait > secs(config.MaxApprovalWait):
		jsonError(w, http.StatusBadRequest, "a call waits between 1 second and 10 minutes")
		return
	case s.CheckEvery < 1 || s.CheckEvery > s.Timeout:
		jsonError(w, http.StatusBadRequest, "agents check between every second and once per timeout")
		return
	case s.Remind != 0 && (s.Remind < 10 || s.Remind > s.Timeout):
		jsonError(w, http.StatusBadRequest, "reminders are off (0) or every 10 seconds up to the timeout")
		return
	}
	cfg := d.b.Config()
	data, err := os.ReadFile(cfg.Path)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := data
	for _, kv := range []struct {
		key string
		n   int
	}{{"timeout", s.Timeout}, {"wait", s.Wait}, {"check_every", s.CheckEvery}, {"remind", s.Remind}} {
		var v any
		if kv.n > 0 {
			v = durText(kv.n)
		}
		if out, err = config.SetEntry(out, "approvals", kv.key, v); err != nil {
			jsonError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}
	d.write(w, cfg.Path, out, config.Hash(data))
}

// durText writes seconds the way a person would in broker.yaml: 90s, 2m, 1h.
func durText(n int) string {
	switch {
	case n%3600 == 0:
		return fmt.Sprintf("%dh", n/3600)
	case n%60 == 0:
		return fmt.Sprintf("%dm", n/60)
	}
	return fmt.Sprintf("%ds", n)
}
