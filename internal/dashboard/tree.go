package dashboard

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/approval"
	"github.com/trustypangolin/mcp-broker/internal/clientcfg"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/policy"
)

// treeAgent is one agent in the tool tree's agent picker.
type treeAgent struct {
	Name   string   `json:"name"`
	Role   string   `json:"role"`
	Shares []string `json:"shares,omitempty"` // other agents with the same role: rule edits apply to them too
}

type treeTool struct {
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Class       config.Class  `json:"class"`
	Action      config.Action `json:"action"` // what a call does now
	Reason      string        `json:"reason"`
	Rule        config.Action `json:"rule,omitempty"`  // the role's rule for exactly this tool
	Floor       config.Action `json:"floor,omitempty"` // strictest floor that matches: the tool can't be laxer
	toolUse
}

type treeConnector struct {
	Name        string        `json:"name"`
	Group       string        `json:"group,omitempty"`
	Description string        `json:"description,omitempty"`
	Live        bool          `json:"live"`
	Seen        time.Time     `json:"seen,omitzero"`  // when the tool list was taken; zero: never ran
	Rule        config.Action `json:"rule,omitempty"` // the role's <connector>/* rule
	Tools       []treeTool    `json:"tools"`
}

// toolUse is what the audit log says an agent did with a tool.
type toolUse struct {
	Calls    int       `json:"calls,omitempty"`    // calls that ran or were asked about
	Asked    int       `json:"asked,omitempty"`    // calls that needed approval
	Approved int       `json:"approved,omitempty"` // of those, approved
	Last     time.Time `json:"last,omitzero"`
}

// tree serves the per-agent tool tree: every tool an agent can see, what a call to it does
// now and why, the role rule that applies, and the agent's history with it. Decisions come
// from the policy engine itself, so the tree can't disagree with what a call does.
func (d *Dashboard) tree(w http.ResponseWriter, r *http.Request) {
	cfg := d.b.Config()
	pol := policy.New(cfg)
	byRole := map[string][]string{}
	for _, n := range slices.Sorted(maps.Keys(cfg.Agents)) {
		byRole[cfg.Agents[n].Role] = append(byRole[cfg.Agents[n].Role], n)
	}
	agents := []treeAgent{}
	for _, n := range slices.Sorted(maps.Keys(cfg.Agents)) {
		role := cfg.Agents[n].Role
		a := treeAgent{Name: n, Role: role}
		for _, o := range byRole[role] {
			if o != n {
				a.Shares = append(a.Shares, o)
			}
		}
		agents = append(agents, a)
	}
	out := map[string]any{"agents": agents}

	agent := r.URL.Query().Get("agent")
	if _, ok := cfg.Agents[agent]; !ok {
		writeJSON(w, out)
		return
	}
	roleName, role := pol.Role(agent)
	exact := map[string]config.Action{}
	for _, ru := range role.Rules {
		if _, dup := exact[ru.Match]; !dup {
			exact[ru.Match] = ru.Action
		}
	}
	use := d.toolUse(agent)
	conns := []treeConnector{}
	for _, name := range pol.VisibleConnectors(agent) {
		cn := cfg.Connectors[name]
		list := d.b.Tools(name)
		c := treeConnector{Name: name, Group: cn.Group, Description: cn.Description, Live: list.Live,
			Seen: list.Seen, Rule: exact[name+"/*"], Tools: []treeTool{}}
		for _, t := range list.Tools {
			dec := pol.Tool(agent, name, t.Name, t.Annotations())
			c.Tools = append(c.Tools, treeTool{
				Name: t.Name, Description: t.Description, Class: dec.Class, Action: dec.Action,
				Reason: dec.Reason, Rule: exact[name+"/"+t.Name], Floor: dec.Floor, toolUse: use[name+"/"+t.Name],
			})
		}
		slices.SortFunc(c.Tools, func(a, b treeTool) int { return strings.Compare(a.Name, b.Name) })
		conns = append(conns, c)
	}
	out["agent"], out["role"], out["connectors"] = agent, roleName, conns
	writeJSON(w, out)
}

