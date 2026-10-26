//go:build !windows

package proc

import (
	"syscall"
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
