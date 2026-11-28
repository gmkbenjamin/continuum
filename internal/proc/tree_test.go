package proc

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Hooks walk this tree to tell an agent another agent started (a `claude -p`
// from a Bash tool) from one you started; it must work on every OS.
func TestTreeKnowsThisProcess(t *testing.T) {
	tree := Tree()
	n, ok := tree[os.Getpid()]
	if !ok {
		t.Fatalf("this process is missing from the tree (%d entries)", len(tree))
	}
	self := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if n.PPID != os.Getppid() || !strings.HasPrefix(n.Name, self[:min(len(self), 8)]) {
		t.Fatalf("this process: %+v, want parent %d and name %s", n, os.Getppid(), self)
	}
}

// ps writes lstart in the user's language and order ("Fri 13 Nov" with an
// Australian locale); the start time must be read whatever the locale.
func TestStartTimeInAnyLocale(t *testing.T) {
	if runtime.GOOS == "windows" {
		at, ok := StartTime(os.Getpid())
		if !ok || time.Since(at) > 10*time.Minute || time.Since(at) < -time.Minute {
			t.Errorf("windows start time %v ok=%v", at, ok)
		}
		return
	}
	for _, loc := range []string{"en_AU.UTF-8", "de_DE.UTF-8", "C"} {
		t.Setenv("LC_ALL", loc)
		at, ok := StartTime(os.Getpid())
		if !ok || time.Since(at) > 10*time.Minute || time.Since(at) < -time.Minute {
			t.Errorf("LC_ALL=%s: start time %v ok=%v", loc, at, ok)
		}
	}
}
