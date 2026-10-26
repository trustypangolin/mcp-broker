//go:build !windows

// Package alive reports whether a PID is a running process, for process-tree tests.
package alive

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// PID reports whether pid is running. Zombies awaiting reaping count as dead.
func PID(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	if data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		fields := strings.Fields(string(data))
		return len(fields) < 3 || fields[2] != "Z"
	}
	return true
}
