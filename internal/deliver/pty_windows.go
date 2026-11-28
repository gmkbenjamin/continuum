//go:build windows

package deliver

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procUpdateProcThreadAttribute = windows.NewLazySystemDLL("kernel32.dll").NewProc("UpdateProcThreadAttribute")

type conPTYRW struct {
	inWrite *os.File
	outRead *os.File
}

func (c *conPTYRW) Read(p []byte) (int, error)  { return c.outRead.Read(p) }
func (c *conPTYRW) Write(p []byte) (int, error) { return c.inWrite.Write(p) }
func (c *conPTYRW) Close() error {
	e1 := c.inWrite.Close()
	e2 := c.outRead.Close()
	if e1 != nil {
		return e1
	}
	return e2
}

type ptyChild struct {
	hpc          windows.Handle
	process      windows.Handle
	job          windows.Handle
	closeConOnce sync.Once
	stopOnce     sync.Once
	rw           interface {
		Read([]byte) (int, error)
		Write([]byte) (int, error)
		Close() error
	}
}

func (c *ptyChild) closeConsole() {
	c.closeConOnce.Do(func() {
		windows.ClosePseudoConsole(c.hpc)
	})
}

func attachKillOnCloseJob(procHandle windows.Handle) windows.Handle {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		_ = windows.CloseHandle(job)
		return 0
	}
	if err := windows.AssignProcessToJobObject(job, procHandle); err != nil {
		_ = windows.CloseHandle(job)
		return 0
	}
	return job
}

// startPTY launches argv inside a Windows ConPTY pseudo console (Windows 10
// 1809+), enabling interactive native attach delivery (e.g. claude-attach) on
// Windows without opening a visible window.
func startPTY(argv []string, dir string, env []string) (*ptyChild, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, err
	}
	comspec := ""
	if isBatchFile(path) {
		comspec = os.Getenv("ComSpec")
		if comspec == "" {
			comspec = "cmd.exe"
		}
		if cs, err := exec.LookPath(comspec); err == nil {
			comspec = cs
		}
	}
	cmdline, err := buildWinCmdLine(path, argv, comspec)
	if err != nil {
		return nil, err
	}
	cmd16, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return nil, err
	}
	var dir16 *uint16
	if dir != "" {
		dir16, err = windows.UTF16PtrFromString(dir)
		if err != nil {
			return nil, err
		}
	}

	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, err
	}
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		return nil, err
	}
	var hpc windows.Handle
	size := windows.Coord{X: 120, Y: 40}
	if err := windows.CreatePseudoConsole(size, inRead, outWrite, 0, &hpc); err != nil {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		windows.CloseHandle(outWrite)
		return nil, err
	}
	windows.CloseHandle(inRead)
	windows.CloseHandle(outWrite)

	started := false
	defer func() {
		if started {
			return
		}
		windows.ClosePseudoConsole(hpc)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
	}()

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return nil, err
	}
	defer attrs.Delete()
	r1, _, uerr := procUpdateProcThreadAttribute.Call(
		uintptr(unsafe.Pointer(attrs.List())),
		0,
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		uintptr(hpc),
		unsafe.Sizeof(hpc),
		0,
		0,
	)
	if r1 == 0 {
		return nil, uerr
	}
	si := new(windows.StartupInfoEx)
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.ProcThreadAttributeList = attrs.List()

	envBlock := makeWinEnvBlock(append(append([]string(nil), env...), "TERM=xterm-256color"))
	pi := new(windows.ProcessInformation)
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	if err := windows.CreateProcess(nil, cmd16, nil, nil, false, flags, &envBlock[0], dir16, &si.StartupInfo, pi); err != nil {
		return nil, err
	}
	windows.CloseHandle(pi.Thread)
	started = true

	child := &ptyChild{
		hpc:     hpc,
		process: pi.Process,
		job:     attachKillOnCloseJob(pi.Process),
		rw: &conPTYRW{
			inWrite: os.NewFile(uintptr(inWrite), "conpty-in"),
			outRead: os.NewFile(uintptr(outRead), "conpty-out"),
		},
	}
	// Close the pseudo console as soon as the child exits so conhost.exe
	// releases outWrite and attach.go's Read loop observes EOF immediately.
	go func() {
		_, _ = windows.WaitForSingleObject(child.process, windows.INFINITE)
		child.closeConsole()
	}()
	return child, nil
}

func (c *ptyChild) stop() {
	if c == nil {
		return
	}
	c.stopOnce.Do(func() {
		// Closing the pseudo console sends CTRL_CLOSE_EVENT to attached console
		// clients (Windows' equivalent of closing a terminal window / SIGHUP).
		c.closeConsole()
		res, err := windows.WaitForSingleObject(c.process, 3000)
		if err != nil || res == uint32(windows.WAIT_TIMEOUT) {
			_ = windows.TerminateProcess(c.process, 1)
			_, _ = windows.WaitForSingleObject(c.process, 1000)
		}
		if c.job != 0 {
			_ = windows.CloseHandle(c.job)
		}
		_ = windows.CloseHandle(c.process)
		_ = c.rw.Close()
	})
}

func winEnvKey(kv string) (string, bool) {
	if kv == "" {
		return "", false
	}
	start := 0
	if kv[0] == '=' {
		start = 1
	}
	eq := strings.IndexByte(kv[start:], '=')
	if (eq <= 0 && start == 0) || eq < 0 {
		return "", false
	}
	return kv[:start+eq], true
}

func makeWinEnvBlock(env []string) []uint16 {
	type item struct {
		upper []uint16
		kv    string
	}
	pos := make(map[string]int, len(env))
	items := make([]item, 0, len(env))
	for _, kv := range env {
		k, ok := winEnvKey(kv)
		if !ok || strings.IndexByte(kv, 0) >= 0 {
			continue
		}
		uk := strings.ToUpper(k)
		if i, exists := pos[uk]; exists {
			items[i].kv = kv
			continue
		}
		pos[uk] = len(items)
		items = append(items, item{upper: utf16.Encode([]rune(uk)), kv: kv})
	}
	slices.SortFunc(items, func(a, b item) int {
		return slices.Compare(a.upper, b.upper)
	})
	if len(items) == 0 {
		return []uint16{0, 0}
	}
	var block []uint16
	for _, it := range items {
		u, err := windows.UTF16FromString(it.kv)
		if err != nil {
			continue
		}
		block = append(block, u...)
	}
	if len(block) == 0 {
		return []uint16{0, 0}
	}
	return append(block, 0)
}
