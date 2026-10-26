//go:build !windows

// Package alive reports whether a PID is a running process, for process-tree tests.
package alive

import "syscall"

// PID reports whether pid is running. Zombies awaiting reaping count as dead.
func PID(pid int) bool {
	return syscall.Kill(pid, 0) == nil && !zombie(pid)
}
