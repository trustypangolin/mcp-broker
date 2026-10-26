// Package supervisor runs profiles: long-lived local apps such as ComfyUI. It shows a plan
// before anything starts, owns each run's whole process tree from spawn to exit, decides
// readiness from health checks rather than a blocking call, and always stops what it
// started.
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/audit"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/proc"
	"github.com/trustypangolin/mcp-broker/internal/redact"
)

// State of a profile.
type State string

const (
	Stopped   State = "stopped"
	Starting  State = "starting"
	Ready     State = "ready"
	Unhealthy State = "unhealthy"
	Stopping  State = "stopping"
	Exited    State = "exited" // ended by itself
	Updating  State = "updating"
)

// running reports whether a state has a live process.
func (s State) running() bool {
	return s == Starting || s == Ready || s == Unhealthy || s == Stopping
}

// Status is what status(profile) reports.
type Status struct {
	Profile     string    `json:"profile"`
	Description string    `json:"description,omitempty"`
	State       State     `json:"state"`
	RunID       string    `json:"run_id,omitempty"`
	PID         int       `json:"pid,omitempty"`
	Started     time.Time `json:"started,omitzero"`
	Uptime      string    `json:"uptime,omitempty"`
	Ports       []int     `json:"ports,omitempty"`
	Health      string    `json:"health,omitempty"` // last health result
	ExitCode    *int      `json:"exit_code,omitempty"`
	Error       string    `json:"error,omitempty"`
	Adopted     bool      `json:"adopted,omitempty"` // started by an earlier broker
}

// Options configure a Supervisor.
type Options struct {
	Config   *config.Config
	Redactor *redact.Redactor
	Audit    *audit.Log
	Logger   *slog.Logger
	// LogDir receives profile-<name>.log per profile.
	LogDir string
	// StateFile records runs so a restarted broker can find them.
	StateFile string
	// OnChange, if set, hears about every state change after start (ready, unhealthy,
	// exited, stopped), for the tray's notifications.
	OnChange func(Status)
}

// Supervisor runs profiles. Safe for concurrent use.
type Supervisor struct {
	opts Options
	cfg  atomic.Pointer[config.Config]
	log  *slog.Logger

	mu   sync.Mutex
	runs map[string]*run // by profile; finished runs stay until the next start
}

// ringSize is how many output lines each run keeps in memory.
const ringSize = 5000

type run struct {
	id      string
	profile string
	p       config.Profile
	proc    *proc.Process
	cmd     *exec.Cmd // nil for adopted runs
	out     *lines
	started time.Time
	adopted bool

	mu       sync.Mutex
	state    State
	health   string
	exitCode *int
	err      string
	stopping bool
	done     chan struct{} // closed when the process has exited
	cancel   context.CancelFunc
}

func (r *run) set(f func(r *run)) {
	r.mu.Lock()
	f(r)
	r.mu.Unlock()
}

func (r *run) getState() State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

// New returns a Supervisor and re-adopts runs a previous broker left behind.
func New(o Options) *Supervisor {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Redactor == nil {
		o.Redactor = redact.New()
	}
	s := &Supervisor{opts: o, log: o.Logger, runs: map[string]*run{}}
	s.cfg.Store(o.Config)
	s.adoptAll()
	return s
}

// SetConfig switches to a reloaded config. Running profiles keep the definition they were
// started with until they are restarted.
func (s *Supervisor) SetConfig(cfg *config.Config) { s.cfg.Store(cfg) }

func (s *Supervisor) profile(name string) (config.Profile, bool) {
	p, ok := s.cfg.Load().Profiles[name]
	return p, ok
}

// Names lists the configured profiles.
func (s *Supervisor) Names() []string {
	return slices.Sorted(maps.Keys(s.cfg.Load().Profiles))
}

