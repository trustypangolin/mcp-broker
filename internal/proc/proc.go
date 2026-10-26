// Package proc starts child processes the broker owns for their whole life, child tree
// included: a Job Object per process on Windows, a process group elsewhere. Kill always
// takes the whole tree down.
package proc

import (
	"os/exec"
	"sync"
)

// Process is a started, owned child.
type Process struct {
	Cmd *exec.Cmd
	os  osProc

	once sync.Once
}

// Options adjust how a process is started.
type Options struct {
	Console bool // Windows: give it a visible console instead of CREATE_NO_WINDOW
}

var (
	liveMu sync.Mutex
	live   = map[*Process]struct{}{}
)

// Start starts cmd as an owned process. cmd must be fully configured (pipes, env, dir) and
// not yet started.
func Start(cmd *exec.Cmd, opts Options) (*Process, error) {
	p := &Process{Cmd: cmd}
	if err := p.start(opts); err != nil {
		return nil, err
	}
	liveMu.Lock()
	live[p] = struct{}{}
	liveMu.Unlock()
	return p, nil
}

// PID returns the root process ID.
func (p *Process) PID() int { return p.Cmd.Process.Pid }

// Kill terminates the whole tree. It is safe to call more than once and after exit.
func (p *Process) Kill() {
	p.once.Do(func() {
		p.kill()
		liveMu.Lock()
		delete(live, p)
		liveMu.Unlock()
	})
}

// KillAll kills every owned process; the broker calls it on shutdown. On Windows the
// broker's own kill-on-close job (see JoinKillOnCloseJob) is the backstop if it crashes.
func KillAll() {
	liveMu.Lock()
	ps := make([]*Process, 0, len(live))
	for p := range live {
		ps = append(ps, p)
	}
	liveMu.Unlock()
	for _, p := range ps {
		p.Kill()
	}
}
