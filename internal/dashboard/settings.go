package dashboard

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/clientcfg"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/gateway"
	"github.com/trustypangolin/mcp-broker/internal/identity"
	"github.com/trustypangolin/mcp-broker/internal/policy"
	"github.com/trustypangolin/mcp-broker/internal/server"
	"github.com/trustypangolin/mcp-broker/internal/skillgen"
)

// validName matches connector and agent names the forms may create. YAML allows more, and
// the raw editor still does; the forms keep names usable in URLs and file names.
var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

type connectorView struct {
	Name      string                `json:"name"`
	Def       config.ConnectorEntry `json:"def"`
	Instances []gateway.Connector   `json:"instances"`
	VisibleTo []string              `json:"visible_to"`
	Issues    []string              `json:"issues,omitempty"`
	// AgentDescription is what list_connectors tells agents; NeedsDescription says it was
	// made up because broker.yaml has none (or an import's placeholder).
	AgentDescription string `json:"agent_description"`
	NeedsDescription bool   `json:"needs_description,omitempty"`
	// AWSProfile is set for connectors running AWS's proxy, so the forms can offer a
	// profile picker.
	AWSProfile string `json:"aws_profile,omitempty"`
}

func (d *Dashboard) listConnectors(w http.ResponseWriter, _ *http.Request) {
	cfg := d.b.Config()
	pol := policy.New(cfg)
	st := d.b.Status()
	issues := map[string][]string{}
	for _, i := range cfg.Check() {
		if name, ok := strings.CutPrefix(i.Where, "connectors."); ok {
			name, _, _ = strings.Cut(name, ".")
			issues[name] = append(issues[name], i.String())
		}
	}
	out := []connectorView{}
	for _, name := range slices.Sorted(maps.Keys(cfg.Connectors)) {
		v := connectorView{Name: name, Def: cfg.Connectors[name].Entry(), Instances: []gateway.Connector{}, VisibleTo: []string{}, Issues: issues[name],
			AgentDescription: d.b.AgentDescription(name), NeedsDescription: cfg.Connectors[name].NeedsDescription()}
		if slices.ContainsFunc(cfg.Connectors[name].Args, func(a string) bool { return strings.HasPrefix(a, "mcp-proxy-for-aws") }) {
			v.AWSProfile = awsProfileOf(cfg.Connectors[name])
		}
		for _, c := range st.Connectors {
			if c.Name == name {
				v.Instances = append(v.Instances, c)
			}
		}
		for _, agent := range slices.Sorted(maps.Keys(cfg.Agents)) {
			if pol.Visible(agent, name) {
				v.VisibleTo = append(v.VisibleTo, agent)
			}
		}
		out = append(out, v)
	}
	groups := cfg.Groups
	if groups == nil {
		groups = map[string]config.Group{}
	}
	writeJSON(w, map[string]any{"connectors": out, "classes": config.Classes, "groups": groups})
}

