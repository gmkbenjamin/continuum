package wrapper

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/continuum-dev/continuum/internal/rules"
)

func TestDetectExplicitHarness(t *testing.T) {
	r, err := rules.Load("")
	if err != nil {
		t.Fatal(err)
	}
	for command, want := range map[string]string{"kilo": "kilocode", "kilocode": "kilocode", "pi": "pi", "acli": "rovodev"} {
		if got := DetectHarness(r, []string{command}); got != want {
			t.Errorf("%q: got %q, want %q", command, got, want)
		}
	}
	dir := t.TempDir()
	cursor := filepath.Join(dir, "cursor-agent")
	alias := filepath.Join(dir, "agent")
	if err := os.WriteFile(cursor, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(alias, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := DetectHarness(r, []string{alias}); got != "generic" {
		t.Fatalf("unrelated agent classified as %s", got)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cursor, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := DetectHarness(r, []string{alias}); got != "cursor" {
		t.Fatalf("Cursor's agent classified as %s", got)
	}
}

func TestEmptyArgvRejected(t *testing.T) {
	if code, err := Run(Options{}); err == nil || code == 0 {
		t.Fatalf("Run(Options{}) = (%d, %v), want non-zero exit code and error", code, err)
	}
	if _, err := start(nil, nil, 80, 24); err == nil {
		t.Fatal("start(nil, ...) = nil error, want error")
	}
}
