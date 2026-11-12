package gateway

import (
	"context"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/redact"
	"github.com/trustypangolin/mcp-broker/internal/skillgen"
)

// Skills turn the broker's connectors into agent skills (see package skillgen, which
// renders them). The gateway's part is the data: which connectors an agent sees, their
// tools as its policy filters them, and, for skills that skip the broker, how to reach
// each server without leaking a secret.

// SkillOptions configures AgentSkills.
type SkillOptions struct {
	// Connectors limits the skills to these connectors; empty means every one the agent sees.
	Connectors []string
	// Direct also fills in each connector's server settings, for skills that reach the
	// server without a broker.
	Direct bool
	// Timeout bounds starting one connector; 0 means 60s.
	Timeout time.Duration
}

// SkillConnector says how one connector's skill was made.
type SkillConnector struct {
	Connector string `json:"connector"`
	Tools     int    `json:"tools"`
	// Source is live (started, full schemas), cache (the tool list last seen, short
	// descriptions and no schemas) or none (never seen).
	Source string `json:"source"`
	Error  string `json:"error,omitempty"`
}

// AgentSkills gathers what skillgen needs for each connector agent can see (or each of
// opts.Connectors that it can see; others are left out, as list_connectors would). It
// starts the connectors, a few at a time, to read their full tool definitions; one that
// won't start, or that needs an OAuth sign-in first, is described from its last-seen list.
func (g *Gateway) AgentSkills(ctx context.Context, agent string, opts SkillOptions) ([]skillgen.Connector, []SkillConnector) {
	if opts.Timeout <= 0 {
		opts.Timeout = 60 * time.Second
	}
	st := g.st()
	names := st.pol.VisibleConnectors(agent)
	if len(opts.Connectors) > 0 {
		names = slices.DeleteFunc(names, func(n string) bool { return !slices.Contains(opts.Connectors, n) })
	}
	conns := make([]skillgen.Connector, len(names))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, name := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			conns[i] = g.skillConnector(ctx, st, agent, name, opts.Timeout)
		}()
	}
	wg.Wait()

	report := make([]SkillConnector, len(names))
	for i, name := range names {
		c := &conns[i]
		cn := st.cfg.Connectors[name]
		c.Name, c.Description, c.Aliases = name, g.AgentDescription(name), cn.Aliases
		if opts.Direct {
			c.Direct = g.directSpec(st.cfg, name, cn)
		}
		report[i] = SkillConnector{Connector: name, Tools: len(c.Tools), Source: c.Source, Error: c.Err}
	}
	return conns, report
}

func (g *Gateway) skillConnector(ctx context.Context, st *state, agent, name string, timeout time.Duration) skillgen.Connector {
	cn := st.cfg.Connectors[name]
	keep := func(n string, ann *mcp.ToolAnnotations) (config.Class, bool, bool) {
		dec := st.pol.Tool(agent, name, n, annotations(&mcp.Tool{Annotations: ann}))
		return dec.Class, dec.Action == config.Approve, dec.Action != config.Deny
	}
	// Starting a connector that needs a sign-in would only start a sign-in nobody asked
	// for; its cached list is the best there is.
	var why string
	if s := g.authState(name, cn); s != "" && s != "signed in" {
		why = "needs an OAuth sign-in (" + s + ")"
	} else {
		cctx, cancel := context.WithTimeout(ctx, timeout)
		sess, tools, err := g.start(cctx, name, agent, "skill generation")
		cancel()
		if err == nil {
			out := skillgen.Connector{Source: "live"}
			if sess != nil && sess.InitializeResult() != nil {
				out.Instructions = strings.TrimSpace(sess.InitializeResult().Instructions)
			}
			for _, t := range tools {
				if class, approve, ok := keep(t.Name, t.Annotations); ok {
					out.Tools = append(out.Tools, skillgen.Tool{Name: t.Name, Summary: summarize(t.Description), Description: t.Description,
						Required: requiredArgs(t.InputSchema), Schema: t.InputSchema, Class: string(class), Approval: approve})
				}
			}
			return out
		}
		why = err.Error()
	}
	l := g.Tools(name)
	out := skillgen.Connector{Source: "cache", Err: why}
	if len(l.Tools) == 0 {
		out.Source = "none"
	}
	for _, t := range l.Tools {
		var ann *mcp.ToolAnnotations
		if t.DestructiveHint != nil {
			ann = &mcp.ToolAnnotations{DestructiveHint: t.DestructiveHint}
		}
		if class, approve, ok := keep(t.Name, ann); ok {
			out.Tools = append(out.Tools, skillgen.Tool{Name: t.Name, Summary: summarize(t.Description), Description: t.Description, Class: string(class), Approval: approve})
		}
	}
	return out
}