// Status reports one profile.
func (s *Supervisor) Status(name string) Status {
	p, _ := s.profile(name)
	st := Status{Profile: name, Description: p.Description, State: Stopped, Ports: p.Ports}
	s.mu.Lock()
	r := s.runs[name]
	s.mu.Unlock()
	if r == nil {
		return st
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	st.State, st.RunID, st.Health, st.ExitCode, st.Error, st.Adopted = r.state, r.id, r.health, r.exitCode, r.err, r.adopted
	if r.proc != nil {
		st.PID = r.proc.PID()
	}
	if !r.started.IsZero() {
		st.Started = r.started
		if r.state.running() {
			st.Uptime = time.Since(r.started).Round(time.Second).String()
		}
	}
	if len(r.p.Ports) > 0 {
		st.Ports = r.p.Ports
	}
	return st
}

// List reports every configured profile.
func (s *Supervisor) List() []Status {
	var out []Status
	for _, n := range s.Names() {
		out = append(out, s.Status(n))
	}
	return out
}

// Logs returns the last n output lines of the profile's current or last run.
func (s *Supervisor) Logs(name string, n int) []string {
	s.mu.Lock()
	r := s.runs[name]
	s.mu.Unlock()
	if r == nil {
		return nil
	}
	return r.out.tail(n)
}

var (
	// ErrUnknown is returned for a profile that is not configured.
	ErrUnknown = errors.New("unknown profile")
	// ErrRunning is returned when starting or updating a profile that is running.
	ErrRunning = errors.New("already running")
	// ErrNotRunning is returned when stopping a profile the broker is not running.
	ErrNotRunning = errors.New("not running")
)

func newRunID() string {
	return "run_" + strings.ReplaceAll(time.Now().UTC().Format("20060102T150405.000"), ".", "")
}

// Start spawns the profile and returns at once with state starting; readiness comes from
// its health check. A plan with blocking conflicts is refused.
func (s *Supervisor) Start(name string) (Status, error) {
	p, ok := s.profile(name)
	if !ok {
		return Status{}, fmt.Errorf("%w %q", ErrUnknown, name)
	}
	pl, err := s.Plan(name, "start")
	if err != nil {
		return Status{}, err
	}
	if len(pl.Blocking) > 0 {
		return s.Status(name), fmt.Errorf("cannot start %s: %s", name, strings.Join(pl.Blocking, "; "))
	}
	if p.Detach {
		s.log.Warn("profile detach is not supported yet; the run stops with the broker", "profile", name)
	}

	r, err := s.spawn(name, p)
	if err != nil {
		s.audit(audit.Event{Kind: audit.KindProcessStart, Profile: name, Error: s.opts.Redactor.String(err.Error())})
		return s.Status(name), err
	}
	s.audit(audit.Event{Kind: audit.KindProcessStart, Profile: name, PID: r.proc.PID(), Detail: r.id})
	s.saveState()
	return s.Status(name), nil
}

func (s *Supervisor) spawn(name string, p config.Profile) (*run, error) {
	s.mu.Lock()
	if old := s.runs[name]; old != nil && (old.getState().running() || old.getState() == Updating) {
		s.mu.Unlock()
		return nil, fmt.Errorf("%s: %w", name, ErrRunning)
	}
	r := &run{id: newRunID(), profile: name, p: p, state: Starting, done: make(chan struct{}), started: time.Now()}
	s.runs[name] = r // claim the slot before releasing the lock
	s.mu.Unlock()

	fail := func(err error) (*run, error) {
		r.set(func(r *run) { r.state, r.err = Exited, s.opts.Redactor.String(err.Error()) })
		close(r.done)
		return nil, err
	}
	res, err := p.Resolve(s.opts.Redactor)
	if err != nil {
		return fail(err)
	}
	var ready *regexp.Regexp
	if p.Health.LogReadyRegex != "" {
		ready = regexp.MustCompile(p.Health.LogReadyRegex) // validated at load
	}
	r.out = newLines(ringSize, s.logFile(name), s.opts.Redactor, func(line string) {
		if ready != nil && ready.MatchString(line) {
			s.becomeReady(r, "log matched "+p.Health.LogReadyRegex)
		}
	})
	cmd := exec.Command(lookPath(res.Command, p.PathPrepend), res.Args...)
	cmd.Dir = res.Cwd
	cmd.Env = childEnv(res.Env, p.PathPrepend)
	cmd.Stdout, cmd.Stderr = r.out, r.out
	// Grandchildren that inherit the pipes must not keep Wait from returning once the root
	// exits; the tree is killed right after anyway.
	cmd.WaitDelay = 2 * time.Second
	pr, err := proc.Start(cmd, proc.Options{Console: p.Console})
	if err != nil {
		r.out.Close()
		return fail(err)
	}
	r.cmd, r.proc = cmd, pr
	r.out.add(fmt.Sprintf("[mcp-broker] started %s (pid %d)", r.id, pr.PID()))

	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go s.wait(r)
	go s.watchHealth(ctx, r)
	return r, nil
}

// childEnv is the broker's environment plus the profile's, with path_prepend in front of
// PATH. Windows environment names are case-insensitive, so Path and PATH are one entry.
func childEnv(extra map[string]string, prepend []string) []string {
	norm := func(k string) string {
		if os.PathSeparator == '\\' {
			return strings.ToUpper(k)
		}
		return k
	}
	env := map[string][2]string{} // normalised name → original name, value
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			env[norm(k)] = [2]string{k, v}
		}
	}
	for k, v := range extra {
		env[norm(k)] = [2]string{k, v}
	}
	if len(prepend) > 0 {
		pk := norm("PATH")
		cur := env[pk]
		if cur[0] == "" {
			cur[0] = "PATH"
		}
		parts := append(slices.Clone(prepend), cur[1])
		cur[1] = strings.Join(parts, string(os.PathListSeparator))
		env[pk] = cur
	}
	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, env[k][0]+"="+env[k][1])
	}
	return out
}

