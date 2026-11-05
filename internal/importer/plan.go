package importer

import (
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/policy"
	"github.com/trustypangolin/mcp-broker/internal/redact"
)

// Item is what importing one server will do.
type Item struct {
	Name    string // connector name
	Server  Server
	Also    []Source // other configs declaring the same server identically
	Entry   config.ConnectorEntry
	Secrets map[string]string // written to EnvFile, never to broker.yaml
	EnvFile string
	Skip    string   // why it is not imported; empty means it is
	Notes   []string // things to check by hand
	Visible []string // agents that will see it, under the current roles
	// Existing is set when broker.yaml already has an identical connector: nothing to
	// import, but the client can still be switched over.
	Existing bool
	// VariantOf names a connector (existing or planned) that runs the same server with
	// other settings: env, headers or option values. The item is imported next to it under
	// another name, in the same group. Diffs says what differs.
	VariantOf string
	Diffs     []string
	// GroupExisting files VariantOf, which has no group yet, under Entry.Group too.
	GroupExisting bool
}

// Imported reports whether the item adds or replaces a connector.
func (it Item) Imported() bool { return it.Skip == "" && !it.Existing }

// ID identifies the item across plans of the same files: source, project and server name.
func (it Item) ID() string {
	return it.Server.Source.Path + "|" + it.Server.Scope + "|" + it.Server.Name
}

// Options tune a plan.
type Options struct {
	Overwrite bool                // replace connectors that already exist with another definition
	Tags      map[string][]string // per connector name; replaces the guessed tags
	// Getenv resolves ${NAME} references to the client's environment. Defaults to os.Getenv.
	Getenv func(string) string
}

// launch is what a server actually runs or reaches, every reference expanded, for
// comparing an imported server with a connector.
type launch struct {
	Command string
	Args    []string
	Cwd     string
	URL     string
	Env     map[string]string // the process environment the entry sets
	Headers map[string]string // lower-cased names
	Secret  map[string]bool   // env keys whose values are secret: diffs don't show them
	Known   bool              // false: the connector's env file couldn't be read
}

