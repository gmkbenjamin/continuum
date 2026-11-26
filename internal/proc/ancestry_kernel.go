//go:build darwin || linux

package proc

// Ancestry returns pid and its ancestors (parent and name of each), read
// from the kernel without starting a program: unlike Tree it doesn't run ps,
// which can take seconds on a busy or monitored computer. For code that only
// walks up from pid, it answers as Tree does.
func Ancestry(pid int) map[int]Node { return kernelAncestry(pid) }