// putGroupDescription sets (or, empty, removes) what agents are told about a connector
// group.
func (d *Dashboard) putGroupDescription(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	var body config.Group
	if err := decode(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	body.Description = strings.Join(strings.Fields(body.Description), " ")
	cfg := d.b.Config()
	if !slices.ContainsFunc(slices.Collect(maps.Values(cfg.Connectors)), func(c config.Connector) bool { return c.Group == name }) {
		jsonError(w, http.StatusNotFound, "no connector is in group "+name)
		return
	}
	var value any
	if body.Description != "" {
		value = body
	}
	d.apply(w, "groups", name, value)
}

// apply edits one entry of the config file, writes it and reloads the broker.
func (d *Dashboard) apply(w http.ResponseWriter, section, key string, value any) {
	cfg := d.b.Config()
	data, err := os.ReadFile(cfg.Path)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := config.SetEntry(data, section, key, value)
	if err != nil {
		jsonError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	d.write(w, cfg.Path, out, config.Hash(data))
}

func (d *Dashboard) write(w http.ResponseWriter, path string, data []byte, hash string) {
	if err := config.WriteFile(path, data, hash); err != nil {
		status := http.StatusUnprocessableEntity
		if errors.Is(err, config.ErrChanged) {
			status = http.StatusConflict
		}
		jsonError(w, status, err.Error())
		return
	}
	res, err := d.b.Reload()
	if err != nil {
		jsonError(w, http.StatusUnprocessableEntity, "saved, but the reload failed: "+err.Error())
		return
	}
	writeJSON(w, res)
}

func (d *Dashboard) putConnector(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validName.MatchString(name) {
		jsonError(w, http.StatusBadRequest, "names are letters, digits, '.', '_' and '-'")
		return
	}
	var f config.ConnectorEntry
	if err := decode(r, &f); err != nil {
		jsonError(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	if (f.Command == "") == (f.URL == "") {
		jsonError(w, http.StatusBadRequest, "set either a command (local server) or a URL (remote server)")
		return
	}
	cfg := d.b.Config()
	// ?new=1 is the add and duplicate forms: never replace an existing entry.
	if _, exists := cfg.Connectors[name]; exists && r.URL.Query().Get("new") != "" {
		jsonError(w, http.StatusConflict, "a connector named "+name+" already exists; pick another name")
		return
	}
	if other := cfg.SameServerAs(f, name); other != "" {
		jsonError(w, http.StatusConflict, "this is the same server as "+other+
			" (same command, arguments, working directory and environment, or same URL and headers); change what differs, such as an argument or env value")
		return
	}
	d.apply(w, "connectors", name, f)
}

func (d *Dashboard) deleteConnector(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := d.b.Config().Connectors[name]; !ok {
		jsonError(w, http.StatusNotFound, "no such connector")
		return
	}
	d.apply(w, "connectors", name, nil)
}

// maxGroupName keeps group headings to something that fits on a line.
const maxGroupName = 64

// setGroup files connectors or profiles under a group ("" takes them out of any group).
// Creating, renaming and ungrouping a group are all this one edit, written at once.
func (d *Dashboard) setGroup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Section string   `json:"section"`
		Names   []string `json:"names"`
		Group   string   `json:"group"`
		// From is the group being renamed or ungrouped; its description moves with it.
		From string `json:"from"`
	}
	if err := decode(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	body.Group = strings.TrimSpace(body.Group)
	if len(body.Group) > maxGroupName || strings.ContainsAny(body.Group, "\r\n\t") {
		jsonError(w, http.StatusBadRequest, "group names are one line of at most 64 characters")
		return
	}
	cfg := d.b.Config()
	var exists func(string) bool
	switch body.Section {
	case "connectors":
		exists = func(n string) bool { _, ok := cfg.Connectors[n]; return ok }
	case "profiles":
		exists = func(n string) bool { _, ok := cfg.Profiles[n]; return ok }
	default:
		jsonError(w, http.StatusBadRequest, "section is connectors or profiles")
		return
	}
	if len(body.Names) == 0 {
		jsonError(w, http.StatusBadRequest, "pick at least one")
		return
	}
	for _, n := range body.Names {
		if !exists(n) {
			jsonError(w, http.StatusNotFound, "no such entry: "+n)
			return
		}
	}
	data, err := os.ReadFile(cfg.Path)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := config.SetFields(data, body.Section, body.Names, "group", body.Group)
	if err == nil && body.Section == "connectors" && body.From != "" && body.From != body.Group {
		out, err = moveGroupDescription(cfg, out, body.From, body.Group, body.Names)
	}
	if err != nil {
		jsonError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	d.write(w, cfg.Path, out, config.Hash(data))
}

// moveGroupDescription follows a rename: when every member of group from moved, its
// description goes to group to (dropped when ungrouping, kept when to already has one).
func moveGroupDescription(cfg *config.Config, data []byte, from, to string, moved []string) ([]byte, error) {
	desc, ok := cfg.Groups[from]
	if !ok {
		return data, nil
	}
	for name, c := range cfg.Connectors {
		if c.Group == from && !slices.Contains(moved, name) {
			return data, nil // some stay behind; the group still exists
		}
	}
	out, err := config.SetEntry(data, "groups", from, nil)
	if err != nil || to == "" {
		return out, err
	}
	if _, has := cfg.Groups[to]; has {
		return out, nil
	}
	return config.SetEntry(out, "groups", to, desc)
}

// agentForm is an agent as the form edits it.
type agentForm struct {
	Role      string         `json:"role" yaml:"role"`
	Match     agentMatchForm `json:"match" yaml:"match,omitempty"`
	Approvals []string       `json:"approvals" yaml:"approvals,omitempty,flow"`
	// MCP mirrors config.Agent.MCP, or a dashboard save would drop a hand-written mcp: false.
	MCP *bool `json:"mcp,omitempty" yaml:"mcp,omitempty"`
}

type agentMatchForm struct {
	Process []string `json:"process" yaml:"process,omitempty"`
	From    []string `json:"from" yaml:"from,omitempty,flow"`
}

type tokenView struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Label   string    `json:"label,omitempty"`
}

type agentView struct {
	Name    string      `json:"name"`
	Def     agentForm   `json:"def"`
	Visible []string    `json:"visible"`
	Tokens  []tokenView `json:"tokens"`
	Saved   bool        `json:"saved_token"` // agent-tokens/<name>.token exists
	Shim    string      `json:"shim"`        // the client config command line
}

type roleView struct {
	Name       string                   `json:"name"`
	Connectors []string                 `json:"connectors"`
	Classes    map[string]config.Action `json:"classes"`
	Rules      []config.Rule            `json:"rules,omitempty"`
}

func (d *Dashboard) listAgents(w http.ResponseWriter, _ *http.Request) {
	cfg := d.b.Config()
	pol := policy.New(cfg)
	tokens := d.b.Tokens().List()
	exe, _ := clientcfg.ShimExe()
	out := []agentView{}
	for _, name := range slices.Sorted(maps.Keys(cfg.Agents)) {
		a := cfg.Agents[name]
		v := agentView{Name: name, Def: agentForm{Role: a.Role, MCP: a.MCP}, Visible: pol.VisibleConnectors(name), Tokens: []tokenView{}}
		if v.Visible == nil {
			v.Visible = []string{} // a fresh config has no connectors; the page reads .length
		}
		v.Def.Match.Process = a.Match.Process
		for _, p := range a.Match.From {
			v.Def.Match.From = append(v.Def.Match.From, p.String())
		}
		for _, c := range a.Approvals {
			v.Def.Approvals = append(v.Def.Approvals, string(c))
		}
		for _, t := range tokens {
			if t.Agent == name {
				v.Tokens = append(v.Tokens, tokenView{t.ID, t.Created, t.Label})
			}
		}
		if _, err := os.Stat(cfg.AgentTokenPath(name)); err == nil {
			v.Saved = true
		}
		v.Shim = fmt.Sprintf("%q connect --agent %s", exe, name)
		out = append(out, v)
	}
	roles := []roleView{}
	for _, name := range slices.Sorted(maps.Keys(cfg.Roles)) {
		r := cfg.Roles[name]
		rv := roleView{Name: name, Connectors: r.Connectors, Classes: r.Classes, Rules: r.Rules}
		if rv.Connectors == nil {
			rv.Connectors = []string{}
		}
		roles = append(roles, rv)
	}
	// Connector → group, so the page can file what each agent sees under its groups.
	groups := map[string]string{}
	for n, c := range cfg.Connectors {
		if c.Group != "" {
			groups[n] = c.Group
		}
	}
	writeJSON(w, map[string]any{
		"agents": out, "roles": roles, "classes": config.Classes, "connector_groups": groups,
		"channels": []config.Channel{config.ChanElicitation, config.ChanTray, config.ChanDashboard, config.ChanNtfy},
	})
}

func (d *Dashboard) putAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validName.MatchString(name) {
		jsonError(w, http.StatusBadRequest, "names are letters, digits, '.', '_' and '-'")
		return
	}
	var f agentForm
	if err := decode(r, &f); err != nil {
		jsonError(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	d.apply(w, "agents", name, f)
}

// skills downloads skills as a zip (see server.SkillZip for the query): an agent's
// connector skills, to use through this broker's API or straight against each server, in
// the layout an agent type expects; with no agent, the mcp-broker-api meta-skill alone.
func (d *Dashboard) skills(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("agent")
	cfg := d.b.Config()
	if _, ok := cfg.Agents[name]; name != "" && !ok {
		jsonError(w, http.StatusNotFound, "no such agent")
		return
	}
	server.SkillZip(w, r, d.b.AgentSkills, cfg, name)
}

// skillFormats lists the layouts skills can be downloaded in.
func (d *Dashboard) skillFormats(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"formats": skillgen.Formats})
}

func (d *Dashboard) deleteAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg := d.b.Config()
	if _, ok := cfg.Agents[name]; !ok {
		jsonError(w, http.StatusNotFound, "no such agent")
		return
	}
	// Tokens of a removed agent are already useless; revoking them keeps tokens.yaml honest.
	d.b.Tokens().Revoke(name)
	os.Remove(cfg.AgentTokenPath(name))
	d.apply(w, "agents", name, nil)
}

func (d *Dashboard) createToken(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg := d.b.Config()
	if _, ok := cfg.Agents[name]; !ok {
		jsonError(w, http.StatusNotFound, "no such agent")
		return
	}
	var body struct {
		Label string `json:"label"`
		Save  bool   `json:"save"`
	}
	if err := decode(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, "bad body")
		return
	}
	tok, e, err := d.b.Tokens().Create(name, body.Label)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	res := map[string]any{"id": e.ID, "token": tok}
	if body.Save {
		p := cfg.AgentTokenPath(name)
		if err := identity.SaveToken(p, tok); err != nil {
			res["save_error"] = err.Error()
		} else {
			res["saved_to"] = p
		}
	}
	d.b.Logger().Info("token created from dashboard", "agent", name, "id", e.ID)
	writeJSON(w, res)
}

