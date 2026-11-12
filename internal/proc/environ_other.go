//go:build !darwin && !linux

package proc

// Environ is not available on this OS.
func Environ(pid int) map[string]string { return nil }
