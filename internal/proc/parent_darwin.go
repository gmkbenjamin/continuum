//go:build darwin

package proc

import "golang.org/x/sys/unix"

// Parent returns pid's parent from the kernel, without starting a program
// (ps can take seconds on a busy computer). 0 if unknown.
func Parent(pid int) int {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || kp == nil || kp.Proc.P_pid != int32(pid) {
		return 0
	}
	return int(kp.Eproc.Ppid)
}
