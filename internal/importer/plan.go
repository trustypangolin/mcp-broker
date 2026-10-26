package importer

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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
}

// Imported reports whether the item adds or replaces a connector.
func (it Item) Imported() bool { return it.Skip == "" && !it.Existing }

// Options tune a plan.
type Options struct {
	Overwrite bool                // replace connectors that already exist with another definition
	Tags      map[string][]string // per connector name; replaces the guessed tags
	// Getenv resolves ${NAME} references to the client's environment. Defaults to os.Getenv.
	Getenv func(string) string
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
func guessTags(name string, s Server) []string {
	hay := strings.ToLower(name + " " + s.Command + " " + strings.Join(s.Args, " "))
	var out []string
	for _, g := range []struct {
		tag   string
		words []string
	}{
		{"office", []string{"excel", "word", "powerpoint", "office", "docx", "xlsx", "pptx", "outlook"}},
		{"mail", []string{"mail", "imap", "smtp", "sieve", "gmail", "pop3"}},
		{"browser", []string{"browser", "chrome", "firefox", "playwright", "puppeteer"}},
		{"media", []string{"comfy", "image", "video", "audio", "media", "stable-diffusion"}},
	} {
		for _, w := range g.words {
			if strings.Contains(hay, w) {
				out = append(out, g.tag)
				break
			}
		}
	}
	return out
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

// sameServer reports whether an existing connector runs the same thing.
func sameServer(cur config.Connector, e config.ConnectorEntry) bool {
	if e.URL != "" {
		return cur.URL == e.URL
	}
	return strings.EqualFold(cur.Command, e.Command) && slices.Equal(cur.Args, e.Args)
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
	var items []Item
	byName := map[string]int{}
	for _, s := range servers {
		name := connectorName(s.Name)
		it := Item{Name: name, Server: s, EnvFile: filepath.Join(cfg.Dir, "secrets", name+".env")}
		it.build(o)

		if i, ok := byName[name]; ok {
			prev := &items[i]
			if reflect.DeepEqual(prev.Entry, it.Entry) && reflect.DeepEqual(prev.Secrets, it.Secrets) {
				prev.Also = append(prev.Also, s.Source)
				continue
			}
			it.Skip = fmt.Sprintf("a different %q is already planned from %s; import this file on its own to use this one", name, prev.Server.Source.Path)
			items = append(items, it)
			continue
		}

		switch {
		case it.Skip != "":
		case isShim(s):
			it.Skip = "this is the broker's own shim"
		default:
			if cur, ok := cfg.Connectors[name]; ok {
				if sameServer(cur, it.Entry) {
					it.Existing = true
				} else if !o.Overwrite {
					it.Skip = "a different connector with this name already exists (use --overwrite to replace it)"
				}
				break
			}
			// The same server under another name is already there too.
			for _, other := range slices.Sorted(maps.Keys(cfg.Connectors)) {
				if sameServer(cfg.Connectors[other], it.Entry) {
					it.Existing = true
					it.Notes = append(it.Notes, "same server as connector "+other)
					it.Name = other
					break
				}
			}
		}
		byName[name] = len(items)
		items = append(items, it)
	}

	// Who would see each connector under the current roles.
	trial := *cfg
	trial.Connectors = maps.Clone(cfg.Connectors)
	for _, it := range items {
		if it.Imported() {
			trial.Connectors[it.Name] = config.Connector{Tags: it.Entry.Tags, Command: it.Entry.Command, URL: it.Entry.URL, Instance: "shared"}
		}
	}
	pol := policy.New(&trial)
	for i := range items {
		for _, agent := range slices.Sorted(maps.Keys(cfg.Agents)) {
			if pol.Visible(agent, items[i].Name) {
				items[i].Visible = append(items[i].Visible, agent)
			}
		}
	}
	return items
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
	e.IdleUnload = "30m"
	it.Entry = e
	it.Notes = notes
}
