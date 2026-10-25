package config

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/trustypangolin/mcp-broker/internal/redact"
)

// ReadEnvFile parses KEY=VALUE lines. Blank lines and # comments are skipped, a leading
// "export " is allowed, and matching single or double quotes around the value are stripped.
func ReadEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	env := map[string]string{}
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		s := strings.TrimSpace(sc.Text())
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		k, v, ok := strings.Cut(s, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, line)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		env[k] = v
	}
	return env, sc.Err()
}

// Expand replaces ${NAME} from vars; "$$" is a literal "$". Unknown names are an error
// rather than an empty string, so a missing secret fails loudly at plan time.
func Expand(s string, vars map[string]string) (string, error) {
	if !strings.Contains(s, "$") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '$' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '$':
			b.WriteByte('$')
			i++
		case '{':
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", fmt.Errorf("unterminated ${ in %q", s)
			}
			name := s[i+2 : i+2+end]
			v, ok := vars[name]
			if !ok {
				return "", fmt.Errorf("undefined variable ${%s}", name)
			}
			b.WriteString(v)
			i += 2 + end
		default:
			b.WriteByte('$')
		}
	}
	return b.String(), nil
}

// Resolved is a connector or profile with env_file loaded and every ${NAME} expanded.
type Resolved struct {
	Command string
	Args    []string
	Cwd     string
	URL     string
	Env     map[string]string // only the variables the config names, not the broker's own env
	Headers map[string]string
	Secret  map[string]bool // env keys whose values are secret
}

// EnvKeys lists env names for display, marking secret ones, never values.
func (r *Resolved) EnvKeys() []string {
	keys := make([]string, 0, len(r.Env))
	for k := range r.Env {
		if r.Secret[k] {
			keys = append(keys, k+" (redacted)")
		} else {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// resolve merges env_file and env (env wins), expands everything, and registers secret
// values with red so they can never be echoed back.
func resolve(envFile string, env map[string]string, command string, args []string, cwd, url string, headers map[string]string, red *redact.Redactor) (*Resolved, error) {
	vars := map[string]string{}
	if envFile != "" {
		fileVars, err := ReadEnvFile(envFile)
		if err != nil {
			return nil, fmt.Errorf("env_file: %w", err)
		}
		for k, v := range fileVars {
			vars[k] = v
		}
	}
	for k, v := range env {
		vars[k] = v
	}

	r := &Resolved{Env: map[string]string{}, Headers: map[string]string{}, Secret: map[string]bool{}}
	for k, v := range vars {
		if redact.IsSecretKey(k) {
			r.Secret[k] = true
			red.Add(v)
		}
	}
	// env values may themselves reference other variables (e.g. VIRTUAL_ENV=${ROOT}\venv).
	for k, v := range vars {
		ev, err := Expand(v, vars)
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		r.Env[k] = ev
	}

	var err error
	if r.Command, err = Expand(command, vars); err != nil {
		return nil, fmt.Errorf("command: %w", err)
	}
	if r.Cwd, err = Expand(cwd, vars); err != nil {
		return nil, fmt.Errorf("cwd: %w", err)
	}
	if r.URL, err = Expand(url, vars); err != nil {
		return nil, fmt.Errorf("url: %w", err)
	}
	for _, a := range args {
		ea, err := Expand(a, vars)
		if err != nil {
			return nil, fmt.Errorf("args: %w", err)
		}
		r.Args = append(r.Args, ea)
	}
	for k, v := range headers {
		ev, err := Expand(v, vars)
		if err != nil {
			return nil, fmt.Errorf("header %s: %w", k, err)
		}
		r.Headers[k] = ev
		red.Add(ev) // header values are always secret
		if after, ok := strings.CutPrefix(ev, "Bearer "); ok {
			red.Add(after)
		}
	}
	return r, nil
}

// Resolve loads the connector's env_file and expands its settings.
func (c Connector) Resolve(red *redact.Redactor) (*Resolved, error) {
	return resolve(c.EnvFile, c.Env, c.Command, c.Args, c.Cwd, c.URL, c.Headers, red)
}

// Resolve loads the profile's env_file and expands its settings.
func (p Profile) Resolve(red *redact.Redactor) (*Resolved, error) {
	return resolve(p.EnvFile, p.Env, p.Command, p.Args, p.Cwd, "", nil, red)
}
