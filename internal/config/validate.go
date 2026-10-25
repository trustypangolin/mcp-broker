package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/trustypangolin/mcp-broker/internal/glob"
	"github.com/trustypangolin/mcp-broker/internal/redact"
)

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// Validate checks the config's structure. It reports every problem at once, sorted, so a
// user fixing broker.yaml by hand sees them all.
func (c *Config) Validate() error {
	var errs []string
	add := func(format string, args ...any) { errs = append(errs, fmt.Sprintf(format, args...)) }

	if _, port, err := net.SplitHostPort(c.Server.Listen); err != nil {
		add("server.listen: %v", err)
	} else if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		add("server.listen: invalid port %q", port)
	}
	if fp := c.Server.FirewallProfile; fp != "private" && fp != "domain" && fp != "public" && fp != "none" {
		add("server.firewall_profile: %q is not private, domain, public or none", fp)
	}
	if (c.Server.TLS.CertFile == "") != (c.Server.TLS.KeyFile == "") {
		add("server.tls: cert_file and key_file must be set together")
	}
	if c.Server.MaxResultBytes < 0 {
		add("server.max_result_bytes: must be positive")
	}
	for _, n := range c.Approvals.Notify {
		if n != string(ChanTray) && n != string(ChanNtfy) {
			add("approvals.notify: unknown notifier %q", n)
		}
		if n == string(ChanNtfy) && c.Approvals.Ntfy.URL == "" {
			add("approvals.notify: ntfy needs approvals.ntfy.url")
		}
	}

	tags := map[string]bool{}
	for name, cn := range c.Connectors {
		p := "connectors." + name
		if !nameRE.MatchString(name) || name == "profile" {
			add("%s: invalid connector name (letters, digits, _ . -; not \"profile\")", p)
		}
		switch {
		case cn.Command == "" && cn.URL == "":
			add("%s: needs command (local) or url (remote)", p)
		case cn.Command != "" && cn.URL != "":
			add("%s: set command or url, not both", p)
		case cn.URL != "":
			if u, err := url.Parse(cn.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				add("%s.url: must be an http(s) URL", p)
			}
			if len(cn.Args) > 0 || cn.Cwd != "" {
				add("%s: args and cwd only apply to local connectors", p)
			}
		default:
			if len(cn.Headers) > 0 {
				add("%s: headers only apply to remote connectors", p)
			}
		}
		if cn.Instance != "shared" && cn.Instance != "per_agent" {
			add("%s.instance: %q is not shared or per_agent", p, cn.Instance)
		}
		for k, v := range cn.Env {
			if redact.IsSecretKey(k) && !strings.Contains(v, "${") {
				add("%s.env.%s: secret values belong in env_file, not broker.yaml", p, k)
			}
		}
		for k, v := range cn.Headers {
			if !strings.Contains(v, "${") {
				add("%s.headers.%s: header values are secret; reference ${NAME} from env_file", p, k)
			}
		}
		for _, t := range cn.Tags {
			tags[t] = true
		}
	}

	for name, a := range c.Agents {
		p := "agents." + name
		if !nameRE.MatchString(name) {
			add("%s: invalid agent name", p)
		}
		if name == AnonymousRole {
			add("%s: %q is reserved for unidentified connections", p, name)
		}
		if a.Role == "" {
			add("%s.role: required", p)
		} else if _, ok := c.Roles[a.Role]; !ok {
			add("%s.role: unknown role %q", p, a.Role)
		}
		for _, pr := range a.Match.Process {
			if strings.TrimSpace(pr) == "" {
				add("%s.match.process: empty pattern", p)
			}
		}
		if len(a.Match.Process) > 0 && len(a.Match.From) > 0 {
			for _, f := range a.Match.From {
				if !f.Addr().IsLoopback() {
					add("%s.match: process matching only works for loopback connections; from %s can never match", p, f)
				}
			}
		}
	}

	for name, r := range c.Roles {
		p := "roles." + name
		if !nameRE.MatchString(name) {
			add("%s: invalid role name", p)
		}
		for _, sel := range r.Connectors {
			s := strings.TrimPrefix(sel, "!")
			switch {
			case s == "":
				add("%s.connectors: empty selector", p)
			case strings.HasPrefix(s, "tag:"):
				if !tags[strings.TrimPrefix(s, "tag:")] {
					add("%s.connectors: no connector has %s", p, s)
				}
			case !strings.ContainsAny(s, "*?"):
				if _, ok := c.Connectors[s]; !ok {
					add("%s.connectors: unknown connector %q", p, s)
				}
			}
		}
		for k := range r.Classes {
			if Class(k).Rank() == len(Classes) {
				add("%s.classes: unknown class %q", p, k)
			}
		}
		for k := range r.Profiles {
			if !slices.Contains(ProfileActions, k) {
				add("%s.profiles: unknown profile action %q", p, k)
			}
		}
		for i, rule := range r.Rules {
			validateRule(fmt.Sprintf("%s.rules[%d]", p, i), rule, add)
		}
	}
	for i, rule := range c.Policy.Floors {
		validateRule(fmt.Sprintf("policy.floors[%d]", i), rule, add)
	}

	for name, pr := range c.Profiles {
		p := "profiles." + name
		if !nameRE.MatchString(name) {
			add("%s: invalid profile name", p)
		}
		if pr.Command == "" {
			add("%s.command: required", p)
		}
		for _, port := range pr.Ports {
			if port <= 0 || port > 65535 {
				add("%s.ports: invalid port %d", p, port)
			}
		}
		if g := pr.Stop.Graceful; g != "" && g != "ctrl_break" && g != "sigterm" && g != "none" {
			add("%s.stop.graceful: %q is not ctrl_break, sigterm or none", p, g)
		}
		if pr.Health.LogReadyRegex != "" {
			if _, err := regexp.Compile(pr.Health.LogReadyRegex); err != nil {
				add("%s.health.log_ready_regex: %v", p, err)
			}
		}
		if pr.Health.HTTP != "" {
			if _, err := url.Parse(pr.Health.HTTP); err != nil {
				add("%s.health.http: %v", p, err)
			}
		}
		for k, v := range pr.Env {
			if redact.IsSecretKey(k) && !strings.Contains(v, "${") {
				add("%s.env.%s: secret values belong in env_file, not broker.yaml", p, k)
			}
		}
	}

	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return errors.New("invalid config:\n  " + strings.Join(errs, "\n  "))
}

