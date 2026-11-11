//go:build !windows

package awake

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"syscall"
)

type cmdHolder struct{ cmd *exec.Cmd }

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
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	c.cmd = cmd
	go func() { _ = cmd.Wait() }()
	return nil
}

func (c *cmdHolder) release() {
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	c.cmd = nil
}
