package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/store"
)

func TestCmdClosedToggle(t *testing.T) {
	sandboxHome(t)
	p := core.GetPaths()

	out := capture(t, func() {
		if err := cmdClosed(nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "tracked (built-in default)") {
		t.Fatalf("default output: %q", out)
	}

	if err := cmdClosed([]string{"off"}); err != nil {
		t.Fatal(err)
	}
	cfg, err := core.LoadConfig(p.Config)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClosedSessionsTracked() {
		t.Fatal("expected track_closed_sessions=false after `continuum closed off`")
	}

	out = capture(t, func() {
		if err := cmdClosed(nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(out, "not tracked") {
		t.Fatalf("off output: %q", out)
	}

	if err := cmdClosed([]string{"on"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ = core.LoadConfig(p.Config)
	if !cfg.ClosedSessionsTracked() || cfg.TrackClosedSessions == nil || !*cfg.TrackClosedSessions {
		t.Fatalf("expected explicit true after `continuum closed on`, got %+v", cfg.TrackClosedSessions)
	}

	if err := cmdClosed([]string{"default"}); err != nil {
		t.Fatal(err)
	}
	cfg, _ = core.LoadConfig(p.Config)
	if !cfg.ClosedSessionsTracked() || cfg.TrackClosedSessions != nil {
		t.Fatalf("expected key removed after `continuum closed default`, got %+v", cfg.TrackClosedSessions)
	}

	if err := cmdClosed([]string{"bogus"}); err == nil {
		t.Fatal("expected error for invalid arg")
	}
}

func TestTrackClosedOffHidesClosedPendingFromStatusListSessions(t *testing.T) {
	sandboxHome(t)
	p := core.GetPaths()
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()

	// Seed a closed session with a pending resume and a live wrapper session.
	if err := store.SaveJSON(p.Sessions, map[string]*store.Session{
		"antigravity:closed-pend": {
			Harness: "antigravity", SessionID: "closed-pend", Cwd: "/w/app",
			Location: &core.Location{Kind: "wrapper", Target: "dead-wrap"},
			LastSeen: now.Add(-10 * time.Minute),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveJSON(p.Jobs, map[string]*core.Job{
		"antigravity:closed-pend": {
			Key: "antigravity:closed-pend", Harness: "antigravity", SessionID: "closed-pend", Cwd: "/w/app",
			Location: &core.Location{Kind: "wrapper", Target: "dead-wrap"},
			Kind: core.KindUsageLimit, State: core.StatePending,
			LimitAt: now.Add(-10 * time.Minute), DueAt: now.Add(2 * time.Hour), UpdatedAt: now,
		},
	}); err != nil {
		t.Fatal(err)
	}
	wdir := filepath.Join(p.Wrappers, "livewrap")
	if err := os.MkdirAll(wdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wdir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{"harness": "antigravity", "cwd": "/w/open", "started": now})
	if err := os.WriteFile(filepath.Join(wdir, "meta.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}

	// Default (on): closed pending session appears in status.
	var foundDefault bool
	for _, r := range sessionRows(0) {
		if r.ID == "closed-pend" {
			foundDefault = true
		}
	}
	if !foundDefault {
		t.Fatal("default config should include closed session with pending resume")
	}

	// Turn closed-session tracking off.
	if err := cmdClosed([]string{"off"}); err != nil {
		t.Fatal(err)
	}

	// sessionRows(0) hides the closed pending session, keeping the open wrapper.
	rows := sessionRows(0)
	var foundLive bool
	for _, r := range rows {
		if r.ID == "closed-pend" {
			t.Fatalf("closed pending session should be hidden when track_closed_sessions=false: %+v", r)
		}
		if r.Wrapper == "livewrap" {
			foundLive = true
		}
	}
	if !foundLive {
		t.Fatalf("live wrapper should still be listed: %+v", rows)
	}

	// `continuum status` shows the setting and does not dump closed jobs under "Other resumes:".
	statusOut := capture(t, func() {
		if err := cmdStatus(nil); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(statusOut, "closed:     not tracked") {
		t.Fatalf("status missing closed setting line:\n%s", statusOut)
	}
	if strings.Contains(statusOut, "closed-p") || strings.Contains(statusOut, "Other resumes:") {
		t.Fatalf("status should not show closed pending job:\n%s", statusOut)
	}

	// `continuum list` hides closed pending jobs unless --all is passed.
	listOut := capture(t, func() {
		if err := cmdList(nil); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(listOut, "closed-p") {
		t.Fatalf("list should hide closed pending job:\n%s", listOut)
	}
	listAllOut := capture(t, func() {
		if err := cmdList([]string{"--all"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(listAllOut, "closed-p") {
		t.Fatalf("list --all should still show closed job:\n%s", listAllOut)
	}

	// `continuum sessions` hides closed sessions unless --all is passed.
	sessOut := capture(t, func() {
		if err := cmdSessions(nil); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(sessOut, "closed-p") {
		t.Fatalf("sessions should hide closed session:\n%s", sessOut)
	}
	sessAllOut := capture(t, func() {
		if err := cmdSessions([]string{"--all"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(sessAllOut, "closed-p") {
		t.Fatalf("sessions --all should include closed session:\n%s", sessAllOut)
	}
}

func TestCodexRolloutBindsToUniqueLiveWrapperWithoutDuplicateRow(t *testing.T) {
	sandboxHome(t)
	p := core.GetPaths()
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := store.SaveJSON(p.Sessions, map[string]*store.Session{
		"codex:01a21f5a-9699-7562-aed6-b0d6c171d2e5": {
			Harness: "codex", SessionID: "01a21f5a-9699-7562-aed6-b0d6c171d2e5",
			Cwd: "/w/agentKeeper", LastSeen: now.Add(-5 * time.Minute),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveJSON(p.Jobs, map[string]*core.Job{
		"codex:01a21f5a-9699-7562-aed6-b0d6c171d2e5": {
			Key: "codex:01a21f5a-9699-7562-aed6-b0d6c171d2e5", Harness: "codex",
			SessionID: "01a21f5a-9699-7562-aed6-b0d6c171d2e5", Cwd: "/w/agentKeeper",
			Kind: core.KindUsageLimit, State: core.StatePending,
			LimitAt: now.Add(-5 * time.Minute), DueAt: now.Add(2 * time.Hour), UpdatedAt: now,
		},
	}); err != nil {
		t.Fatal(err)
	}
	wdir := filepath.Join(p.Wrappers, "404bf0cceb368543")
	if err := os.MkdirAll(wdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wdir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{
		"harness": "codex", "cwd": "/w/agentKeeper", "argv": []string{"codex"},
		"started": now.Add(-10 * time.Minute),
	})
	if err := os.WriteFile(filepath.Join(wdir, "meta.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}

	rows := sessionRows(0)
	if len(rows) != 1 {
		t.Fatalf("expected 1 unified row (no duplicate wrapper row), got %d: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.ID != "01a21f5a-9699-7562-aed6-b0d6c171d2e5" || !r.Open || r.Wrapper != "404bf0cceb368543" || r.State != "limit" || strings.Contains(r.Detail, "closed") {
		t.Fatalf("expected open limit row bound to wrapper, got %+v", r)
	}
}