var nonVar = regexp.MustCompile(`[^A-Z0-9]+`)

// directSpec is how to reach a connector's server without the broker, from its raw
// config: ${NAME} references stay as they are (the computer running it supplies them), a
// secret-named env value becomes ${ITS_NAME}, a header value is kept only as a ${...}
// template, and an env_file contributes variable names, never values. The redactor then
// scrubs anything that still matches a secret the broker knows.
func (g *Gateway) directSpec(cfg *config.Config, name string, cn config.Connector) *skillgen.Direct {
	d := &skillgen.Direct{Transport: "stdio", Command: cn.Command, Args: slices.Clone(cn.Args), Cwd: cn.Cwd, URL: cn.URL}
	if cn.Remote() {
		d.Transport = "http"
	}
	env := map[string]string{}
	literal := map[string]bool{}
	for k, v := range cn.Env {
		if redact.IsSecretKey(k) {
			env[k] = "${" + k + "}"
			continue
		}
		env[k] = v
		literal[k] = !strings.Contains(v, "${")
	}
	refs := func() []string {
		var all []string
		for _, s := range append(append([]string{d.Command, d.Cwd, d.URL}, d.Args...), slices.Collect(maps.Values(cn.Headers))...) {
			all = append(all, skillgen.VarRefs(s)...)
		}
		for _, v := range env {
			all = append(all, skillgen.VarRefs(v)...)
		}
		return all
	}
	if cn.EnvFile != "" {
		if vars, err := config.ReadEnvFile(cn.EnvFile); err == nil {
			// A file only this connector uses holds what its server needs, and the broker
			// passes all of it on: name every variable. A shared one (G:\Ai\.env) would
			// name every other connector's secrets too: only what this one references.
			shared := 0
			for _, other := range cfg.Connectors {
				if strings.EqualFold(other.EnvFile, cn.EnvFile) {
					shared++
				}
			}
			wanted := map[string]bool{}
			for _, r := range refs() {
				wanted[r] = true
			}
			for k := range vars {
				if _, set := env[k]; set {
					continue
				}
				if shared <= 1 || wanted[k] {
					env[k] = "${" + k + "}"
				}
			}
		}
	}
	if len(cn.Headers) > 0 {
		d.Headers = map[string]string{}
		for k, v := range cn.Headers {
			if strings.Contains(v, "${") {
				d.Headers[k] = v
			} else {
				d.Headers[k] = "${" + strings.Trim(nonVar.ReplaceAllString(strings.ToUpper(name+"_"+k), "_"), "_") + "}"
			}
		}
	}
	if len(env) > 0 {
		d.Env = env
	}
	// Anything that still matches a secret value the broker knows is scrubbed.
	scrub := func(s string) string { return g.red.String(s) }
	d.Command, d.Cwd, d.URL = scrub(d.Command), scrub(d.Cwd), scrub(d.URL)
	for i := range d.Args {
		d.Args[i] = scrub(d.Args[i])
	}
	for k, v := range d.Env {
		d.Env[k] = scrub(v)
	}
	for k, v := range d.Headers {
		d.Headers[k] = scrub(v)
	}

	need := map[string]bool{}
	for _, s := range append(append([]string{d.Command, d.Cwd, d.URL}, d.Args...), slices.Collect(maps.Values(d.Headers))...) {
		for _, r := range skillgen.VarRefs(s) {
			need[r] = true
		}
	}
	for _, v := range d.Env {
		for _, r := range skillgen.VarRefs(v) {
			need[r] = true
		}
	}
	for k := range d.Env {
		if literal[k] {
			delete(need, k) // server.json defines it
		}
	}
	d.Vars = slices.Sorted(maps.Keys(need))
	if o, implicit := cn.OAuthSettings(); o != nil && !implicit {
		d.OAuth = true
	}
	return d
}
