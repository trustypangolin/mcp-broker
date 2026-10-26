//go:build !windows && !linux && !darwin

package alive

func zombie(int) bool { return false }
