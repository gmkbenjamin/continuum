//go:build !windows

package awake

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCmdHolderKillsProcessGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	h := &cmdHolder{}
	err := h.start([]string{
		"/bin/sh", "-c",
		`sleep 30 & echo $! > "` + pidFile + `"; wait`,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.release)

	var childPID int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && n > 0 {
				childPID = n
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("child process did not write pid file")
	}

	h.release()

	dead := false
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPID, 0); err == syscall.ESRCH {
			dead = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !dead {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
		t.Fatalf("grandchild pid %d survived process-group release", childPID)
	}
}
