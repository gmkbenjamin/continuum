//go:build !windows

package awake

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"syscall"
)

type cmdHolder struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	done chan struct{}
}

func newHolder() holder { return &cmdHolder{} }

func (c *cmdHolder) hold(reason string) error {
	pid := strconv.Itoa(os.Getpid())
	var argv []string
	switch runtime.GOOS {
	case "darwin":
		argv = []string{"/usr/bin/caffeinate", "-i", "-s", "-w", pid}
	default:
		p, err := exec.LookPath("systemd-inhibit")
		if err != nil {
			return errors.New("systemd-inhibit not found; cannot keep the computer awake")
		}
		tail, err := exec.LookPath("tail")
		if err != nil {
			return err
		}
		argv = []string{p, "--what=idle:sleep", "--who=Continuum", "--why=" + reason, "--mode=block", tail, "--pid=" + pid, "-f", "/dev/null"}
	}
	return c.start(argv)
}

func (c *cmdHolder) start(argv []string) error {
	c.release()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	c.cmd = cmd
	c.done = done
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	return nil
}

func (c *cmdHolder) release() {
	c.mu.Lock()
	cmd := c.cmd
	done := c.done
	c.cmd = nil
	c.done = nil
	c.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	select {
	case <-done:
		return
	default:
	}
	// Kill the whole process group so child helpers (such as systemd-inhibit's
	// tail --pid=...) never survive as orphans.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	<-done
}
