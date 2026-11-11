//go:build !windows

package deliver

import (
	"os/exec"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type ptyChild struct {
	cmd *exec.Cmd
	rw  interface {
		Read([]byte) (int, error)
		Write([]byte) (int, error)
		Close() error
	}
}

func startPTY(argv []string, dir string, env []string) (*ptyChild, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(env, "TERM=xterm-256color")
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 120, Rows: 40})
	if err != nil {
		return nil, err
	}
	return &ptyChild{cmd: cmd, rw: f}, nil
}

// stop closes the terminal the way closing a window does (SIGHUP), then
// makes sure the client is gone.
func (c *ptyChild) stop() {
	_ = c.cmd.Process.Signal(syscall.SIGHUP)
	done := make(chan struct{})
	go func() { _, _ = c.cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = c.cmd.Process.Kill()
		<-done
	}
	_ = c.rw.Close()
}