// lookPath resolves a bare command name against path_prepend first, as the child's PATH
// will; exec.Command alone would search only the broker's PATH.
func lookPath(command string, prepend []string) string {
	if filepath.IsAbs(command) || strings.ContainsAny(command, `/\`) {
		return command
	}
	exts := []string{""}
	if os.PathSeparator == '\\' {
		exts = []string{"", ".exe", ".cmd", ".bat", ".com"}
	}
	for _, dir := range prepend {
		for _, ext := range exts {
			p := filepath.Join(dir, command+ext)
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p
			}
		}
	}
	return command
}

func (s *Supervisor) wait(r *run) {
	err := r.cmd.Wait()
	code := r.cmd.ProcessState.ExitCode()
	r.proc.Kill() // the root exiting does not mean its children did
	s.finish(r, code, err)
}

func (s *Supervisor) finish(r *run, code int, err error) {
	r.cancel()
	r.set(func(r *run) {
		r.exitCode = &code
		if r.stopping {
			r.state = Stopped
		} else {
			r.state = Exited
			if err != nil && code < 0 {
				r.err = err.Error()
			}
		}
	})
	r.out.add(fmt.Sprintf("[mcp-broker] %s exited with code %d", r.id, code))
	r.out.Close()
	kind, detail := audit.KindExit, fmt.Sprintf("exit code %d", code)
	if r.getState() == Stopped {
		kind = audit.KindStop
	}
	s.audit(audit.Event{Kind: kind, Profile: r.profile, PID: r.proc.PID(), Detail: detail})
	s.log.Info("profile exited", "profile", r.profile, "run", r.id, "code", code)
	s.saveState()
	s.changed(r)
	close(r.done) // last: Stop returns on this, and the record must be gone by then
}

func (s *Supervisor) changed(r *run) {
	if s.opts.OnChange != nil {
		go s.opts.OnChange(s.Status(r.profile))
	}
}

func (s *Supervisor) becomeReady(r *run, why string) {
	changed := false
	r.set(func(r *run) {
		if r.state == Starting || r.state == Unhealthy {
			r.state, r.health, changed = Ready, why, true
		}
	})
	if changed {
		s.changed(r)
		s.audit(audit.Event{Kind: audit.KindHealth, Profile: r.profile, Decision: string(Ready), Detail: why, DurationMS: time.Since(r.started).Milliseconds()})
		s.log.Info("profile ready", "profile", r.profile, "after", time.Since(r.started).Round(time.Millisecond))
	}
}

// Stop stops a run the broker owns: the graceful signal, then the whole tree after
// stop.timeout. It returns once the process is gone.
func (s *Supervisor) Stop(ctx context.Context, name string) (Status, error) {
	s.mu.Lock()
	r := s.runs[name]
	s.mu.Unlock()
	if r == nil || !r.getState().running() {
		if _, ok := s.profile(name); !ok {
			return Status{}, fmt.Errorf("%w %q", ErrUnknown, name)
		}
		return s.Status(name), fmt.Errorf("%s: %w", name, ErrNotRunning)
	}
	r.set(func(r *run) { r.state, r.stopping = Stopping, true })
	graceful := r.p.Stop.Graceful
	if graceful == "" {
		graceful = "ctrl_break" // CTRL_BREAK on Windows, SIGTERM elsewhere: the same call
	}
	timeout := r.p.Stop.Timeout
	if graceful != "none" {
		if err := r.proc.Interrupt(); err != nil {
			s.log.Warn("graceful stop failed; killing", "profile", name, "err", err)
			timeout = 0
		}
	}
	r.out.add(fmt.Sprintf("[mcp-broker] stopping %s (%s, then kill after %s)", r.id, graceful, timeout))
	select {
	case <-r.done:
	case <-time.After(timeout):
		r.out.add("[mcp-broker] did not exit in time; killing the process tree")
		r.proc.Kill()
	case <-ctx.Done():
		r.proc.Kill()
	}
	select {
	case <-r.done:
	case <-time.After(10 * time.Second):
		return s.Status(name), fmt.Errorf("%s: process tree did not exit after kill", name)
	}
	return s.Status(name), nil
}

// Close stops every run (the broker is shutting down), each with its graceful sequence.
func (s *Supervisor) Close() {
	var wg sync.WaitGroup
	for _, n := range s.Names() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			s.Stop(ctx, n)
		}()
	}
	wg.Wait()
	s.mu.Lock()
	for _, r := range s.runs {
		if r.cancel != nil {
			r.cancel()
		}
	}
	s.mu.Unlock()
}

func (s *Supervisor) audit(e audit.Event) {
	if s.opts.Audit != nil {
		s.opts.Audit.Write(e)
	}
}

func (s *Supervisor) logFile(name string) *rotating {
	if s.opts.LogDir == "" {
		return nil
	}
	f, err := openRotating(filepath.Join(s.opts.LogDir, "profile-"+name+".log"))
	if err != nil {
		s.log.Warn("profile log file", "profile", name, "err", err)
		return nil
	}
	return f
}

// KillForeign kills the process holding the profile's port that the broker did not start.
// The caller must have had profile/<name>/kill_foreign approved, for this plan's PID.
func (s *Supervisor) KillForeign(name string, pid int) error {
	pl, err := s.Plan(name, "start")
	if err != nil {
		return err
	}
	if pl.ForeignPID == 0 || pl.ForeignPID != pid {
		return fmt.Errorf("pid %d no longer holds a port of %s", pid, name)
	}
	proc.KillForeign(pid)
	s.audit(audit.Event{Kind: audit.KindStop, Profile: name, PID: pid, Action: "kill_foreign", Detail: pl.ForeignPath})
	return nil
}
