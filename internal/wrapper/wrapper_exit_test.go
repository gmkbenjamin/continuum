//go:build !windows

package wrapper

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/continuum-dev/continuum/internal/core"
)

func TestWrapperEmitsExitEvent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CONTINUUM_HOME", home)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.Close()
	defer r.Close()
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()

	code, err := Run(Options{
		Harness: "generic",
		Argv:    []string{"/bin/sh", "-c", "exit 0"},
		Stdin:   r,
		Stdout:  devNull,
	})
	if err != nil || code != 0 {
		t.Fatalf("Run = (%d, %v), want (0, nil)", code, err)
	}

	entries, err := os.ReadDir(filepath.Join(home, "spool"))
	if err != nil {
		t.Fatal(err)
	}
	var sawStart, sawExit bool
	for _, e := range entries {
		f, err := os.Open(filepath.Join(home, "spool", e.Name()))
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(f)
		f.Close()
		var ev core.Event
		if json.Unmarshal(data, &ev) == nil && ev.Location != nil && ev.Location.Kind == "wrapper" {
			if ev.Kind == core.KindWrapperStart {
				sawStart = true
			}
			if ev.Kind == core.KindWrapperExit {
				sawExit = true
			}
		}
	}
	if !sawStart || !sawExit {
		t.Fatalf("expected %q and %q events in spool, got start=%v exit=%v (%d files)", core.KindWrapperStart, core.KindWrapperExit, sawStart, sawExit, len(entries))
	}
}
