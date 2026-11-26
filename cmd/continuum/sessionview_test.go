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
	"github.com/continuum-dev/continuum/internal/messages"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/scope"
	"github.com/continuum-dev/continuum/internal/store"
)

func TestSessionStates(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 0, 0, 0, time.Local)
	cases := []struct {
		s        knownSession
		j        *core.Job
		code     string
		contains string
	}{
		{knownSession{Live: true, Status: "busy", Open: true}, nil, "working", "working"},
		{knownSession{Live: true, Status: "shell", Open: true}, nil, "working", "running a command"},
		{knownSession{Live: true, Status: "idle", Open: true}, nil, "idle", "idle"},
		{knownSession{Live: true, Status: "waiting", WaitingFor: "dialog open", Open: true}, nil, "waiting", "dialog open"},
		{knownSession{Live: true, Kind: "background"}, nil, "not_running", "not running"},
		{knownSession{Live: true, Status: "idle", Open: true}, &core.Job{State: core.StatePending, Kind: core.KindUsageLimit, DueAt: now.Add(2 * time.Hour)}, "limit", "resumes 11:00 (in 2h)"},
		{knownSession{Open: true}, &core.Job{State: core.StatePending, Kind: core.KindTransient, DueAt: now.Add(5 * time.Minute)}, "outage", "retrying"},
		{knownSession{Open: true}, &core.Job{State: core.StateNeedsUser, Result: "this limit needs you (billing)"}, "needs_you", "billing"},
		{knownSession{Live: true, Status: "idle", Open: true}, &core.Job{State: core.StateDelivered, DeliveredAt: now.Add(-10 * time.Minute)}, "resumed", "resumed 08:50"},
		{knownSession{Live: true, Status: "idle", Open: true}, &core.Job{State: core.StateDelivered, DeliveredAt: now.Add(-10 * time.Minute), TurnFailed: "API Error: refused"}, "needs_you", "resumed 08:50, but its turn failed: API Error: refused"},
		// Busy again after the resume: the agent's own state wins.
		{knownSession{Live: true, Status: "busy", Open: true}, &core.Job{State: core.StateDelivered, DeliveredAt: now.Add(-10 * time.Minute)}, "working", "working"},
		{knownSession{Open: true}, &core.Job{State: core.StateIgnored, Result: "excluded: every session in /x", UpdatedAt: now}, "not_resuming", "excluded"},
		{knownSession{Open: false, LastSeen: now.Add(-3 * time.Hour)}, nil, "closed", "closed"},
		// Closed while waiting on its limit: say so, and how it continues.
		{knownSession{Open: false, Checked: true, ResumesClosed: true}, &core.Job{State: core.StatePending, Kind: core.KindUsageLimit, DueAt: now.Add(2 * time.Hour)}, "limit", "closed · usage limit · continues it in the background 11:00"},
		// No resume command for a closed session (or headless off): say so.
		{knownSession{Open: false, Checked: true}, &core.Job{State: core.StatePending, Kind: core.KindUsageLimit, DueAt: now.Add(2 * time.Hour)}, "limit", "closed · usage limit · reopen it"},
		// Only an out-of-date copy is open: shown, and not typed into.
		{knownSession{Open: true, Live: true, Status: "idle", StaleCopy: true}, nil, "stale_copy", "out-of-date copy"},
		{knownSession{Open: true, Live: true, Status: "idle", StaleCopy: true}, &core.Job{State: core.StatePending, Kind: core.KindUsageLimit, DueAt: now.Add(2 * time.Hour)}, "stale_copy", "won't type into it"},
		{knownSession{Open: true, Live: true, Status: "idle"}, &core.Job{State: core.StatePending, Kind: core.KindUsageLimit, StaleCopyPID: 42, WaitingOpen: true}, "stale_copy", "limit reset · only an out-of-date copy is open"},
		// Not checked: whether it is open is unknown, so not "closed".
		{knownSession{Open: false, ResumesClosed: true}, &core.Job{State: core.StatePending, Kind: core.KindUsageLimit, DueAt: now.Add(2 * time.Hour)}, "limit", "usage limit · resumes 11:00"},
		{knownSession{Open: true, LastSeen: now.Add(-time.Minute)}, nil, "active", "open"},
	}
	for i, c := range cases {
		code, detail := sessionState(c.s, c.j, true, now)
		if code != c.code || !strings.Contains(detail, c.contains) {
			t.Errorf("%d: got %s %q, want %s ~%q", i, code, detail, c.code, c.contains)
		}
	}
}