func (d *Dashboard) revokeToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var agent string
	for _, t := range d.b.Tokens().List() {
		if t.ID == id {
			agent = t.Agent
		}
	}
	if agent == "" {
		jsonError(w, http.StatusNotFound, "no such token")
		return
	}
	if _, err := d.b.Tokens().Revoke(id); err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// The saved shim token may be this one; if the agent has none left, it certainly is
	// no good any more.
	left := 0
	for _, t := range d.b.Tokens().List() {
		if t.Agent == agent {
			left++
		}
	}
	if left == 0 {
		os.Remove(d.b.Config().AgentTokenPath(agent))
	}
	d.b.Logger().Info("token revoked from dashboard", "agent", agent, "id", id)
	writeJSON(w, map[string]any{"revoked": id})
}

func (d *Dashboard) getConfig(w http.ResponseWriter, _ *http.Request) {
	cfg := d.b.Config()
	data, err := os.ReadFile(cfg.Path)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]string{"path": cfg.Path, "text": string(data), "hash": config.Hash(data)})
}

func (d *Dashboard) checkConfig(w http.ResponseWriter, r *http.Request) {
	var body struct{ Text string }
	if err := decode(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, "bad body")
		return
	}
	cfg, err := config.Parse([]byte(body.Text), d.b.Config().Dir)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	issues := []string{}
	for _, i := range cfg.Check() {
		issues = append(issues, i.String())
	}
	writeJSON(w, map[string]any{"ok": true, "issues": issues})
}

