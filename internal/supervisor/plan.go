package supervisor

import (
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/audit"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/procinfo"
)

// Plan is exactly what an action will do, resolved and redacted, plus the pre-flight
// checks. It is what the user approves.
type Plan struct {
	Profile        string     `json:"profile"`
	Action         string     `json:"action"`
	State          State      `json:"state"`
	Command        string     `json:"command,omitempty"`
	Args           []string   `json:"args,omitempty"`
	Cwd            string     `json:"cwd,omitempty"`
	EnvKeys        []string   `json:"env_keys,omitempty"` // secret ones marked (redacted)
	PathPrepend    []string   `json:"path_prepend,omitempty"`
	Ports          []int      `json:"ports,omitempty"`
	Bind           string     `json:"bind,omitempty"`
	NetworkExposed bool       `json:"network_exposed"`
	Steps          [][]string `json:"steps,omitempty"` // update
	StopTimeout    string     `json:"stop_timeout,omitempty"`
	// Blocking problems stop the action; Conflicts are only worth knowing.
	Blocking  []string `json:"blocking,omitempty"`
	Conflicts []string `json:"conflicts,omitempty"`
	// ForeignPID holds a profile port without being a run of ours (0 if none or unknown).
	ForeignPID  int    `json:"foreign_pid,omitempty"`
	ForeignPath string `json:"foreign_path,omitempty"`
	Hash        string `json:"hash"` // binds an approval to this exact plan
}

// bindFlags are the arguments apps commonly take a listen address from.
var bindFlags = []string{"--listen", "--host", "--bind", "--address", "--ip", "-H"}

// bindAddress finds the listen address in args. A bare --listen with no value is how
// ComfyUI (and others) say "all interfaces".
func bindAddress(args []string) string {
	for i, a := range args {
		for _, f := range bindFlags {
			if v, ok := strings.CutPrefix(a, f+"="); ok {
				return v
			}
			if a == f {
				if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
					return args[i+1]
				}
				return "0.0.0.0"
			}
		}
	}
	return ""
}

