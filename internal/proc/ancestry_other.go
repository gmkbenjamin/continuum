//go:build !darwin && !linux

package proc

// Ancestry returns pid and its ancestors (parent and name of each) from one
// process snapshot (Windows reads it from the kernel; elsewhere it is one ps).
func Ancestry(pid int) map[int]Node {
	tree := Tree()
	return walkUp(pid, func(p int) (Node, bool) {
		n, ok := tree[p]
		return n, ok
	})
}