// toolUse tallies an agent's tool calls from the end of the audit log, by connector/tool.
func (d *Dashboard) toolUse(agent string) map[string]toolUse {
	out := map[string]toolUse{}
	f, err := os.Open(d.b.Config().Audit.Path)
	if err != nil {
		return out
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > auditTail {
		f.Seek(-auditTail, io.SeekEnd)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var e struct {
			TS                                             time.Time
			Kind, Agent, Connector, Tool, Decision, Detail string
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Kind != "tool_call" || e.Agent != agent || e.Tool == "" {
			continue
		}
		if e.Decision == string(config.Deny) {
			continue // never ran, and not something the user can pre-approve away
		}
		conn, _, _ := strings.Cut(e.Connector, "@") // per_agent instances
		k := conn + "/" + e.Tool
		u := out[k]
		u.Calls++
		if e.Decision == string(config.Approve) {
			u.Asked++
			if e.Detail == "approved" {
				u.Approved++
			}
		}
		if e.TS.After(u.Last) {
			u.Last = e.TS
		}
		out[k] = u
	}
	return out
}

// setRule sets or clears (action "") a role rule for one connector's tool, or for all of a
// connector's tools (tool "*").
func (d *Dashboard) setRule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Role      string        `json:"role"`
		Connector string        `json:"connector"`
		Tool      string        `json:"tool"`
		Action    config.Action `json:"action"`
	}
	if err := decode(r, &body); err != nil {
		jsonError(w, http.StatusBadRequest, "bad body: "+err.Error())
		return
	}
	d.writeRule(w, body.Role, body.Connector, body.Tool, body.Action)
}

func (d *Dashboard) writeRule(w http.ResponseWriter, role, connector, tool string, action config.Action) {
	cfg := d.b.Config()
	if _, ok := cfg.Roles[role]; !ok {
		jsonError(w, http.StatusNotFound, "no such role")
		return
	}
	if _, ok := cfg.Connectors[connector]; !ok {
		jsonError(w, http.StatusNotFound, "no such connector")
		return
	}
	if tool == "" || strings.ContainsAny(tool, "/\r\n") || (tool != "*" && strings.ContainsAny(tool, "*?[")) {
		jsonError(w, http.StatusBadRequest, "tool is a tool name, or * for all of the connector's tools")
		return
	}
	switch action {
	case "", config.Allow, config.Approve, config.Deny:
	default:
		jsonError(w, http.StatusBadRequest, "action is allow, approve, deny, or empty to inherit")
		return
	}
	data, err := os.ReadFile(cfg.Path)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out, err := config.SetRule(data, role, connector+"/"+tool, action)
	if err != nil {
		jsonError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	d.write(w, cfg.Path, out, config.Hash(data))
}

// alwaysAllow writes an allow rule for an approved tool call's tool, for the agent's role.
func (d *Dashboard) alwaysAllow(w http.ResponseWriter, e approval.Entry) {
	cfg := d.b.Config()
	a, ok := cfg.Agents[e.Agent]
	if e.Kind != "tool.call" || !ok || e.Connector == "" || e.Tool == "" {
		jsonError(w, http.StatusBadRequest, "approved, but only a known agent's tool calls can be always allowed")
		return
	}
	conn, _, _ := strings.Cut(e.Connector, "@")
	d.writeRule(w, a.Role, conn, e.Tool, config.Allow)
}

// discover starts a connector so its tools show in the tree.
func (d *Dashboard) discover(w http.ResponseWriter, r *http.Request) {
	name, agent := r.PathValue("connector"), r.URL.Query().Get("agent")
	cfg := d.b.Config()
	if _, ok := cfg.Connectors[name]; !ok {
		jsonError(w, http.StatusNotFound, "no such connector")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	l, err := d.b.Discover(ctx, name, agent)
	if err != nil {
		jsonError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, map[string]int{"tools": len(l.Tools)})
}

// clientConfig renders the snippet a client needs to connect as an agent. Snippets carry no
// token: the shim reads the saved one, HTTP ones read it from the client's environment.
func (d *Dashboard) clientConfig(w http.ResponseWriter, r *http.Request) {
	name, q := r.PathValue("name"), r.URL.Query()
	cfg := d.b.Config()
	if _, ok := cfg.Agents[name]; !ok {
		jsonError(w, http.StatusNotFound, "no such agent")
		return
	}
	h, ok := clientcfg.Find(q.Get("harness"))
	if !ok {
		writeJSON(w, map[string]any{"harnesses": clientcfg.Harnesses()})
		return
	}
	exe, err := clientcfg.ShimExe()
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if q.Get("wsl") != "" {
		exe = clientcfg.WSLPath(exe)
	}
	s, err := clientcfg.Render(h, clientcfg.Target{Exe: exe, Agent: name, URL: d.b.Status().URL, HTTP: q.Get("http") != ""})
	if err != nil {
		jsonError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"harnesses": clientcfg.Harnesses(), "snippet": s})
}
