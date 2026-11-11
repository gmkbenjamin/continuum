//go:build windows

package wrapper

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// conPTY runs the child in a Windows pseudo console (Windows 10 1809+).
type conPTY struct {
	hpc     windows.Handle
	inWrite *os.File
	outRead *os.File
	process windows.Handle
	closed  bool
}

func start(argv []string, env []string, cols, rows int) (ptyProcess, error) {
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
		return nil, err
	}
	// The pseudo console owns these ends now.
	windows.CloseHandle(inRead)
	windows.CloseHandle(outWrite)

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		return nil, err
	}
	defer attrs.Delete()
	// The attribute value is the HPCON itself, not a pointer to it (see the
	// CreatePseudoConsole documentation), hence the conversion.
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, unsafe.Pointer(hpc), unsafe.Sizeof(hpc)); err != nil {
		windows.ClosePseudoConsole(hpc)
		return nil, err
	}
	si := new(windows.StartupInfoEx)
	si.Cb = uint32(unsafe.Sizeof(*si))
	si.ProcThreadAttributeList = attrs.List()

	path, err := exec.LookPath(argv[0])
	if err != nil {
		windows.ClosePseudoConsole(hpc)
		return nil, err
	}
	cmdline := makeCmdLine(append([]string{path}, argv[1:]...))
	cmd16, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return nil, err
	}
	envBlock := makeEnvBlock(env)
	pi := new(windows.ProcessInformation)
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT)
	if err := windows.CreateProcess(nil, cmd16, nil, nil, false, flags, &envBlock[0], nil, &si.StartupInfo, pi); err != nil {
		windows.ClosePseudoConsole(hpc)
		return nil, err
	}
	windows.CloseHandle(pi.Thread)
	return &conPTY{
		hpc:     hpc,
		inWrite: os.NewFile(uintptr(inWrite), "conpty-in"),
		outRead: os.NewFile(uintptr(outRead), "conpty-out"),
		process: pi.Process,
	}, nil
}

func makeCmdLine(args []string) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = syscall.EscapeArg(a)
	}
	return strings.Join(parts, " ")
}

func makeEnvBlock(env []string) []uint16 {
	var block []uint16
	for _, kv := range env {
		u, err := windows.UTF16FromString(kv)
		if err != nil {
			continue
		}
		block = append(block, u...)
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
