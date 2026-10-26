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

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/gateway"
	"github.com/trustypangolin/mcp-broker/internal/identity"
	"github.com/trustypangolin/mcp-broker/internal/policy"
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
		v := connectorView{Name: name, Def: cfg.Connectors[name].Entry(), Instances: []gateway.Connector{}, VisibleTo: []string{}, Issues: issues[name]}
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
	writeJSON(w, map[string]any{"connectors": out, "classes": config.Classes})
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

// agentForm is an agent as the form edits it.
type agentForm struct {
	Role      string         `json:"role" yaml:"role"`
	Match     agentMatchForm `json:"match" yaml:"match,omitempty"`
	Approvals []string       `json:"approvals" yaml:"approvals,omitempty,flow"`
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
	exe, _ := os.Executable()
	out := []agentView{}
	for _, name := range slices.Sorted(maps.Keys(cfg.Agents)) {
		a := cfg.Agents[name]
		v := agentView{Name: name, Def: agentForm{Role: a.Role}, Visible: pol.VisibleConnectors(name), Tokens: []tokenView{}}
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
	writeJSON(w, map[string]any{
		"agents": out, "roles": roles, "classes": config.Classes,
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
	agent, kind, text := q.Get("agent"), q.Get("kind"), strings.ToLower(q.Get("q"))

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
		var e struct{ Agent, Kind string }
		if json.Unmarshal(lines[i], &e) != nil {
			continue // the partial first line after a seek
		}
		if (agent != "" && e.Agent != agent) || (kind != "" && e.Kind != kind) ||
			(text != "" && !strings.Contains(strings.ToLower(string(lines[i])), text)) {
			continue
		}
		events = append(events, lines[i])
	}
	writeJSON(w, map[string]any{"events": events})
}
