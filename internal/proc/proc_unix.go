//go:build !windows

package proc

import (
	"syscall"
	"time"
)

type osProc struct {
	pgid int
}

// JoinKillOnCloseJob is a no-op outside Windows: Linux children get Pdeathsig, and the broker
// kills owned process groups on shutdown (KillAll).
func JoinKillOnCloseJob() error { return nil }

func (p *Process) start(Options) error {
	if p.Cmd.SysProcAttr == nil {
		p.Cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	p.Cmd.SysProcAttr.Setpgid = true
	setPdeathsig(p.Cmd.SysProcAttr)
	if err := p.Cmd.Start(); err != nil {
		return err
	}
	p.os.pgid = p.Cmd.Process.Pid // leader of its own group (Setpgid)
	return nil
}

func (p *Process) kill() {
	// A negative pid addresses the whole process group.
	syscall.Kill(-p.os.pgid, syscall.SIGKILL)
}

// adopt checks that pid is alive and still leads its own process group, as every process
// the broker starts does. Start times are not compared on Unix.
func (p *Process) adopt(time.Time) bool {
	if !isAlive(p.pid) {
		return false
	}
	if pgid, err := syscall.Getpgid(p.pid); err != nil || pgid != p.pid {
		return false
	}
	p.os.pgid = p.pid
	return true
}

// isAlive treats a zombie (exited, not yet reaped) as dead.
func isAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil && !zombie(pid)
}

// RunHelper is a no-op outside Windows: signals reach process groups directly.
func RunHelper() {}

func (p *Process) interrupt() error { return syscall.Kill(-p.os.pgid, syscall.SIGTERM) }

// KillForeign terminates a process the broker did not start. Only for an approved
// kill_foreign. Its group is left alone: it may be the user's shell.
func KillForeign(pid int) {
	syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 50 && isAlive(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if isAlive(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
	}
}
