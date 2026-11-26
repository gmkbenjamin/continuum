//go:build !darwin && !linux

package proc

// Parent returns pid's parent. 0 if unknown.
func Parent(pid int) int {
	ppid, _, ok := Info(pid)
	if !ok {
		return 0
	}
	return ppid
}
