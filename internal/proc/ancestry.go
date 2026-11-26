package proc

import (
	"path/filepath"
	"strings"
)

// Ancestry returns pid and its ancestors (parent and name of each), read
// from the kernel without starting a program: unlike Tree it doesn't run ps,
// which can take seconds on a busy or monitored computer. For code that only
// walks up from pid, it answers as Tree does.
func Ancestry(pid int) map[int]Node {
	out := map[int]Node{}
	for steps := 0; pid > 1 && steps < 64; steps++ {
		if _, seen := out[pid]; seen {
			break // a loop (a reused parent id)
		}
		ppid, name := Parent(pid), Name(pid)
		if ppid == 0 && name == "" {
			break // gone, or not ours to see
		}
		out[pid] = Node{PPID: ppid, Name: name}
		pid = ppid
	}
	return out
}

func baseName(path string) string { return filepath.Base(strings.TrimSpace(path)) }

func indexNUL(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}
