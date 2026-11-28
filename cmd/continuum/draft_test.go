package main

import (
	"strings"
	"testing"

	"github.com/continuum-dev/continuum/internal/core"
)

func TestCmdDraftToggle(t *testing.T) {
	sandboxHome(t)
	p := core.GetPaths()

	out := capture(t, func() {
		if err := cmdDraft(nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "restore (built-in default)") {
		t.Fatalf("default output: %q", out)
	}

	if err := cmdDraft([]string{"submit"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := core.LoadConfig(p.Config)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DraftMode() != core.DraftSubmit || cfg.DraftHandling != core.DraftSubmit {
		t.Fatalf("expected draft_handling=submit, got %+v", cfg.DraftHandling)
	}

	out = capture(t, func() {
		if err := cmdDraft(nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "submit (your setting)") {
		t.Fatalf("submit output: %q", out)
	}

	// Alias "send" maps to "submit".
	if err := cmdDraft([]string{"restore"}); err != nil {
		t.Fatal(err)
	}
	if err := cmdDraft([]string{"send"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ = core.LoadConfig(p.Config)
	if cfg.DraftMode() != core.DraftSubmit {
		t.Fatalf("expected alias send -> submit, got %q", cfg.DraftHandling)
	}

	if err := cmdDraft([]string{"default"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ = core.LoadConfig(p.Config)
	if cfg.DraftMode() != core.DraftRestore || cfg.DraftHandling != "" {
		t.Fatalf("expected key removed after default, got %q", cfg.DraftHandling)
	}

	if err := cmdDraft([]string{"bogus"}); err == nil {
		t.Fatal("expected error for invalid arg")
	}
}