func validateRule(p string, r Rule, add func(string, ...any)) {
	if r.Match == "" {
		add("%s.match: required", p)
	} else if !strings.Contains(r.Match, "/") {
		add("%s.match: %q should be <connector>/<tool> or profile/<name>/<action>", p, r.Match)
	}
	if r.Action == "" {
		add("%s.action: required", p)
	}
}

// Issue is an environment problem found by Check.
type Issue struct {
	Warning bool // false means error
	Where   string
	Msg     string
}

func (i Issue) String() string {
	level := "error"
	if i.Warning {
		level = "warning"
	}
	return fmt.Sprintf("%s: %s: %s", level, i.Where, i.Msg)
}

// Check looks at the machine rather than the file: env files, commands, working
// directories and unused config. It never starts anything.
func (c *Config) Check() []Issue {
	var out []Issue
	issue := func(warn bool, where, format string, args ...any) {
		out = append(out, Issue{Warning: warn, Where: where, Msg: fmt.Sprintf(format, args...)})
	}
	red := redact.New()

	for _, name := range sortedKeys(c.Connectors) {
		cn := c.Connectors[name]
		p := "connectors." + name
		r, err := cn.Resolve(red)
		if err != nil {
			issue(false, p, "%s", red.String(err.Error()))
			continue
		}
		if !cn.Remote() {
			checkCommand(p, r.Command, r.Cwd, issue)
		}
	}
	for _, name := range sortedKeys(c.Profiles) {
		pr := c.Profiles[name]
		p := "profiles." + name
		r, err := pr.Resolve(red)
		if err != nil {
			issue(false, p, "%s", red.String(err.Error()))
			continue
		}
		checkCommand(p, r.Command, r.Cwd, issue)
	}

	used := map[string]bool{}
	for _, a := range c.Agents {
		used[a.Role] = true
	}
	for _, name := range sortedKeys(c.Roles) {
		if !used[name] && name != AnonymousRole {
			issue(true, "roles."+name, "no agent uses this role")
		}
	}
	for _, name := range sortedKeys(c.Connectors) {
		if !c.anyRoleSees(name) {
			issue(true, "connectors."+name, "no role grants this connector")
		}
	}

	if host, _, err := net.SplitHostPort(c.Server.Listen); err == nil {
		if ip := net.ParseIP(host); host == "" || (ip != nil && !ip.IsLoopback()) {
			if c.Server.TLS.CertFile == "" {
				issue(true, "server.listen", "listening beyond loopback without TLS: agent tokens cross the network in clear text")
			}
		}
	}
	return out
}

func (c *Config) anyRoleSees(connector string) bool {
	for _, r := range c.Roles {
		if r.Sees(connector, c.Connectors[connector].Tags) {
			return true
		}
	}
	return false
}

// Sees reports whether the role grants a connector. Selectors are evaluated in order and the
// last one that matches decides, so ["*", "!tag:mail"] means everything except mail.
func (r Role) Sees(connector string, tags []string) bool {
	seen := false
	for _, sel := range r.Connectors {
		neg := strings.HasPrefix(sel, "!")
		s := strings.TrimPrefix(sel, "!")
		var hit bool
		if t, ok := strings.CutPrefix(s, "tag:"); ok {
			hit = slices.ContainsFunc(tags, func(x string) bool { return strings.EqualFold(x, t) })
		} else {
			hit = glob.Match(s, connector)
		}
		if hit {
			seen = !neg
		}
	}
	return seen
}

func checkCommand(p, command, cwd string, issue func(bool, string, string, ...any)) {
	if _, err := exec.LookPath(command); err != nil {
		issue(false, p+".command", "%q not found", command)
	}
	if cwd != "" {
		if st, err := os.Stat(cwd); err != nil || !st.IsDir() {
			issue(false, p+".cwd", "%q is not a directory", cwd)
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
