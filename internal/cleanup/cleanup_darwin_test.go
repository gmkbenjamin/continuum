package cleanup

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/proc"
	"golang.org/x/sys/unix"
)

func TestLogStreamRequiresDisconnectedOutput(t *testing.T) {
	now := time.Now()
	old := process{Name: "log", StartedAt: now.Add(-2 * time.Hour)}
	prefix := "p50\x00\nf0\x00tCHR\x00n(revoked)\x00\n"
	for _, tc := range []struct {
		name, output, want string
	}{
		{"revoked terminal", "f1\x00tCHR\x00n\x00\nf2\x00tCHR\x00n\x00", logReason},
		{"output file", "f1\x00tREG\x00n/tmp/log-output\x00", ""},
		{"unnamed pipe", "f1\x00tPIPE\x00", ""},
		{"unknown open descriptor", "f1\x00", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := parseFiles(prefix + tc.output)[50]
			if got := describe(old, f, now, []string{"log", "stream"}); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDetachedLauncherCleanup(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("requires a regular user")
	}
	if _, err := snapshot(); err != nil {
		t.Skipf("requires macOS process table access: %v", err)
	}
	if _, err := launchdPIDs(); err != nil {
		t.Skipf("requires access to launchd: %v", err)
	}
	if _, err := readFiles([]int{os.Getpid()}); err != nil {
		t.Skipf("requires access to file descriptors: %v", err)
	}
	logDir, err := os.MkdirTemp("/private/tmp", ".tmp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(logDir) })
	cwd := filepath.Join(t.TempDir(), "neo-rsh", "crates", "neo-launcher")
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestLauncherFixture$")
	cmd.Env = append(os.Environ(), "CONTINUUM_CLEANUP_TEST_CWD="+cwd, "CONTINUUM_CLEANUP_TEST_LOG="+filepath.Join(logDir, "launcher.err"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("start fixture: %v (%s)", err, out)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 1 {
		t.Fatalf("invalid fixture PID: %q", out)
	}
	t.Cleanup(func() { _ = unix.Kill(-pid, unix.SIGTERM) })

	// Advance only the classifier's clock. The fixture is a genuine orphan in
	// its own process group, with the same file layout as the leaked test jobs.
	now := time.Now().Add(2 * time.Hour)
	var target Finding
	var registered map[int]bool
	for deadline := time.Now().Add(3 * time.Second); ; {
		table, err := snapshot()
		if err != nil {
			t.Fatal(err)
		}
		found, services, err := discover(now, &Finding{PID: pid, StartedAt: table[pid].StartedAt})
		if err != nil {
			t.Fatal(err)
		}
		if len(found) == 1 {
			target, registered = found[0], services
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("launcher fixture was not found: %+v", table[pid])
		}
		time.Sleep(50 * time.Millisecond)
	}
	if target.Reason != launcherReason || target.Members < 2 {
		t.Fatalf("unexpected finding: %+v", target)
	}
	wrong := target
	wrong.StartedAt = target.StartedAt.Add(time.Microsecond)
	if signaled, err := terminate(now, wrong, registered); signaled || err != nil {
		t.Fatalf("different process identity: signaled=%v err=%v", signaled, err)
	}
	if signaled, err := terminate(now, target, registered); !signaled || err != nil {
		t.Fatalf("stop fixture: signaled=%v err=%v", signaled, err)
	}
	for deadline := time.Now().Add(3 * time.Second); ; {
		table, err := snapshot()
		if err != nil {
			t.Fatal(err)
		}
		living := false
		for _, p := range table {
			living = living || (p.Group == pid && !p.Zombie)
		}
		if !living {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatal("fixture process group survived SIGTERM")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestLauncherFixture(t *testing.T) {
	cwd := os.Getenv("CONTINUUM_CLEANUP_TEST_CWD")
	if cwd == "" {
		return
	}
	pid, err := proc.StartDetached(
		[]string{"/bin/sh", "-c", `/bin/sh -c 'sleep 30; :' & sleep 30; wait`},
		cwd, nil, os.Getenv("CONTINUUM_CLEANUP_TEST_LOG"),
	)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println(pid)
	os.Exit(0)
}
