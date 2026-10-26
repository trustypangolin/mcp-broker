package importer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/config"
)

// envFileText renders secrets as KEY=value lines.
func envFileText(source string, vars map[string]string) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Imported by mcp-broker from %s. Values are secret: keep this file private.\n", source)
	for _, k := range slices.Sorted(maps.Keys(vars)) {
		v := vars[k]
		if strings.ContainsAny(v, "\r\n") {
			return "", fmt.Errorf("%s: multi-line values are not supported in env files", k)
		}
		if v != strings.TrimSpace(v) || strings.HasPrefix(v, `"`) || strings.HasPrefix(v, "'") {
			v = `"` + v + `"`
		}
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return b.String(), nil
}

// Apply writes the env files and adds the imported connectors to the config file, which
// the running broker then reloads. Nothing is written if the result would not be a valid
// config.
func Apply(cfg *config.Config, items []Item) (n int, err error) {
	data, err := os.ReadFile(cfg.Path)
	if err != nil {
		return 0, err
	}
	var written []string
	defer func() {
		if err != nil {
			for _, p := range written {
				os.Remove(p)
			}
		}
	}()
	out := data
	for _, it := range items {
		if !it.Imported() {
			continue
		}
		if len(it.Secrets) > 0 {
			text, err := envFileText(it.Server.Source.Path, it.Secrets)
			if err != nil {
				return 0, fmt.Errorf("%s: %w", it.Name, err)
			}
			if _, err := os.Stat(it.EnvFile); err == nil {
				return 0, fmt.Errorf("%s: %s already exists; move it aside first", it.Name, it.EnvFile)
			}
			if err := os.MkdirAll(filepath.Dir(it.EnvFile), 0o700); err != nil {
				return 0, err
			}
			if err := os.WriteFile(it.EnvFile, []byte(text), 0o600); err != nil {
				return 0, err
			}
			written = append(written, it.EnvFile)
		}
		if out, err = config.SetEntry(out, "connectors", it.Name, it.Entry); err != nil {
			return 0, fmt.Errorf("%s: %w", it.Name, err)
		}
		n++
	}
	if n == 0 {
		return 0, nil
	}
	if err = config.WriteFile(cfg.Path, out, config.Hash(data)); err != nil {
		return 0, err
	}
	return n, nil
}

// AgentFor is the broker agent a client's shim entry connects as.
func AgentFor(c Client) string { return string(c) }

// Switched reports what switching one client config did.
type Switched struct {
	Source  Source
	Backup  string   // copy of the original file
	Removed []string // server entries replaced by the broker
	// Commands, for clients whose config is not rewritten (Claude Code's ~/.claude.json),
	// are what to run instead.
	Commands []string
}

func isUserClaudeJSON(path string) bool {
	home, err := os.UserHomeDir()
	return err == nil && strings.EqualFold(filepath.Clean(path), filepath.Join(home, ".claude.json"))
}

// Switch points a client config at the broker: the servers now behind the broker are
// removed and one "broker" entry running the shim is added. The original is kept next to
// it. Claude Code's ~/.claude.json is never edited (it holds much more than servers and
// Claude Code rewrites it constantly); the equivalent `claude mcp` commands are returned.
func Switch(src Source, items []Item, shimExe string) (Switched, error) {
	res := Switched{Source: src}
	var names []string
	for _, it := range items {
		if it.Skip != "" {
			continue
		}
		for _, s := range append([]Source{it.Server.Source}, it.Also...) {
			if s.Path == src.Path {
				names = append(names, it.Server.Name)
			}
		}
	}
	agent := AgentFor(src.Client)
	args := []string{"connect", "--agent", agent}

	if src.Client == ClaudeCode && isUserClaudeJSON(src.Path) {
		for _, n := range names {
			res.Commands = append(res.Commands, "claude mcp remove -s user "+n)
		}
		res.Commands = append(res.Commands, fmt.Sprintf(`claude mcp add -s user broker -- "%s" %s`, shimExe, strings.Join(args, " ")))
		res.Removed = names
		return res, nil
	}

	data, err := os.ReadFile(src.Path)
	if err != nil {
		return res, err
	}
	top, err := parseObject(data)
	if err != nil {
		return res, fmt.Errorf("%s: %w (files with comments must be edited by hand)", src.Path, err)
	}
	var servers object
	if raw, ok := top.get(src.Key); ok {
		if servers, err = parseObject(raw); err != nil {
			return res, fmt.Errorf("%s: %s: %w", src.Path, src.Key, err)
		}
	}
	for _, n := range names {
		servers.remove(n)
	}
	entry := map[string]any{"command": shimExe, "args": args}
	if src.Client == VSCode {
		entry["type"] = "stdio"
	}
	if err := servers.set("broker", entry); err != nil {
		return res, err
	}
	if err := top.set(src.Key, servers); err != nil {
		return res, err
	}
	compact, err := json.Marshal(top)
	if err != nil {
		return res, err
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, compact, "", "  "); err != nil {
		return res, err
	}
	pretty.WriteByte('\n')

	res.Backup = src.Path + ".bak-" + time.Now().Format("20060102-150405")
	if err := os.WriteFile(res.Backup, data, 0o600); err != nil {
		return res, err
	}
	tmp := src.Path + ".tmp"
	if err := os.WriteFile(tmp, pretty.Bytes(), 0o600); err != nil {
		return res, err
	}
	if err := os.Rename(tmp, src.Path); err != nil {
		return res, err
	}
	res.Removed = names
	return res, nil
}
