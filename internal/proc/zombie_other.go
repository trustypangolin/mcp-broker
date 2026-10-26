//go:build !windows && !linux && !darwin

package proc

func zombie(int) bool { return false }
