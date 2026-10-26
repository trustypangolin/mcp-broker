//go:build !windows && !linux

package proc

import "syscall"

// macOS has no Pdeathsig; KillAll on broker shutdown covers it.
func setPdeathsig(*syscall.SysProcAttr) {}
