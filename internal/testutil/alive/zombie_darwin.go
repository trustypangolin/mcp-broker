package alive

import "golang.org/x/sys/unix"

func zombie(pid int) bool {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	return err == nil && kp.Proc.P_stat == 5 // SZOMB
}
