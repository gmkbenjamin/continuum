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

// Name returns pid's name as ps's comm column gives it: on macOS that is the
// process's argv[0] (so "claude" for Claude Code, whose executable is a
// versioned file, and "-zsh" for a login shell), read from the kernel; the
// kernel's own short name when argv can't be read. "" if unknown.
func Name(pid int) string {
	if args := Args(pid); len(args) > 0 && args[0] != "" {
		return baseName(args[0])
	}
	if kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid); err == nil && kp != nil && kp.Proc.P_pid == int32(pid) {
		b := kp.Proc.P_comm[:]
		if i := indexNUL(b); i >= 0 {
			b = b[:i]
		}
		return string(b)
	}
	return ""
}