func normPath(s string) string {
	s = strings.TrimSpace(s)
	if runtime.GOOS == "windows" {
		s = strings.TrimSuffix(strings.ToLower(filepath.Clean(strings.ReplaceAll(s, "/", `\`))), ".exe")
	}
	return s
}

func normURL(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), "/")
	if u, err := url.Parse(s); err == nil {
		u.Host, u.Scheme = strings.ToLower(u.Host), strings.ToLower(u.Scheme)
		return u.String()
	}
	return s
}

// expand replaces client ${NAME} references (and ${NAME:-default}) with their values.
func expand(s string, getenv func(string) string) string {
	return envRef.ReplaceAllStringFunc(s, func(m string) string {
		sub := envRef.FindStringSubmatch(m)
		if v := getenv(sub[1]); v != "" {
			return v
		}
		return sub[2]
	})
}

// launchOfServer is how the client would run s.
func launchOfServer(s Server, getenv func(string) string) launch {
	if b, ok := fromBridge(s); ok {
		s = b
	}
	l := launch{Known: true, Env: map[string]string{}, Headers: map[string]string{}, Secret: map[string]bool{}}
	if s.Type == "stdio" {
		l.Command, l.Cwd = normPath(expand(s.Command, getenv)), expand(s.Cwd, getenv)
		if l.Cwd != "" {
			l.Cwd = normPath(l.Cwd)
		}
		for _, a := range s.Args {
			l.Args = append(l.Args, expand(a, getenv))
		}
	} else {
		l.URL = normURL(expand(s.URL, getenv))
	}
	for k, v := range s.Env {
		l.Env[k] = expand(v, getenv)
		l.Secret[k] = redact.IsSecretKey(k)
	}
	for k, v := range s.Headers {
		l.Headers[strings.ToLower(k)] = expand(v, getenv)
	}
	return l
}

// launchOfConnector is how the broker runs cn. Variables in its env file that only feed
// ${NAME} references in the command, arguments, URL or headers (an import writes those)
// aren't counted as process environment.
func launchOfConnector(cn config.Connector) launch {
	l := launch{Env: map[string]string{}, Headers: map[string]string{}, Secret: map[string]bool{}}
	r, err := cn.Resolve(redact.New())
	if err != nil {
		l.Command, l.Args, l.Cwd, l.URL = normPath(cn.Command), cn.Args, cn.Cwd, normURL(cn.URL)
		if cn.URL == "" {
			l.URL = ""
		}
		return l
	}
	l.Known = true
	if cn.Remote() {
		l.URL = normURL(r.URL)
	} else {
		l.Command, l.Args, l.Cwd = normPath(r.Command), r.Args, r.Cwd
		if l.Cwd != "" {
			l.Cwd = normPath(l.Cwd)
		}
	}
	// Names referenced from anywhere, env values and the env file's own values included
	// (an import writes GITHUB_TOKEN=${GH_TOKEN} plus GH_TOKEN=<value>).
	texts := append(append([]string{cn.Command, cn.Cwd, cn.URL}, cn.Args...), slices.Collect(maps.Values(cn.Headers))...)
	texts = append(texts, slices.Collect(maps.Values(cn.Env))...)
	if cn.EnvFile != "" {
		if raw, err := config.ReadEnvFile(cn.EnvFile); err == nil {
			for k, v := range raw {
				if k != strings.Trim(strings.TrimPrefix(v, "${"), "}") {
					texts = append(texts, v)
				}
			}
		}
	}
	refs := map[string]bool{}
	for _, s := range texts {
		for _, m := range envRef.FindAllStringSubmatch(s, -1) {
			refs[m[1]] = true
		}
	}
	for k, v := range r.Env {
		if _, named := cn.Env[k]; !named && (refs[k] || strings.HasPrefix(k, "HEADER_")) {
			continue
		}
		l.Env[k] = v
		l.Secret[k] = r.Secret[k] || redact.IsSecretKey(k)
	}
	for k, v := range r.Headers {
		l.Headers[strings.ToLower(k)] = v
	}
	return l
}

// argShape is args with option values blanked: --profile prod and --profile dev have the
// same shape, so they are the same server set up differently.
func argShape(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		switch {
		case strings.HasPrefix(a, "-") && strings.Contains(a, "="):
			k, _, _ := strings.Cut(a, "=")
			out[i] = k + "=*"
		case i > 0 && strings.HasPrefix(args[i-1], "--") && !strings.Contains(args[i-1], "=") && !strings.HasPrefix(a, "-"):
			out[i] = "*"
		default:
			out[i] = a
		}
	}
	return out
}

// relation is how an imported server relates to a connector.
type relation int

const (
	unrelated relation = iota
	variant            // same server, other settings
	identical
)

// compare relates imported launch a to connector launch b, and says what differs.
func compare(a, b launch) (relation, []string, string) {
	if a.URL != b.URL || a.Command != b.Command || a.Cwd != b.Cwd {
		return unrelated, nil, ""
	}
	var diffs []string
	suffix := ""
	if !slices.Equal(a.Args, b.Args) {
		if a.URL != "" || !slices.Equal(argShape(a.Args), argShape(b.Args)) {
			return unrelated, nil, ""
		}
		for i := range a.Args {
			if a.Args[i] != b.Args[i] {
				opt := ""
				if i > 0 && strings.HasPrefix(a.Args[i-1], "-") {
					opt = a.Args[i-1] + " "
				}
				diffs = append(diffs, fmt.Sprintf("%s%s (existing: %s)", opt, a.Args[i], b.Args[i]))
				if suffix == "" {
					_, v, ok := strings.Cut(a.Args[i], "=")
					if !ok {
						v = a.Args[i]
					}
					suffix = v
				}
			}
		}
	}
	if !b.Known {
		if len(diffs) == 0 {
			return identical, []string{"its env file couldn't be read, so env wasn't compared"}, ""
		}
		return variant, diffs, suffix
	}
	for _, k := range slices.Sorted(maps.Keys(a.Env)) {
		bv, ok := b.Env[k]
		switch {
		case ok && bv == a.Env[k]:
			continue
		case a.Secret[k] || b.Secret[k]:
			diffs = append(diffs, k+" differs (secret)")
		case !ok:
			diffs = append(diffs, fmt.Sprintf("%s=%s (existing doesn't set it)", k, a.Env[k]))
		default:
			diffs = append(diffs, fmt.Sprintf("%s=%s (existing: %s)", k, a.Env[k], bv))
		}
		if suffix == "" && !a.Secret[k] {
			suffix = a.Env[k]
		}
	}
	for _, k := range slices.Sorted(maps.Keys(a.Headers)) {
		if bv, ok := b.Headers[k]; ok && bv != a.Headers[k] || !ok {
			diffs = append(diffs, "header "+k+" differs")
		}
	}
	if len(diffs) > 0 {
		return variant, diffs, suffix
	}
	// What only the broker's connector sets doesn't make another server: the client config
	// is usually the sparser one (the broker's was set up more fully, or the client gets
	// the rest elsewhere). Say so, but it's the same server.
	var extra []string
	for _, k := range slices.Sorted(maps.Keys(b.Env)) {
		if _, ok := a.Env[k]; !ok {
			extra = append(extra, k)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(b.Headers)) {
		if _, ok := a.Headers[k]; !ok {
			extra = append(extra, "header "+k)
		}
	}
	if len(extra) > 0 {
		return identical, []string{"the broker's connector also sets " + strings.Join(extra, ", ")}, ""
	}
	return identical, nil, ""
}

var nameRE = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// connectorName makes a server name usable as a connector name.
func connectorName(s string) string {
	n := strings.Trim(nameRE.ReplaceAllString(s, "-"), "-.")
	if n == "" || n == "profile" {
		n = "server-" + n
	}
	return n
}

// isShim reports whether a server entry is the broker's own shim.
func isShim(s Server) bool {
	// Split on both separators: a config read on Linux may name a Windows path.
	cmd := s.Command
	if i := strings.LastIndexAny(cmd, `/\`); i >= 0 {
		cmd = cmd[i+1:]
	}
	exe := strings.ToLower(cmd)
	return strings.HasPrefix(exe, "mcp-broker") && slices.Contains(s.Args, "connect")
}

// guessTags picks tags from well-known names, so the default roles (which select by tag)
// treat imported connectors sensibly. Mail is tagged so the LAN viewer role excludes it.
// Words match whole tokens, so "wordpress" is not "word" and "password" is not office.
func guessTags(name string, s Server) []string {
	hay := strings.ToLower(name + " " + s.Command + " " + strings.Join(s.Args, " "))
	tokens := strings.FieldsFunc(hay, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	})
	var out []string
	for _, g := range []struct {
		tag   string
		words []string
	}{
		{"office", []string{"excel", "word", "powerpoint", "office", "docx", "xlsx", "pptx", "outlook"}},
		{"mail", []string{"mail", "email", "imap", "smtp", "sieve", "gmail", "pop3"}},
		{"browser", []string{"browser", "chrome", "firefox", "playwright", "puppeteer"}},
		{"media", []string{"comfy", "comfyui", "image", "video", "audio", "media", "diffusion"}},
	} {
		for _, w := range g.words {
			if slices.Contains(tokens, w) {
				out = append(out, g.tag)
				break
			}
		}
	}
	return out
}

// systemVars are Windows location variables. They are not secrets, and the broker runs as
// the same user as the client, so an import writes their values straight into broker.yaml
// instead of copying them into every connector's env file.
var systemVars = map[string]bool{
	"APPDATA": true, "LOCALAPPDATA": true, "USERPROFILE": true, "HOMEDRIVE": true, "HOMEPATH": true,
	"PUBLIC": true, "PROGRAMDATA": true, "PROGRAMFILES": true, "PROGRAMW6432": true,
	"COMMONPROGRAMFILES": true, "SYSTEMDRIVE": true, "SYSTEMROOT": true, "WINDIR": true,
}

var envRef = regexp.MustCompile(`\$\{(?:env:)?([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)
var inputRef = regexp.MustCompile(`\$\{input:[^}]+\}`)

// secretArg spots arguments that look like they carry a credential.
var secretArg = regexp.MustCompile(`(?i)(token|secret|password|passwd|api[-_]?key|authorization|bearer)`)

// fromBridge recognises a stdio-to-HTTP bridge (npx mcp-remote <url> --header K:V ...) and
// returns the remote server it fronts: the broker speaks Streamable HTTP itself, so the
// bridge process, and the header in its command line, are not needed.
func fromBridge(s Server) (Server, bool) {
	args := s.Args
	i := slices.IndexFunc(args, func(a string) bool {
		return a == "mcp-remote" || strings.HasPrefix(a, "mcp-remote@")
	})
	if s.Type != "stdio" || i < 0 || i+1 >= len(args) {
		return s, false
	}
	out := s
	out.Type, out.Command, out.Args, out.URL, out.Headers = "http", "", nil, args[i+1], map[string]string{}
	for j := i + 2; j < len(args); j++ {
		switch {
		case args[j] == "--header" && j+1 < len(args):
			k, v, ok := strings.Cut(args[j+1], ":")
			if !ok {
				return s, false
			}
			out.Headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
			j++
		case args[j] == "--transport" && j+1 < len(args):
			if t := args[j+1]; t != "http-only" && t != "http-first" {
				return s, false // SSE
			}
			j++
		case args[j] == "--allow-http":
		default:
			return s, false // an option we do not understand: leave the bridge alone
		}
	}
	if !strings.HasPrefix(out.URL, "http://") && !strings.HasPrefix(out.URL, "https://") {
		return s, false
	}
	return out, true
}

func escapeDollar(s string) string { return strings.ReplaceAll(s, "$", "$$") }

// translate rewrites a client string for broker.yaml. Client configs use ${NAME} for the
// client's own environment; the broker only expands names from the connector's env and
// env_file, so each reference is resolved now and carried as a variable in vars. Literal $
// is escaped. Everything in vars is in broker form (escaped) too, since env_file values
// are expanded as well.
func translate(s string, vars map[string]string, getenv func(string) string, notes *[]string) string {
	var b strings.Builder
	last := 0
	for _, m := range envRef.FindAllStringSubmatchIndex(s, -1) {
		b.WriteString(escapeDollar(s[last:m[0]]))
		name := s[m[2]:m[3]]
		if systemVars[strings.ToUpper(name)] {
			if v := getenv(name); v != "" {
				b.WriteString(escapeDollar(v))
				last = m[1]
				continue
			}
		}
		if _, ok := vars[name]; !ok {
			v := getenv(name)
			if v == "" && m[4] >= 0 {
				v = s[m[4]:m[5]]
			}
			if v == "" {
				*notes = append(*notes, fmt.Sprintf("${%s} is not set in this environment; fill it in the env file", name))
			}
			vars[name] = escapeDollar(v)
		}
		b.WriteString("${" + name + "}")
		last = m[1]
	}
	b.WriteString(escapeDollar(s[last:]))
	out := b.String()
	if inputRef.MatchString(s) {
		*notes = append(*notes, "uses a VS Code ${input:...} prompt, which the broker cannot ask; replace it with a value in the env file")
	}
	return out
}

// Plan works out what importing servers into cfg would do.
func Plan(cfg *config.Config, servers []Server, o Options) []Item {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	existing := map[string]launch{}
	for name, cn := range cfg.Connectors {
		existing[name] = launchOfConnector(cn)
	}
	var items []Item
	planned := map[string]launch{} // by planned name
	for _, s := range servers {
		name := connectorName(s.Name)
		it := Item{Name: name, Server: s}
		it.build(o)
		if s.Disabled {
			it.Notes = append(it.Notes, "disabled in the client")
		}
		switch {
		case it.Skip != "":
			items = append(items, it)
			continue
		case isShim(s):
			it.Skip = "this is the broker's own shim"
			items = append(items, it)
			continue
		}
		l := launchOfServer(s, o.Getenv)

		// Matches, best first: already in broker.yaml; already planned from another file
		// (or another project in the same file); then a variant of either.
		match, rel := bestMatch(l, existing, name)
		prev, prevRel := bestMatch(l, planned, name)
		switch {
		case rel == identical:
			it.Existing, it.Name = true, match
			if match != name {
				it.Notes = append(it.Notes, "same server as connector "+match)
			}
			_, it.Diffs, _ = compare(l, existing[match])
		case prevRel == identical:
			i := slices.IndexFunc(items, func(p Item) bool { return p.Name == prev && p.Imported() })
			items[i].Also = append(items[i].Also, s.Source)
			continue
		case rel == variant:
			it.markVariant(match, l, existing[match], name, cfg, items)
			if cur := cfg.Connectors[match]; cur.Group == "" {
				it.GroupExisting = true
			} else {
				it.Entry.Group = cur.Group
			}
			if _, set := o.Tags[it.Name]; !set && len(cfg.Connectors[match].Tags) > 0 {
				it.Entry.Tags = slices.Clone(cfg.Connectors[match].Tags) // so the same roles see it
			}
		case prevRel == variant:
			it.markVariant(prev, l, planned[prev], name, cfg, items)
		case o.Overwrite:
			// Same name, different server: replace it.
		default:
			if _, taken := cfg.Connectors[it.Name]; taken {
				was := it.Name
				it.Name = uniqueName(cfg, items, renameFor(it.Name, s))
				it.Notes = append(it.Notes, fmt.Sprintf("renamed: a different connector %s already exists", was))
			}
		}
		if !it.Existing && it.VariantOf == "" && slices.ContainsFunc(items, func(p Item) bool { return p.Name == it.Name && p.Skip == "" }) {
			was := it.Name
			it.Name = uniqueName(cfg, items, renameFor(it.Name, s))
			it.Notes = append(it.Notes, fmt.Sprintf("renamed: a different %s is planned from %s", was, sourceLabel(s)))
		}
		it.setName(it.Name, cfg)
		if it.Imported() {
			planned[it.Name] = l
		}
		items = append(items, it)
	}
	Visibility(cfg, items)
	return items
}

// bestMatch finds the connector in pool the launch is identical to, or failing that a
// variant of, preferring the one named name.
func bestMatch(l launch, pool map[string]launch, name string) (string, relation) {
	best, bestRel := "", unrelated
	for _, n := range slices.Sorted(maps.Keys(pool)) {
		rel, _, _ := compare(l, pool[n])
		if rel > bestRel || (rel == bestRel && rel != unrelated && n == name) {
			best, bestRel = n, rel
		}
	}
	return best, bestRel
}

// markVariant makes the item a variant of connector of, named after what differs.
func (it *Item) markVariant(of string, l, other launch, name string, cfg *config.Config, items []Item) {
	_, diffs, suffix := compare(l, other)
	it.VariantOf, it.Diffs = of, diffs
	if suffix = connectorName(strings.ToLower(filepath.Base(suffix))); suffix == "" || strings.HasPrefix(suffix, "server-") || len(suffix) > 24 {
		suffix = suffixFor(it.Server)
	}
	want := name
	if name == of {
		want = of + "-" + suffix
	}
	it.Name = uniqueName(cfg, items, want)
	it.Entry.Group = of
	// A variant of a planned item shares its group; that item gets it too.
	for i := range items {
		if items[i].Name == of && items[i].Imported() {
			if items[i].Entry.Group == "" {
				items[i].Entry.Group = of
			}
			it.Entry.Group = items[i].Entry.Group
		}
	}
}

// suffixFor names a server's copy after where it came from: its project, else its client.
func suffixFor(s Server) string {
	if s.Scope != "" {
		if n := connectorName(strings.ToLower(filepath.Base(filepath.Clean(s.Scope)))); n != "" && !strings.HasPrefix(n, "server-") {
			return n
		}
	}
	return string(s.Source.Client)
}

// renameFor is the name for a server whose name a different connector has: the project's
// name when it already starts with the server's (imap in project imap-mcp-server is
// imap-mcp-server, not imap-imap-mcp-server), else name-<project or client>.
func renameFor(name string, s Server) string {
	suffix := suffixFor(s)
	if strings.HasPrefix(suffix, strings.ToLower(name)) && suffix != strings.ToLower(name) {
		return suffix
	}
	return name + "-" + suffix
}

func sourceLabel(s Server) string {
	if s.Scope != "" {
		return s.Source.Path + " (project " + s.Scope + ")"
	}
	return s.Source.Path
}

// uniqueName is want, or want-2, want-3... when a connector or planned item has it.
func uniqueName(cfg *config.Config, items []Item, want string) string {
	taken := func(n string) bool {
		_, ok := cfg.Connectors[n]
		return ok || slices.ContainsFunc(items, func(p Item) bool { return p.Name == n && p.Skip == "" })
	}
	if !taken(want) {
		return want
	}
	for i := 2; ; i++ {
		if n := fmt.Sprintf("%s-%d", want, i); !taken(n) {
			return n
		}
	}
}

// setName names the item's connector, and its env file after it.
func (it *Item) setName(name string, cfg *config.Config) {
	it.Name = name
	it.EnvFile = filepath.Join(cfg.Dir, "secrets", name+".env")
	if len(it.Secrets) > 0 {
		it.Entry.EnvFile = it.EnvFile
	}
}

// Visibility works out which agents would see each item under the current roles.
func Visibility(cfg *config.Config, items []Item) {
	trial := *cfg
	trial.Connectors = maps.Clone(cfg.Connectors)
	for _, it := range items {
		if it.Imported() {
			trial.Connectors[it.Name] = config.Connector{Tags: it.Entry.Tags, Command: it.Entry.Command, URL: it.Entry.URL, Instance: "shared"}
		}
	}
	pol := policy.New(&trial)
	for i := range items {
		items[i].Visible = nil
		if items[i].Skip != "" {
			continue
		}
		for _, agent := range slices.Sorted(maps.Keys(cfg.Agents)) {
			if pol.Visible(agent, items[i].Name) {
				items[i].Visible = append(items[i].Visible, agent)
			}
		}
	}
}

// Choice is a person's edit to one planned item, by Item.ID.
type Choice struct {
	Skip  bool     `json:"skip"`
	Name  string   `json:"name"`
	Tags  []string `json:"tags"`
	Group *string  `json:"group"`
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// Choose applies people's edits to a plan: leaving items out, renaming, retagging and
// regrouping. Names are checked against the config and each other.
func Choose(cfg *config.Config, items []Item, choices map[string]Choice) error {
	for i := range items {
		it := &items[i]
		c, ok := choices[it.ID()]
		if !ok || !it.Imported() {
			continue
		}
		if c.Skip {
			it.Skip = "left out"
			continue
		}
		if c.Name != "" && c.Name != it.Name {
			if !validName.MatchString(c.Name) {
				return fmt.Errorf("%q isn't a usable connector name (letters, digits, - _ . only)", c.Name)
			}
			if _, ok := cfg.Connectors[c.Name]; ok {
				return fmt.Errorf("connector %s already exists; pick another name for %s", c.Name, it.Server.Name)
			}
			it.setName(c.Name, cfg)
		}
		if c.Tags != nil {
			it.Entry.Tags = nil
			for _, t := range c.Tags {
				if t = strings.TrimSpace(t); t != "" && !slices.Contains(it.Entry.Tags, t) {
					it.Entry.Tags = append(it.Entry.Tags, t)
				}
			}
		}
		if c.Group != nil {
			it.Entry.Group = strings.TrimSpace(*c.Group)
			if it.Entry.Group == "" {
				it.GroupExisting = false
			}
		}
	}
	seen := map[string]string{}
	for _, it := range items {
		if !it.Imported() {
			continue
		}
		if prev, dup := seen[it.Name]; dup {
			return fmt.Errorf("%s and %s would both be named %s", prev, it.Server.Name, it.Name)
		}
		seen[it.Name] = it.Server.Name
	}
	Visibility(cfg, items)
	return nil
}

func (it *Item) build(o Options) {
	s := it.Server
	e := config.ConnectorEntry{}
	vars := map[string]string{} // env_file contents
	var notes []string
	if b, ok := fromBridge(s); ok {
		s = b
		notes = append(notes, "was run through the mcp-remote bridge; the broker connects to "+s.URL+" directly")
	}

	switch s.Type {
	case "stdio":
		if s.Command == "" {
			it.Skip = "no command"
			return
		}
	case "http":
		if !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
			it.Skip = "the URL is not http(s)"
			return
		}
	case "sse":
		it.Skip = "the SSE transport is not supported; use the server's Streamable HTTP endpoint"
		return
	default:
		it.Skip = fmt.Sprintf("unknown server type %q", s.Type)
		return
	}

	if s.Type == "stdio" {
		e.Command = translate(s.Command, vars, o.Getenv, &notes)
		for _, a := range s.Args {
			e.Args = append(e.Args, translate(a, vars, o.Getenv, &notes))
			if secretArg.MatchString(a) {
				notes = append(notes, "an argument mentions a token, key or password; if it holds the value itself, move it to the env file")
			}
		}
		e.Cwd = translate(s.Cwd, vars, o.Getenv, &notes)
	} else {
		e.URL = translate(s.URL, vars, o.Getenv, &notes)
	}

	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		v := translate(s.Env[k], vars, o.Getenv, &notes)
		if redact.IsSecretKey(k) {
			vars[k] = v
			continue
		}
		if e.Env == nil {
			e.Env = map[string]string{}
		}
		e.Env[k] = v
	}
	for _, k := range slices.Sorted(maps.Keys(s.Headers)) {
		v := translate(s.Headers[k], vars, o.Getenv, &notes)
		// Header values are always secret: keep the scheme in broker.yaml, the value in
		// the env file.
		scheme := ""
		if before, after, ok := strings.Cut(v, " "); ok && (strings.EqualFold(before, "Bearer") || strings.EqualFold(before, "Basic")) {
			scheme, v = before+" ", after
		}
		if strings.HasPrefix(v, "${") && strings.HasSuffix(v, "}") && strings.Count(v, "${") == 1 {
			if e.Headers == nil {
				e.Headers = map[string]string{}
			}
			e.Headers[k] = scheme + v
			continue
		}
		name := "HEADER_" + strings.ToUpper(nameRE.ReplaceAllString(k, "_"))
		vars[name] = v
		if e.Headers == nil {
			e.Headers = map[string]string{}
		}
		e.Headers[k] = scheme + "${" + name + "}"
	}

	if len(vars) > 0 {
		e.EnvFile = it.EnvFile
		it.Secrets = vars
	}
	if tags, ok := o.Tags[it.Name]; ok {
		e.Tags = tags
	} else {
		e.Tags = guessTags(it.Name, s)
	}
	e.Description = "Imported from " + string(s.Source.Client)
	if s.Scope != "" {
		e.Description += " (project " + filepath.Base(filepath.Clean(s.Scope)) + ")"
	}
	e.IdleUnload = "30m"
	it.Entry = e
	it.Notes = notes
}