func TestSessionWhere(t *testing.T) {
	r, _ := rules.Load("")
	cases := map[string]knownSession{
		"Claude Desktop":           {Harness: "claude", Entrypoint: "claude-desktop"},
		"Codex Desktop":            {Harness: "codex", Entrypoint: "Codex Desktop"},
		"tmux %3":                  {Harness: "claude", Location: &core.Location{Kind: "tmux", Target: "%3"}},
		"Rex":                      {Harness: "claude", Location: &core.Location{Kind: "rex", Target: "b1"}},
		"terminal (via Continuum)": {Harness: "claude", Location: &core.Location{Kind: "wrapper", Target: "w1"}},
		"background":               {Harness: "claude", Kind: "background"},
		"terminal":                 {Harness: "claude", Open: true},
	}
	for want, s := range cases {
		if got := sessionWhere(s, r); got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
}

func TestSessionRowsWindowAndWrapperMessage(t *testing.T) {
	sandboxHome(t)
	p := core.GetPaths()
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := store.SaveJSON(p.Sessions, map[string]*store.Session{
		"claude:recent-closed": {Harness: "claude", SessionID: "recent-closed", Cwd: "/w/r", LastSeen: now.Add(-2 * time.Hour)},
		"claude:old-closed":    {Harness: "claude", SessionID: "old-closed", Cwd: "/w/o", LastSeen: now.Add(-20 * time.Hour)},
	}); err != nil {
		t.Fatal(err)
	}

	wdir := filepath.Join(p.Wrappers, "wrap1")
	if err := os.MkdirAll(wdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wdir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{"harness": "antigravity", "cwd": "/w/ag", "started": now})
	if err := os.WriteFile(filepath.Join(wdir, "meta.json"), meta, 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := messages.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	m.Set(scope.Entry{Harness: "antigravity", Wrapper: "wrap1"}, messages.Pair{Text: "keep working"})
	if err := m.Save(p); err != nil {
		t.Fatal(err)
	}

	// Default `continuum status` (window == 0) only lists open sessions and
	// pending resumes, never closed sessions.
	for _, r := range sessionRows(0) {
		if r.ID == "recent-closed" || r.ID == "old-closed" {
			t.Fatalf("status default window=0 should not include closed session %q", r.ID)
		}
	}

	rows := sessionRows(12 * time.Hour)
	var gotRecent, gotOld, gotWrapMsg bool
	for _, r := range rows {
		switch r.ID {
		case "recent-closed":
			gotRecent = true
		case "old-closed":
			gotOld = true
		}
		if r.Wrapper == "wrap1" && r.Message == "session" {
			gotWrapMsg = true
		}
	}
	if !gotRecent {
		t.Fatal("12h window should include closed session active 2h ago")
	}
	if gotOld {
		t.Fatal("12h window should exclude closed session active 20h ago")
	}
	if !gotWrapMsg {
		t.Fatalf("wrapper row missing message source: %+v", rows)
	}

	openSessions := map[string]bool{"claude:open-1": true}
	if !jobSessionOpen(p, &core.Job{Harness: "claude", SessionID: "open-1"}, openSessions) {
		t.Fatal("open session job should be reported as open")
	}
	if jobSessionOpen(p, &core.Job{Harness: "claude", SessionID: "recent-closed"}, openSessions) {
		t.Fatal("closed session job should not be reported as open")
	}
	if !jobSessionOpen(p, &core.Job{Harness: "antigravity", Location: &core.Location{Kind: "wrapper", Target: "wrap1"}}, openSessions) {
		t.Fatal("alive wrapper job should be reported as open")
	}
	if jobSessionOpen(p, &core.Job{Harness: "antigravity", Location: &core.Location{Kind: "wrapper", Target: "dead-wrap"}}, openSessions) {
		t.Fatal("dead wrapper job should not be reported as open")
	}
}

func TestStaleCopyNeedsInactivity(t *testing.T) {
	r, _ := rules.Load("")
	last := time.Now().Add(-time.Hour)
	rec := &store.Session{PID: 999999, Ended: true, LastSeen: last}
	idle := knownSession{Harness: "claude", Open: true, PID: 4242, Status: "idle", StatusSince: last.Add(-time.Hour)}
	if !staleCopy(r, nil, idle, rec) {
		t.Fatal("idle since before the record: want out of date")
	}
	since := idle
	since.StatusSince = last.Add(time.Minute)
	if staleCopy(r, nil, since, rec) {
		t.Fatal("active since the record: want current")
	}
	working := idle
	working.Status, working.StatusSince = "busy", time.Time{}
	if staleCopy(r, nil, working, rec) {
		t.Fatal("working, no time given: want current")
	}
	stuck := idle
	stuck.Status = "busy" // since before the record
	if !staleCopy(r, nil, stuck, rec) {
		t.Fatal("busy since before the record: want out of date")
	}
	same := idle
	same.PID = 999999
	if staleCopy(r, nil, same, rec) {
		t.Fatal("the recorded process itself: want current")
	}
}
