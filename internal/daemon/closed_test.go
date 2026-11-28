package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/scope"
)

func disableClosedTracking(t *testing.T, h *harnessT) {
	t.Helper()
	cfg := `{"claude_home": ` + jsonStr(h.claude) + `, "codex_home": ` + jsonStr(h.codex) + `, "notify": false, "scan_panes": false, "poll_seconds": 1, "track_closed_sessions": false}`
	if err := os.WriteFile(h.d.Paths.Config, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	h.d.reload()
}

func makeLiveWrapper(t *testing.T, p core.Paths, id string) {
	t.Helper()
	dir := filepath.Join(p.Wrappers, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTrackClosedOffSkipsOnSessionEnd(t *testing.T) {
	h := setup(t)
	disableClosedTracking(t, h)
	h.live = &live.State{Checked: true, Open: true, Authoritative: true}

	ev := limitEvent(h, "s1", 30*time.Minute)
	ev.PID = os.Getpid()
	h.d.Handle(ev)
	if j := h.d.Jobs["claude:s1"]; j == nil || j.State != core.StatePending {
		t.Fatalf("expected pending job for open session, got %+v", j)
	}

	h.d.Handle(core.Event{
		Harness: "claude", Kind: core.KindSessionEnd, SessionID: "s1",
		At: h.now.Add(time.Minute), Source: "hook:SessionEnd", PID: os.Getpid(),
	})
	j := h.d.Jobs["claude:s1"]
	if j.State != core.StateSkipped {
		t.Fatalf("expected skipped after SessionEnd, got state=%s result=%q", j.State, j.Result)
	}
	if h.d.resumesClosed("claude") {
		t.Fatal("resumesClosed should be false when track_closed_sessions is off")
	}
}

func TestTrackClosedOffIgnoresAlreadyClosedLimitEvent(t *testing.T) {
	h := setup(t)
	disableClosedTracking(t, h)
	h.live = &live.State{Checked: true, Open: false, Authoritative: true}

	ev := limitEvent(h, "closed-1", 30*time.Minute)
	h.d.Handle(ev)
	if j := h.d.Jobs["claude:closed-1"]; j != nil {
		t.Fatalf("closed session should not be scheduled when track_closed_sessions=false: %+v", j)
	}
}

func TestTrackClosedOffPrunesDeadWrapperAndPreservesOpenSessions(t *testing.T) {
	h := setup(t)
	makeLiveWrapper(t, h.d.Paths, "alive-wrap")

	// Start with default (tracking closed sessions on) and schedule three jobs:
	// 1. an antigravity job whose wrapper is dead
	// 2. an antigravity job whose wrapper is alive
	// 3. a rollout-only codex job whose folder still has a live codex process
	h.d.Handle(core.Event{
		Harness: "antigravity", Kind: core.KindUsageLimit, SessionID: "dead-ag", Cwd: "/work/app",
		At: h.now, ResetAt: h.now.Add(2 * time.Hour), Source: "hook:Stop", PID: deadPID,
		Location: &core.Location{Kind: "wrapper", Target: "dead-wrap"},
	})
	h.d.Handle(core.Event{
		Harness: "antigravity", Kind: core.KindUsageLimit, SessionID: "live-ag", Cwd: "/work/app",
		At: h.now, ResetAt: h.now.Add(2 * time.Hour), Source: "hook:Stop", PID: os.Getpid(),
		Location: &core.Location{Kind: "wrapper", Target: "alive-wrap"},
	})
	h.d.Handle(core.Event{
		Harness: "codex", Kind: core.KindUsageLimit, SessionID: "open-codex", Cwd: "/work/app",
		At: h.now, ResetAt: h.now.Add(2 * time.Hour), Source: "rollout",
	})

	h.d.LiveFn = func(j *core.Job) *live.State {
		switch j.SessionID {
		case "dead-ag":
			// Even if another agy runs in the same folder (ProcessMatch=false),
			// dead-ag's wrapper exited so it is closed.
			return &live.State{Checked: true, Open: true, Source: "processes", ProcessMatch: false}
		case "open-codex":
			// Rollout-only codex with no dead wrapper/PID and a matching codex
			// process in the folder must stay tracked while open.
			return &live.State{Checked: true, Open: true, Source: "processes", ProcessMatch: false}
		default:
			return &live.State{Checked: true, Open: true, Source: "processes", ProcessMatch: true}
		}
	}

	// Flip track_closed_sessions off and tick once.
	disableClosedTracking(t, h)
	h.advance(time.Second)

	if j := h.d.Jobs["antigravity:dead-ag"]; j.State != core.StateSkipped {
		t.Fatalf("dead wrapper job should be skipped, got state=%s result=%q", j.State, j.Result)
	}
	if j := h.d.Jobs["antigravity:live-ag"]; j.State != core.StatePending {
		t.Fatalf("live wrapper job should remain pending, got state=%s", j.State)
	}
	if j := h.d.Jobs["codex:open-codex"]; j.State != core.StatePending {
		t.Fatalf("open rollout-only codex job should remain pending, got state=%s", j.State)
	}

	// Once the codex process in that folder closes, it is pruned too.
	h.d.LiveFn = func(j *core.Job) *live.State {
		if j.SessionID == "open-codex" {
			return &live.State{Checked: true, Open: false, Source: "processes"}
		}
		return &live.State{Checked: true, Open: true, Source: "processes", ProcessMatch: true}
	}
	h.advance(20 * time.Second)
	if j := h.d.Jobs["codex:open-codex"]; j.State != core.StateSkipped {
		t.Fatalf("closed codex job should be skipped after process exits, got state=%s", j.State)
	}
}

func TestTrackClosedOffSkipsBeforeDelivery(t *testing.T) {
	h := setup(t)
	h.live = &live.State{Checked: true, Open: true, Authoritative: true}

	ev := limitEvent(h, "s-due", 10*time.Minute)
	ev.PID = os.Getpid()
	h.d.Handle(ev)

	disableClosedTracking(t, h)
	// Session closes right when due.
	h.live = &live.State{Checked: true, Open: false, Authoritative: true}
	h.advance(12 * time.Minute)

	if len(h.delivered) != 0 {
		t.Fatalf("closed session should not be delivered, got %d deliveries", len(h.delivered))
	}
	if j := h.d.Jobs["claude:s-due"]; j.State != core.StateSkipped {
		t.Fatalf("expected skipped at delivery time, got state=%s", j.State)
	}
}

func TestTrackClosedOffDoesNotRescheduleClosedIgnoredJob(t *testing.T) {
	h := setup(t)
	if _, err := scope.Update(h.d.Paths, func(cur scope.Scope) (scope.Scope, bool, error) {
		cur.Mode = scope.ModeChosen
		return cur, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	h.d.reload()
	h.live = &live.State{Checked: true, Open: true, Authoritative: true}

	ev := limitEvent(h, "s-ign", 30*time.Minute)
	ev.PID = os.Getpid()
	h.d.Handle(ev)
	if j := h.d.Jobs["claude:s-ign"]; j == nil || j.State != core.StateIgnored {
		t.Fatalf("expected ignored job, got %+v", j)
	}

	disableClosedTracking(t, h)
	h.live = &live.State{Checked: true, Open: false, Authoritative: true}
	if _, err := scope.Update(h.d.Paths, func(cur scope.Scope) (scope.Scope, bool, error) {
		cur.Mode = scope.ModeAll
		return cur, true, nil
	}); err != nil {
		t.Fatal(err)
	}
	h.advance(5 * time.Second)
	if j := h.d.Jobs["claude:s-ign"]; j.State != core.StateIgnored {
		t.Fatalf("closed ignored job should not be rescheduled when scope widens, got state=%s", j.State)
	}
}
