//go:build windows

package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
	createNoWindow        = 0x08000000
)

func hideWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNewProcessGroup | detachedProcess}
}

// Alive reports whether pid exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == 259 // STILL_ACTIVE
}

// Info returns the parent pid and executable name of pid.
func Info(pid int) (ppid int, name string, ok bool) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return 0, "", false
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if int(e.ProcessID) == pid {
			return int(e.ParentProcessID), windows.UTF16ToString(e.ExeFile[:]), true
		}
	}
	return 0, "", false
}

func extraDirs(home string) []string {
	var dirs []string
	if home != "" {
		dirs = append(dirs,
			filepath.Join(home, ".local", "bin"),
			filepath.Join(home, "AppData", "Roaming", "npm"),
			filepath.Join(home, "AppData", "Local", "Programs"),
			filepath.Join(home, ".bun", "bin"),
		)
	}
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		dirs = append(dirs, filepath.Join(localAppData, "Microsoft", "WinGet", "Links"))
	}
	return dirs
}

func candidates(dir, name string) []string {
	return []string{
		filepath.Join(dir, name+".exe"),
		filepath.Join(dir, name+".cmd"),
		filepath.Join(dir, name),
	}
}

// Process is one entry of List.
type Process struct {
	PID  int
	Args []string
	Line string
}

// List returns every process. Windows does not expose other processes'
// command lines cheaply, so Args holds only the executable name.
func List() ([]Process, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	var ps []Process
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		name := windows.UTF16ToString(e.ExeFile[:])
		ps = append(ps, Process{PID: int(e.ProcessID), Args: []string{name}, Line: name})
	}
	return ps, nil
}

// Cwd is not available for other processes on Windows.
func Cwd(pid int) string { return "" }

// TTY is not meaningful on Windows.
func TTY(pid int) string { return "" }

// ForegroundCommand is not available on Windows.
func ForegroundCommand(tty string) string { return "" }

// State is not available on Windows.
func State(pid int) string { return "" }

// StartTime is not available on Windows.
func StartTime(pid int) (time.Time, bool) { return time.Time{}, false }

// Node is one process in Tree.
type Node struct {
	PPID int
	Name string
}

// Tree returns every process's parent and executable name, from one
// snapshot. Hooks use it to tell an agent started by another agent. A parent
// id may belong to a process that has exited (its id may even be reused), so
// callers bound their walks up the tree.
func Tree() map[int]Node {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	tree := map[int]Node{}
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		tree[int(e.ProcessID)] = Node{PPID: int(e.ParentProcessID), Name: windows.UTF16ToString(e.ExeFile[:])}
	}
	return tree
}
