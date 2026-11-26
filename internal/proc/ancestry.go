package proc

import (
	"path/filepath"
	"strings"
)

// kernelAncestry walks up from pid with Parent and Name.
func kernelAncestry(pid int) map[int]Node {
	return walkUp(pid, func(p int) (Node, bool) {
		ppid, name := Parent(p), Name(p)
		return Node{PPID: ppid, Name: name}, ppid != 0 || name != ""
	})
}

// walkUp collects pid and its ancestors as node reports them.
func walkUp(pid int, node func(int) (Node, bool)) map[int]Node {
	out := map[int]Node{}
	for steps := 0; pid > 1 && steps < 64; steps++ {
		if _, seen := out[pid]; seen {
			break // a loop (a reused parent id)
		}
		n, ok := node(pid)
		if !ok {
			break // gone, or not ours to see
		}
		out[pid] = n
		pid = n.PPID
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
