// Package alive reports whether a PID is a running process, for process-tree tests.
package alive

import "golang.org/x/sys/windows"

// PID reports whether pid is running.
func PID(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if windows.GetExitCodeProcess(h, &code) != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
