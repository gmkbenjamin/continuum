package main

import (
	"strings"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/rules"
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
		// Busy again after the resume: the agent's own state wins.
		{knownSession{Live: true, Status: "busy", Open: true}, &core.Job{State: core.StateDelivered, DeliveredAt: now.Add(-10 * time.Minute)}, "working", "working"},
		{knownSession{Open: true}, &core.Job{State: core.StateIgnored, Result: "excluded: every session in /x", UpdatedAt: now}, "not_resuming", "excluded"},
		{knownSession{Open: false, LastSeen: now.Add(-3 * time.Hour)}, nil, "closed", "closed"},
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
