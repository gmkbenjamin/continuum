//go:build !windows

package wrapper

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/term"
)

type unixPTY struct {
	f      *os.File
	cmd    *exec.Cmd
	onStop func()
	mu     sync.Mutex
	reaped bool
}

// start runs the agent through `continuum __pty-helper`, which owns the new
// terminal session the way a shell does and runs the agent as a job in it.
// Without that the agent's process group would be orphaned, and the kernel
// silently drops the SIGTSTP an agent sends itself on Ctrl+Z, leaving it
// half-suspended.
func start(argv []string, env []string, cols, rows int) (ptyProcess, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, append([]string{"__pty-helper", "--"}, argv...)...)
	cmd.Env = env
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		return nil, err
	}
	return &unixPTY{f: f, cmd: cmd}, nil
}

func (u *unixPTY) Read(p []byte) (int, error)  { return u.f.Read(p) }
func (u *unixPTY) Write(p []byte) (int, error) { return u.f.Write(p) }

func (u *unixPTY) signalChild(fn func(*os.Process)) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.reaped || u.cmd.Process == nil {
		return
	}
	fn(u.cmd.Process)
}

// Close closes the terminal and makes sure the helper (and so the agent) is
// not left suspended: a stopped process never sees the hang-up otherwise.
func (u *unixPTY) Close() error {
	u.signalChild(func(p *os.Process) {
		_ = syscall.Kill(p.Pid, syscall.SIGCONT)
		_ = syscall.Kill(p.Pid, syscall.SIGHUP)
	})
	return u.f.Close()
}

// Signal passes a signal the wrapper received (hang-up, terminate) on to the
// agent, waking it first in case it is suspended.
func (u *unixPTY) Signal(sig os.Signal) {
	u.signalChild(func(p *os.Process) {
		_ = syscall.Kill(p.Pid, syscall.SIGCONT)
		_ = p.Signal(sig)
	})
}

// forwardSignals relays hang-up and terminate to the agent instead of letting
// them kill the wrapper first.
func forwardSignals(child ptyProcess) func() {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGHUP, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case s := <-ch:
				child.Signal(s)
			}
		}
	}()
	return func() { signal.Stop(ch); close(done) }
}

func (u *unixPTY) Resize(cols, rows int) error {
	return pty.Setsize(u.f, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
}

func (u *unixPTY) OnStop(f func()) { u.onStop = f }

// Wait waits for the agent to exit. When it suspends itself (Ctrl+Z), the
// helper stops too; onStop then hands the terminal back to the user's shell,
// and once the user resumes the job (fg) the agent is continued.
func (u *unixPTY) Wait() (int, error) {
	pid := u.cmd.Process.Pid
	for {
		var ws syscall.WaitStatus
		_, err := syscall.Wait4(pid, &ws, syscall.WUNTRACED, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return 1, err
		}
		switch {
		case stopped(ws):
			if u.onStop != nil {
				u.onStop()
			}
			_ = syscall.Kill(pid, syscall.SIGCONT)
		case ws.Exited():
			u.mu.Lock()
			u.reaped = true
			u.mu.Unlock()
			return ws.ExitStatus(), nil
		case ws.Signaled():
			u.mu.Lock()
			u.reaped = true
			u.mu.Unlock()
			return 128 + int(ws.Signal()), nil
		}
	}
}

// suspendSelf stops this process like Ctrl+Z stops a job; it returns when
// the shell continues it (fg / bg).
func suspendSelf() { _ = syscall.Kill(os.Getpid(), syscall.SIGSTOP) }

// Helper is `continuum __pty-helper -- ARGV`: the session leader of the
// wrapper's terminal. It runs ARGV as the terminal's foreground job, mirrors
// the job's stops to its own parent, and exits with the job's status.
func Helper(argv []string) int {
	if len(argv) == 0 {
		return 2
	}
	path, err := exec.LookPath(argv[0])
	if err != nil {
		os.Stderr.WriteString("continuum: " + err.Error() + "\r\n")
		return 127
	}
	cmd := exec.Command(path, argv[1:]...)
	cmd.Args[0] = argv[0]
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Foreground: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		os.Stderr.WriteString("continuum: " + err.Error() + "\r\n")
		return 127
	}
	pid := cmd.Process.Pid
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT, syscall.SIGQUIT)
	go func() {
		for s := range sigs {
			_ = syscall.Kill(-pid, s.(syscall.Signal))
		}
	}()
	for {
		var ws syscall.WaitStatus
		_, err := syscall.Wait4(pid, &ws, syscall.WUNTRACED, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			return 1
		}
		switch {
		case stopped(ws):
			// Tell the wrapper by stopping too; it continues us on fg.
			_ = syscall.Kill(os.Getpid(), syscall.SIGSTOP)
			_ = syscall.Kill(-pid, syscall.SIGCONT)
		case ws.Exited():
			return ws.ExitStatus()
		case ws.Signaled():
			return 128 + int(ws.Signal())
		}
	}
}

func watchResize(out *os.File, child ptyProcess) func() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	done := make(chan struct{})
	go func() {
		for {
			select {
			case <-done:
				return
			case <-ch:
				if c, r, err := term.GetSize(int(out.Fd())); err == nil {
					_ = child.Resize(c, r)
				}
			}
		}
	}()
	return func() { signal.Stop(ch); close(done) }
}

// stopped reports a stopped child. syscall.WaitStatus.Stopped is not used:
// on BSD and macOS it ignores stops caused by SIGSTOP, which is exactly how
// the helper signals a suspended agent.
func stopped(ws syscall.WaitStatus) bool { return uint32(ws)&0xff == 0x7f }
