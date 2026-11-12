//go:build !windows

package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func hideWindow(cmd *exec.Cmd) {}

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// Alive reports whether pid exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// Info returns the parent pid and command name of pid.
func Info(pid int) (ppid int, name string, ok bool) {
	out, err := Run(2*time.Second, "", "ps", "-o", "ppid=", "-o", "comm=", "-p", strconv.Itoa(pid))
	if err != nil {
		return 0, "", false
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) < 2 {
		return 0, "", false
	}
	ppid, _ = strconv.Atoi(fields[0])
	name = strings.Join(fields[1:], " ")
	return ppid, filepath.Base(name), true
}

func extraDirs(home string) []string {
	return []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, "bin"),
		filepath.Join(home, ".npm-global", "bin"),
		filepath.Join(home, ".bun", "bin"),
		filepath.Join(home, ".cargo", "bin"),
		filepath.Join(home, "go", "bin"),
		"/opt/homebrew/bin",
		"/usr/local/bin",
		"/usr/bin",
		"/bin",
		"/snap/bin",
	}
}

func candidates(dir, name string) []string { return []string{filepath.Join(dir, name)} }

// Process is one entry of List.
type Process struct {
	PID  int
	Args []string // argv split on spaces (good enough for matching names and ids)
	Line string
}

// List returns every process with its command line.
func List() ([]Process, error) {
	out, err := Run(5*time.Second, "", "ps", "-axww", "-o", "pid=", "-o", "args=")
	if err != nil {
		return nil, err
	}
	var ps []Process
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		sp := strings.IndexByte(line, ' ')
		if sp <= 0 {
			continue
		}
		pid, err := strconv.Atoi(line[:sp])
		if err != nil {
			continue
		}
		args := strings.TrimSpace(line[sp+1:])
		ps = append(ps, Process{PID: pid, Args: strings.Fields(args), Line: args})
	}
	return ps, nil
}

// Cwd returns the working directory of pid.
func Cwd(pid int) string {
	if p, err := os.Readlink("/proc/" + strconv.Itoa(pid) + "/cwd"); err == nil {
		return p
	}
	out, err := Run(5*time.Second, "", "lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn")
	if err != nil && out == "" {
		return ""
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "n") {
			return strings.TrimSpace(l[1:])
		}
	}
	return ""
}

// TTY returns the controlling terminal of pid, e.g. /dev/ttys003.
func TTY(pid int) string {
	out, err := Run(2*time.Second, "", "ps", "-o", "tty=", "-p", strconv.Itoa(pid))
	if err != nil {
		return ""
	}
	t := strings.TrimSpace(out)
	if t == "" || t == "?" || t == "??" {
		return ""
	}
	if !strings.HasPrefix(t, "/dev/") {
		t = "/dev/" + t
	}
	return t
}

// ForegroundCommand returns the command line of the foreground process on a
// terminal (the one marked "+" by ps), e.g. "node /…/gemini-cli/dist/index.js".
func ForegroundCommand(tty string) string {
	t := strings.TrimPrefix(tty, "/dev/")
	if t == "" {
		return ""
	}
	out, err := Run(5*time.Second, "", "ps", "-ww", "-t", t, "-o", "stat=", "-o", "args=")
	if err != nil {
		return ""
	}
	last := ""
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) >= 2 && strings.Contains(f[0], "+") {
			last = strings.Join(f[1:], " ")
		}
	}
	return last
}

// State returns ps's STAT field for pid, e.g. "S+" (sleeping, in the
// foreground of its terminal) or "T" (stopped, as after Ctrl+Z). "" when
// unknown.
func State(pid int) string {
	out, err := Run(2*time.Second, "", "ps", "-o", "stat=", "-p", strconv.Itoa(pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// StartTime returns when pid started (to the second).
func StartTime(pid int) (time.Time, bool) {
	out, err := Run(2*time.Second, "", "ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	if err != nil {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(strings.Fields(out), " "), time.Local)
	return t, err == nil
}

// Node is one process in Tree.
type Node struct {
	PPID int
	Name string
}

// Tree returns every process's parent and command name, from one ps call.
func Tree() map[int]Node {
	out, err := Run(5*time.Second, "", "ps", "-axo", "pid=,ppid=,comm=")
	if err != nil {
		return nil
	}
	tree := map[int]Node{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		tree[pid] = Node{PPID: ppid, Name: filepath.Base(strings.Join(f[2:], " "))}
	}
	return tree
}
