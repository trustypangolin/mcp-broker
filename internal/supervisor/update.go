package supervisor

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/trustypangolin/mcp-broker/internal/audit"
	"github.com/trustypangolin/mcp-broker/internal/config"
	"github.com/trustypangolin/mcp-broker/internal/proc"
)

// updateStepTimeout bounds one update step (a git fetch or a pip install).
var updateStepTimeout = 30 * time.Minute

// Update runs the profile's update steps in order, in the background. It returns at once
// with state updating; the output goes to the profile's logs and the result to its status.
func (s *Supervisor) Update(name string) (Status, error) {
	p, ok := s.profile(name)
	if !ok {
		return Status{}, fmt.Errorf("%w %q", ErrUnknown, name)
	}
	pl, err := s.Plan(name, "update")
	if err != nil {
		return Status{}, err
	}
	if len(pl.Blocking) > 0 {
		return s.Status(name), fmt.Errorf("cannot update %s: %s", name, strings.Join(pl.Blocking, "; "))
	}
	res, err := p.Resolve(s.opts.Redactor)
	if err != nil {
		return s.Status(name), err
	}

	s.mu.Lock()
	if old := s.runs[name]; old != nil && (old.getState().running() || old.getState() == Updating) {
		s.mu.Unlock()
		return s.Status(name), fmt.Errorf("%s: %w", name, ErrRunning)
	}
	r := &run{id: newRunID(), profile: name, p: p, state: Updating, done: make(chan struct{}), started: time.Now(), cancel: func() {}}
	r.out = newLines(ringSize, s.logFile(name), s.opts.Redactor, nil)
	s.runs[name] = r
	s.mu.Unlock()
	s.audit(audit.Event{Kind: audit.KindProcessStart, Profile: name, Action: "update", Detail: r.id})

	go func() {
		defer close(r.done)
		defer r.out.Close()
		for i, step := range p.Update {
			var argv []string
			for _, tok := range step {
				v, _ := config.Expand(tok, res.Env) // checked by Plan
				argv = append(argv, v)
			}
			r.out.add(fmt.Sprintf("[mcp-broker] update step %d/%d: %s", i+1, len(p.Update), s.opts.Redactor.String(strings.Join(argv, " "))))
			code, err := s.runStep(r, argv, res.Cwd, res.Env, p.PathPrepend)
			if err != nil || code != 0 {
				msg := fmt.Sprintf("update step %d failed (exit code %d)", i+1, code)
				if err != nil {
					msg = fmt.Sprintf("update step %d failed: %s", i+1, s.opts.Redactor.String(err.Error()))
				}
				r.out.add("[mcp-broker] " + msg)
				r.set(func(r *run) { r.state, r.err, r.exitCode = Exited, msg, &code })
				s.audit(audit.Event{Kind: audit.KindExit, Profile: name, Action: "update", Error: msg})
				return
			}
		}
		zero := 0
		r.out.add("[mcp-broker] update finished")
		r.set(func(r *run) { r.state, r.health, r.exitCode = Stopped, "update finished", &zero })
		s.audit(audit.Event{Kind: audit.KindExit, Profile: name, Action: "update", Detail: "update finished"})
	}()
	return s.Status(name), nil
}

func (s *Supervisor) runStep(r *run, argv []string, cwd string, env map[string]string, prepend []string) (int, error) {
	cmd := exec.Command(lookPath(argv[0], prepend), argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = childEnv(env, prepend)
	cmd.Stdout, cmd.Stderr = r.out, r.out
	cmd.WaitDelay = 2 * time.Second
	pr, err := proc.Start(cmd, proc.Options{})
	if err != nil {
		return -1, err
	}
	r.set(func(r *run) { r.proc = pr })
	defer pr.Kill()
	ctx, cancel := context.WithTimeout(context.Background(), updateStepTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return cmd.ProcessState.ExitCode(), err
	case <-ctx.Done():
		pr.Kill()
		<-done
		return -1, fmt.Errorf("timed out after %s", updateStepTimeout)
	}
}
