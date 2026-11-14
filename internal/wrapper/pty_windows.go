//go:build windows

package wrapper

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

var procUpdateProcThreadAttribute = windows.NewLazySystemDLL("kernel32.dll").NewProc("UpdateProcThreadAttribute")

// conPTY runs the child in a Windows pseudo console (Windows 10 1809+).
type conPTY struct {
	hpc     windows.Handle
	inWrite *os.File
	outRead *os.File
	process windows.Handle
	closed  bool
}

func start(argv []string, env []string, cols, rows int) (ptyProcess, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
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
	size := windows.Coord{X: int16(cols), Y: int16(rows)}
	if err := windows.CreatePseudoConsole(size, inRead, outWrite, 0, &hpc); err != nil {
		windows.CloseHandle(inRead)
		windows.CloseHandle(inWrite)
		windows.CloseHandle(outRead)
		windows.CloseHandle(outWrite)
		return nil, err
	}
	// The pseudo console owns these ends now.
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
	// PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE takes the HPCON value directly as
	// lpValue, so call UpdateProcThreadAttribute with uintptr(hpc) rather than
	// converting a raw handle into an unsafe.Pointer.
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

	path, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, err
	}
	comspec := ""
	if ext := filepath.Ext(path); strings.EqualFold(ext, ".cmd") || strings.EqualFold(ext, ".bat") {
		comspec = os.Getenv("ComSpec")
		if comspec == "" {
			comspec = "cmd.exe"
		}
		if cs, err := exec.LookPath(comspec); err == nil {
			comspec = cs
		}
	}
	cmdline := buildCommandLine(path, argv, comspec)
	cmd16, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return nil, err
	}
	envBlock := makeEnvBlock(env)
	pi := new(windows.ProcessInformation)
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	if err := windows.CreateProcess(nil, cmd16, nil, nil, false, flags, &envBlock[0], nil, &si.StartupInfo, pi); err != nil {
		return nil, err
	}
	windows.CloseHandle(pi.Thread)
	started = true
	return &conPTY{
		hpc:     hpc,
		inWrite: os.NewFile(uintptr(inWrite), "conpty-in"),
		outRead: os.NewFile(uintptr(outRead), "conpty-out"),
		process: pi.Process,
	}, nil
}

func buildCommandLine(path string, argv []string, comspec string) string {
	inner := makeCmdLine(append([]string{path}, argv[1:]...))
	if ext := filepath.Ext(path); strings.EqualFold(ext, ".cmd") || strings.EqualFold(ext, ".bat") {
		if comspec == "" {
			comspec = "cmd.exe"
		}
		// cmd.exe /d /s /c "..." strips only the outermost quotes around the
		// command string. Passing that string through syscall.EscapeArg would
		// escape internal quotes and break paths/args that contain spaces.
		return syscall.EscapeArg(comspec) + ` /d /s /c "` + inner + `"`
	}
	return inner
}

func makeCmdLine(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = syscall.EscapeArg(a)
	}
	return strings.Join(parts, " ")
}

func envKey(kv string) (string, bool) {
	if kv == "" {
		return "", false
	}
	// Windows reserves leading '=' entries for per-drive current directories (=C:=...).
	start := 0
	if kv[0] == '=' {
		start = 1
	}
	eq := strings.IndexByte(kv[start:], '=')
	if eq <= 0 && start == 0 {
		return "", false
	}
	if eq < 0 {
		return "", false
	}
	return kv[:start+eq], true
}

// makeEnvBlock builds a sorted, case-insensitively deduplicated UTF-16
// environment block as required by CREATE_UNICODE_ENVIRONMENT. When duplicate
// keys appear, the last entry wins (matching os/exec.Cmd).
func makeEnvBlock(env []string) []uint16 {
	type item struct {
		upper []uint16
		kv    string
	}
	pos := make(map[string]int, len(env))
	items := make([]item, 0, len(env))
	for _, kv := range env {
		k, ok := envKey(kv)
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

func (c *conPTY) Read(p []byte) (int, error)  { return c.outRead.Read(p) }
func (c *conPTY) Write(p []byte) (int, error) { return c.inWrite.Write(p) }

func (c *conPTY) Resize(cols, rows int) error {
	return windows.ResizePseudoConsole(c.hpc, windows.Coord{X: int16(cols), Y: int16(rows)})
}

func (c *conPTY) Wait() (int, error) {
	if _, err := windows.WaitForSingleObject(c.process, windows.INFINITE); err != nil {
		return 1, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(c.process, &code); err != nil {
		return 1, err
	}
	return int(code), nil
}

func (c *conPTY) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	windows.ClosePseudoConsole(c.hpc)
	c.inWrite.Close()
	c.outRead.Close()
	windows.CloseHandle(c.process)
	return nil
}

// Windows has no SIGWINCH: poll the console size.
func watchResize(out *os.File, child ptyProcess) func() {
	done := make(chan struct{})
	go func() {
		lc, lr := 0, 0
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				c, r, err := term.GetSize(int(out.Fd()))
				if err == nil && (c != lc || r != lr) {
					lc, lr = c, r
					_ = child.Resize(c, r)
				}
			}
		}
	}()
	return func() { close(done) }
}

func (c *conPTY) OnStop(func()) {}

func suspendSelf() {}

// Helper is unused on Windows (no job control).
func Helper(argv []string) int { return 2 }

func (c *conPTY) Signal(os.Signal) {}

func forwardSignals(ptyProcess) func() { return func() {} }
