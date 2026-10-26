// Package proc starts child processes the broker owns for their whole life, child tree
// included: a Job Object per process on Windows, a process group elsewhere. Kill always
// takes the whole tree down.
package proc

import (
	"os/exec"
	"sync"
	"time"
)

// Process is a started, owned child, or one adopted after a broker restart (Cmd nil).
type Process struct {
	Cmd *exec.Cmd
	pid int
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
	p.pid = cmd.Process.Pid
	track(p)
	return p, nil
}

func track(p *Process) {
	liveMu.Lock()
	live[p] = struct{}{}
	liveMu.Unlock()
}

// Adopt takes ownership of a running process the broker started before it restarted,
// given the start time it recorded. It reports false if pid is gone or now belongs to a
// different process.
func Adopt(pid int, started time.Time) (*Process, bool) {
	p := &Process{pid: pid}
	if !p.adopt(started) {
		return nil, false
	}
	track(p)
	return p, true
}

// PID returns the root process ID.
func (p *Process) PID() int { return p.pid }

// Alive reports whether the root process is still running.
func (p *Process) Alive() bool { return isAlive(p.pid) }

// Interrupt asks the tree to shut down: CTRL_BREAK to its process group on Windows,
// SIGTERM to the group elsewhere. Follow it with Kill after a timeout.
func (p *Process) Interrupt() error { return p.interrupt() }

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
