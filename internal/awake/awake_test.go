package awake

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

type fake struct{ holds, releases int }

func (f *fake) hold(string) error { f.holds++; return nil }
func (f *fake) release()          { f.releases++ }

func TestSetIsIdempotent(t *testing.T) {
	f := &fake{}
	k := &Keeper{impl: f}
	k.Set(true, "a")
	k.Set(true, "b")
	if !k.Held() || f.holds != 1 {
		t.Fatalf("holds %d", f.holds)
	}
	k.Set(false, "")
	k.Set(false, "")
	if k.Held() || f.releases != 1 {
		t.Fatalf("releases %d", f.releases)
	}
}

// On macOS, check the request really reaches power management.
func TestMacOSAssertion(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	ours := func() bool {
		out, _ := exec.Command("ps", "-axo", "args=").Output()
		return strings.Contains(string(out), "caffeinate -i -s -w "+strconv.Itoa(os.Getpid()))
	}
	k := New()
	if _, err := k.Set(true, "test"); err != nil {
		t.Fatal(err)
	}
	held := false
	for i := 0; i < 20 && !held; i++ {
		time.Sleep(100 * time.Millisecond)
		held = ours()
	}
	out, _ := exec.Command("pmset", "-g", "assertions").Output()
	k.Set(false, "")
	if !held || !strings.Contains(string(out), "PreventUserIdleSystemSleep") {
		t.Fatal("no idle-sleep assertion while held")
	}
	time.Sleep(300 * time.Millisecond)
	if ours() {
		t.Fatal("assertion not released")
	}
}
