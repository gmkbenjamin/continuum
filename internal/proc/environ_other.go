//go:build !darwin && !linux && !windows

package proc

// Environ is not available on this OS.
func Environ(pid int) map[string]string { return nil }

// Args is not available on this OS.
func Args(pid int) []string { return nil }
