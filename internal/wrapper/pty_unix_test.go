//go:build !windows

package wrapper

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestUnixPTYCloseAfterWaitSkipsReapedPID(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	if err := cmd.Start(); err != nil {
		_ = w.Close()
		t.Fatal(err)
	}
	u := &unixPTY{f: w, cmd: cmd}
	code, err := u.Wait()
	if err != nil || code != 7 {
		t.Fatalf("Wait() = (%d, %v), want (7, nil)", code, err)
	}
	u.mu.Lock()
	reaped := u.reaped
	u.mu.Unlock()
	if !reaped {
		t.Fatal("expected reaped=true after child exited")
	}

	called := false
	u.signalChild(func(*os.Process) { called = true })
	if called {
		t.Fatal("signalChild ran on already-reaped PID")
	}
	u.Signal(syscall.SIGTERM)
	if err := u.Close(); err != nil {
		t.Fatalf("Close() after Wait(): %v", err)
	}
}
