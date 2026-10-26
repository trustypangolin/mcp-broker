package proc

import "golang.org/x/sys/unix"

func zombie(pid int) bool {
	const sZomb = 5 // SZOMB in <sys/proc.h>
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return err == nil && kp.Proc.P_stat == sZomb
}