func exposed(bind string) bool {
	if bind == "" || strings.EqualFold(bind, "localhost") {
		return false
	}
	host := bind
	if h, _, err := net.SplitHostPort(bind); err == nil {
		host = h
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip == nil || !ip.IsLoopback() // a hostname or 0.0.0.0 reaches the network
}

// commandExists reports whether the resolved command can be run.
func commandExists(command string, prepend []string) bool {
	c := lookPath(command, prepend)
	if filepath.IsAbs(c) || strings.ContainsAny(c, `/\`) {
		fi, err := os.Stat(c)
		return err == nil && !fi.IsDir()
	}
	_, err := exec.LookPath(c)
	return err == nil
}

// portInUse reports whether something accepts connections on a loopback port.
func portInUse(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 300*time.Millisecond)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// Plan resolves an action ("start", "stop" or "update") without doing anything.
func (s *Supervisor) Plan(name, action string) (Plan, error) {
	p, ok := s.profile(name)
	if !ok {
		return Plan{}, fmt.Errorf("%w %q", ErrUnknown, name)
	}
	st := s.Status(name)
	pl := Plan{Profile: name, Action: action, State: st.State, Ports: p.Ports, PathPrepend: p.PathPrepend}
	ours := st.State.running() || st.State == Updating

	res, err := p.Resolve(s.opts.Redactor)
	if err != nil {
		pl.Blocking = append(pl.Blocking, s.opts.Redactor.String(err.Error()))
	} else {
		red := s.opts.Redactor.String
		pl.Command, pl.Cwd = red(res.Command), red(res.Cwd)
		for _, a := range res.Args {
			pl.Args = append(pl.Args, red(a))
		}
		for _, k := range slices.Sorted(maps.Keys(res.Env)) {
			if res.Secret[k] {
				k += " (redacted)"
			}
			pl.EnvKeys = append(pl.EnvKeys, k)
		}
		pl.Bind = bindAddress(res.Args)
		if action == "update" {
			for i, step := range p.Update {
				var out []string
				for _, tok := range step {
					v, err := config.Expand(tok, res.Env)
					if err != nil {
						pl.Blocking = append(pl.Blocking, fmt.Sprintf("update step %d: %v", i+1, err))
					}
					out = append(out, red(v))
				}
				pl.Steps = append(pl.Steps, out)
			}
		}
	}
	pl.NetworkExposed = p.NetworkExposed || exposed(pl.Bind)

	// Who holds the profile's ports, if not us. Another profile on the same port counts
	// even before it has bound it (ComfyUI takes a while to load), so two setups of one app
	// are never started together.
	if !ours {
		for _, port := range p.Ports {
			if other, state := s.portOwner(name, port); other != "" {
				pl.Conflicts = append(pl.Conflicts, fmt.Sprintf("port %d is used by profile %s, which is %s; stop it first", port, other, state))
				continue
			}
			if !portInUse(port) {
				continue
			}
			msg := fmt.Sprintf("port %d is in use by another process", port)
			if pid, err := procinfo.ListenerPID(port); err == nil {
				pl.ForeignPID, pl.ForeignPath = pid, procinfo.Path(pid)
				msg = fmt.Sprintf("port %d is in use by pid %d", port, pid)
				if pl.ForeignPath != "" {
					msg += " (" + pl.ForeignPath + ")"
				}
			}
			pl.Conflicts = append(pl.Conflicts, msg)
		}
	}

	switch action {
	case "start":
		if ours {
			pl.Blocking = append(pl.Blocking, fmt.Sprintf("%s is already %s", name, st.State))
		}
		if err == nil && !commandExists(res.Command, p.PathPrepend) {
			pl.Blocking = append(pl.Blocking, "command not found: "+pl.Command)
		}
		if err == nil && res.Cwd != "" {
			if fi, err := os.Stat(res.Cwd); err != nil || !fi.IsDir() {
				pl.Blocking = append(pl.Blocking, "working directory not found: "+pl.Cwd)
			}
		}
		for _, rp := range p.RequiresPaths {
			if _, err := os.Stat(rp); err != nil {
				pl.Blocking = append(pl.Blocking, rp+" is not available (is its drive mounted?)")
			}
		}
		if !ours && len(pl.Conflicts) > 0 {
			pl.Blocking = append(pl.Blocking, pl.Conflicts...)
		}
	case "stop":
		pl.StopTimeout = p.Stop.Timeout.String()
		if !ours {
			pl.Blocking = append(pl.Blocking, fmt.Sprintf("%s is not running under the broker", name))
		}
	case "update":
		if len(p.Update) == 0 {
			pl.Blocking = append(pl.Blocking, "the profile has no update steps")
		}
		if ours {
			pl.Blocking = append(pl.Blocking, fmt.Sprintf("%s is %s; stop it before updating", name, st.State))
		}
		// Profiles sharing a port are setups of one app, which often share its checkout
		// and models too: do not update one under another that is running.
		for _, port := range p.Ports {
			if other, state := s.portOwner(name, port); other != "" {
				pl.Blocking = append(pl.Blocking, fmt.Sprintf("profile %s (same port %d) is %s; stop it before updating", other, port, state))
				break
			}
		}
	default:
		return Plan{}, fmt.Errorf("unknown action %q (want start, stop or update)", action)
	}

	b, _ := json.Marshal([]any{pl.Action, pl.Command, pl.Args, pl.Cwd, pl.EnvKeys, pl.PathPrepend, pl.Ports, pl.Bind, pl.Steps})
	pl.Hash = audit.HashArgs(b)
	return pl, nil
}

// Summary is a one-line description for approval prompts.
func (pl Plan) Summary() string {
	switch pl.Action {
	case "update":
		return fmt.Sprintf("update %s: %d step(s), first: %s", pl.Profile, len(pl.Steps), strings.Join(first(pl.Steps), " "))
	case "stop":
		return fmt.Sprintf("stop %s", pl.Profile)
	}
	where := ""
	if pl.Bind != "" {
		where = " listening on " + pl.Bind
	}
	if len(pl.Ports) > 0 {
		where += fmt.Sprintf(" (ports %v)", pl.Ports)
	}
	exp := ""
	if pl.NetworkExposed {
		exp = " NETWORK EXPOSED."
	}
	return fmt.Sprintf("start %s: %s%s.%s", pl.Profile, filepath.Base(pl.Command), where, exp)
}

func first(steps [][]string) []string {
	if len(steps) == 0 {
		return nil
	}
	return steps[0]
}

// Text renders the plan for people: what runs, where, with which environment.
func (pl Plan) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s (currently %s)\n", pl.Action, pl.Profile, pl.State)
	if pl.Action == "start" {
		fmt.Fprintf(&b, "command: %s\n", pl.Command)
		if len(pl.Args) > 0 {
			fmt.Fprintf(&b, "args:    %s\n", strings.Join(pl.Args, " "))
		}
		if pl.Cwd != "" {
			fmt.Fprintf(&b, "cwd:     %s\n", pl.Cwd)
		}
		if len(pl.EnvKeys) > 0 {
			fmt.Fprintf(&b, "env:     %s\n", strings.Join(pl.EnvKeys, ", "))
		}
		if len(pl.PathPrepend) > 0 {
			fmt.Fprintf(&b, "path:    %s (prepended)\n", strings.Join(pl.PathPrepend, string(os.PathListSeparator)))
		}
		if len(pl.Ports) > 0 {
			fmt.Fprintf(&b, "ports:   %v\n", pl.Ports)
		}
		bind := pl.Bind
		if bind == "" {
			bind = "(not set in args)"
		}
		fmt.Fprintf(&b, "bind:    %s, network exposed: %v\n", bind, pl.NetworkExposed)
	}
	for i, s := range pl.Steps {
		fmt.Fprintf(&b, "step %d:  %s\n", i+1, strings.Join(s, " "))
	}
	for _, c := range pl.Blocking {
		fmt.Fprintf(&b, "BLOCKED: %s\n", c)
	}
	for _, c := range pl.Conflicts {
		if !slices.Contains(pl.Blocking, c) {
			fmt.Fprintf(&b, "note:    %s\n", c)
		}
	}
	return b.String()
}

// portOwner returns another profile with a live run that uses port.
func (s *Supervisor) portOwner(self string, port int) (string, State) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, r := range s.runs {
		if name == self {
			continue
		}
		if st := r.getState(); (st.running() || st == Updating) && slices.Contains(r.p.Ports, port) {
			return name, st
		}
	}
	return "", ""
}