func (d *Dashboard) putConfig(w http.ResponseWriter, r *http.Request) {
	var body struct{ Text, Hash string }
	if err := decode(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, "bad body")
		return
	}
	if body.Hash == "" {
		jsonError(w, http.StatusBadRequest, "missing hash of the version being edited")
		return
	}
	d.write(w, d.b.Config().Path, []byte(body.Text), body.Hash)
}

// auditTail is how much of the end of the audit log the viewer reads.
const auditTail = 1 << 20

func (d *Dashboard) audit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	n := 200
	fmt.Sscan(q.Get("n"), &n)
	n = min(max(n, 1), 2000)
	agent, kind, outcome, text := q.Get("agent"), q.Get("kind"), q.Get("outcome"), strings.ToLower(q.Get("q"))

	f, err := os.Open(d.b.Config().Audit.Path)
	if err != nil {
		writeJSON(w, map[string]any{"events": []any{}})
		return
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > auditTail {
		f.Seek(-auditTail, io.SeekEnd)
	}
	var lines [][]byte
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		lines = append(lines, bytes.Clone(sc.Bytes()))
	}
	events := []json.RawMessage{}
	for i := len(lines) - 1; i >= 0 && len(events) < n; i-- {
		var e struct{ Agent, Kind, Outcome string }
		if json.Unmarshal(lines[i], &e) != nil {
			continue // the partial first line after a seek
		}
		if (agent != "" && e.Agent != agent) || (kind != "" && e.Kind != kind) || (outcome != "" && e.Outcome != outcome) ||
			(text != "" && !strings.Contains(strings.ToLower(string(lines[i])), text)) {
			continue
		}
		events = append(events, lines[i])
	}
	writeJSON(w, map[string]any{"events": events})
}
