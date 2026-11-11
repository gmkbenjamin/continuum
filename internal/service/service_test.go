package service

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/continuum-dev/continuum/internal/core"
)

func TestPlan(t *testing.T) {
	t.Setenv("CONTINUUM_HOME", t.TempDir())
	p := core.GetPaths()
	bin := "/Users/me/My Tools/continuum"
	plan, err := MakePlan(bin, p)
	if err != nil {
		t.Skip(err)
	}
	switch runtime.GOOS {
	case "darwin":
		if !strings.Contains(plan.Content, "<string>/Users/me/My Tools/continuum</string>") {
			t.Fatalf("binary missing:\n%s", plan.Content)
		}
		f := filepath.Join(t.TempDir(), "x.plist")
		os.WriteFile(f, []byte(plan.Content), 0o644)
		if out, err := exec.Command("plutil", "-lint", f).CombinedOutput(); err != nil {
			t.Fatalf("invalid plist: %s", out)
		}
	case "linux":
		if !strings.Contains(plan.Content, `ExecStart="/Users/me/My Tools/continuum" daemon`) {
			t.Fatalf("unit:\n%s", plan.Content)
		}
	}
}
