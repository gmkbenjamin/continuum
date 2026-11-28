package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/store"
)

func TestWrapperExitClearsRouteAndDropsJobsImmediately(t *testing.T) {
	h := setup(t)
	// Wrapper-only job (no session id) should be skipped immediately on wrapper_exit,
	// without waiting 5 minutes for prune.
	h.d.Handle(core.Event{
		Harness: "antigravity", Kind: core.KindUsageLimit, Cwd: "/work/app",
		At: h.now, ResetAt: h.now.Add(2 * time.Hour), Source: "wrapper",
		Location: &core.Location{Kind: "wrapper", Target: "w-anon"},
		Message:  "Individual quota reached. Resets in 2h.",
	})
	if j := h.d.Jobs["antigravity@wrapper:w-anon"]; j == nil || j.State != core.StatePending {
		t.Fatalf("expected pending wrapper job, got %+v", j)
	}

	h.d.Handle(core.Event{
		Harness: "antigravity", Kind: core.KindWrapperExit, Cwd: "/work/app",
		At: h.now.Add(10 * time.Second), Source: "wrapper",
		Location: &core.Location{Kind: "wrapper", Target: "w-anon"},
	})
	if j := h.d.Jobs["antigravity@wrapper:w-anon"]; j.State != core.StateSkipped {
		t.Fatalf("wrapper-only job should be skipped immediately on wrapper_exit, got %s", j.State)
	}

	// Named session with track_closed_sessions=true: wrapper_exit clears dead
	// wrapper location and marks session Ended, keeping the job for reopen/headless.
	h.d.Handle(core.Event{
		Harness: "antigravity", Kind: core.KindUsageLimit, SessionID: "ag-1", Cwd: "/work/app",
		At: h.now, ResetAt: h.now.Add(2 * time.Hour), Source: "hook:Stop", PID: deadPID,
		Location: &core.Location{Kind: "wrapper", Target: "w-ag"},
		Message:  "Individual quota reached. Resets in 2h.",
	})
	h.d.Handle(core.Event{
		Harness: "antigravity", Kind: core.KindWrapperExit, Cwd: "/work/app",
		At: h.now.Add(15 * time.Second), Source: "wrapper",
		Location: &core.Location{Kind: "wrapper", Target: "w-ag"},
	})
	j := h.d.Jobs["antigravity:ag-1"]
	if j.State != core.StatePending || j.Location != nil {
		t.Fatalf("expected pending job with cleared wrapper route, got state=%s loc=%v", j.State, j.Location)
	}
	if s := h.d.Sessions["antigravity:ag-1"]; s == nil || !s.Ended || s.Location != nil {
		t.Fatalf("expected session ended with cleared route, got %+v", s)
	}

	// Because Antigravity has no headless resume command and the session is now
	// ended, wantAwake must NOT keep the computer awake for it.
	if on, why := h.d.wantAwake(); on {
		t.Fatalf("wantAwake should be false for closed non-headless job, got true (%s)", why)
	}

	// With track_closed_sessions=false, wrapper_exit drops the session job immediately.
	disableClosedTracking(t, h)
	makeLiveWrapper(t, h.d.Paths, "w-ag2")
	h.live = &live.State{Checked: true, Open: true, Source: "processes"}
	h.d.Handle(core.Event{
		Harness: "antigravity", Kind: core.KindUsageLimit, SessionID: "ag-2", Cwd: "/work/app",
		At: h.now, ResetAt: h.now.Add(2 * time.Hour), Source: "hook:Stop", PID: os.Getpid(),
		Location: &core.Location{Kind: "wrapper", Target: "w-ag2"},
		Message:  "Individual quota reached. Resets in 2h.",
	})
	if j2 := h.d.Jobs["antigravity:ag-2"]; j2 == nil || j2.State != core.StatePending {
		t.Fatalf("expected pending job while wrapper is alive, got %+v", j2)
	}
	h.d.Handle(core.Event{
		Harness: "antigravity", Kind: core.KindWrapperExit, Cwd: "/work/app",
		At: h.now.Add(20 * time.Second), Source: "wrapper",
		Location: &core.Location{Kind: "wrapper", Target: "w-ag2"},
	})
	if j2 := h.d.Jobs["antigravity:ag-2"]; j2 == nil || j2.State != core.StateSkipped {
		t.Fatalf("expected skipped immediately on wrapper_exit when track_closed_sessions=false, got %+v", j2)
	}
}

func TestResolveWrapperBindings(t *testing.T) {
	r, err := rules.Load("")
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 28, 10, 0, 0, 0, time.UTC)

	sessions := map[string]*store.Session{
		"codex:01a21f63-a12b-7451-8916-0dda329d4611": {
			Harness: "codex", SessionID: "01a21f63-a12b-7451-8916-0dda329d4611",
			Cwd: "/work/continuum", LastSeen: t0.Add(10 * time.Minute),
		},
		"codex:01a21f5a-9699-7562-aed6-b0d6c171d2e5": {
			Harness: "codex", SessionID: "01a21f5a-9699-7562-aed6-b0d6c171d2e5",
			Cwd: "/work/agentKeeper", LastSeen: t0.Add(35 * time.Minute),
		},
		// Older session in resetcalendar active BEFORE the wrapper started: must NOT bind.
		"codex:01a20522-3346-7641-a879-066bed9d0916": {
			Harness: "codex", SessionID: "01a20522-3346-7641-a879-066bed9d0916",
			Cwd: "/work/resetcalendar", LastSeen: t0.Add(21 * time.Minute),
		},
		// Newer session in resetcalendar active AFTER the wrapper started: MUST bind.
		"codex:01a21f51-42d1-7391-8f0d-4e7a7f3eaadd": {
			Harness: "codex", SessionID: "01a21f51-42d1-7391-8f0d-4e7a7f3eaadd",
			Cwd: "/work/resetcalendar", LastSeen: t0.Add(49 * time.Minute),
		},
		// Claude has an authoritative session list: must NOT auto-bind by folder alone.
		"claude:old-claude": {
			Harness: "claude", SessionID: "old-claude",
			Cwd: "/work/claudeproj", LastSeen: t0.Add(30 * time.Minute),
		},
	}
	wrappers := []deliver.WrapperInfo{
		{ID: "w-resume", Harness: "codex", Cwd: "/work/continuum", Argv: []string{"codex", "resume", "01a21f63-a12b-7451-8916-0dda329d4611"}, Started: t0.Add(55 * time.Minute), PID: 100},
		{ID: "w-ak", Harness: "codex", Cwd: "/work/agentKeeper", Argv: []string{"codex"}, Started: t0.Add(32 * time.Minute), PID: 101},
		{ID: "w-rc", Harness: "codex", Cwd: "/work/resetcalendar", Argv: []string{"codex"}, Started: t0.Add(22 * time.Minute), PID: 102},
		{ID: "w-claude", Harness: "claude", Cwd: "/work/claudeproj", Argv: []string{"claude"}, Started: t0.Add(20 * time.Minute), PID: 103},
	}
	procs := []proc.Process{
		{PID: 201, Args: []string{"codex"}, Line: "codex"},
		{PID: 202, Args: []string{"codex"}, Line: "codex"},
	}
	cwds := map[int]string{201: "/work/agentKeeper", 202: "/work/resetcalendar"}

	got := ResolveWrapperBindings(r, sessions, nil, wrappers, procs, func(pid int) string { return cwds[pid] })
	if got["codex:01a21f63-a12b-7451-8916-0dda329d4611"].ID != "w-resume" {
		t.Fatalf("explicit argv resume not bound: %+v", got)
	}
	if got["codex:01a21f5a-9699-7562-aed6-b0d6c171d2e5"].ID != "w-ak" {
		t.Fatalf("unique codex wrapper in agentKeeper not bound: %+v", got)
	}
	if got["codex:01a21f51-42d1-7391-8f0d-4e7a7f3eaadd"].ID != "w-rc" {
		t.Fatalf("unique codex wrapper in resetcalendar not bound to newest session: %+v", got)
	}
	if _, bad := got["codex:01a20522-3346-7641-a879-066bed9d0916"]; bad {
		t.Fatalf("older session before wrapper start should not bind: %+v", got)
	}
	if _, bad := got["claude:old-claude"]; bad {
		t.Fatalf("authoritative-list harness (claude) should not folder-bind: %+v", got)
	}

	// Ambiguity check 1: two wrappers in the same folder -> must NOT bind.
	dupWrappers := append(append([]deliver.WrapperInfo(nil), wrappers...), deliver.WrapperInfo{
		ID: "w-ak-2", Harness: "codex", Cwd: "/work/agentKeeper", Argv: []string{"codex"}, Started: t0.Add(33 * time.Minute), PID: 104,
	})
	gotDup := ResolveWrapperBindings(r, sessions, nil, dupWrappers, procs, func(pid int) string { return cwds[pid] })
	if _, bad := gotDup["codex:01a21f5a-9699-7562-aed6-b0d6c171d2e5"]; bad {
		t.Fatalf("two wrappers in same folder must not bind: %+v", gotDup)
	}

	// Ambiguity check 2: one wrapper + a second unwrapped codex process in same folder -> must NOT bind.
	extraProcs := append(append([]proc.Process(nil), procs...), proc.Process{PID: 299, Args: []string{"codex"}, Line: "codex"})
	cwds[299] = "/work/agentKeeper"
	gotExtra := ResolveWrapperBindings(r, sessions, nil, wrappers, extraProcs, func(pid int) string { return cwds[pid] })
	if _, bad := gotExtra["codex:01a21f5a-9699-7562-aed6-b0d6c171d2e5"]; bad {
		t.Fatalf("second unwrapped process in same folder must block binding: %+v", gotExtra)
	}
}

func TestReconcileSkipsPreExistingAntigravitySubagentJob(t *testing.T) {
	h := setup(t)
	subPath := filepath.Join(t.TempDir(), "transcript_full.jsonl")
	step0 := `{"step_index":0,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","created_at":"2026-11-28T09:08:46Z","content":"<SYSTEM_MESSAGE>\n[Message] timestamp=2026-11-28T09:08:46Z sender=parent-1 priority=HIGH content=review\n</SYSTEM_MESSAGE>"}`
	if err := os.WriteFile(subPath, []byte(step0+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.d.Jobs["antigravity:sub-1"] = &core.Job{
		Key: "antigravity:sub-1", Harness: "antigravity", SessionID: "sub-1",
		TranscriptPath: subPath, Kind: core.KindUsageLimit, State: core.StatePending,
		WaitingOpen: true, StaleCopyPID: 42, WaitingSince: h.now,
		LimitAt: h.now, DueAt: h.now.Add(120 * time.Hour),
	}
	h.d.reconcileLoadedJobs()
	j := h.d.Jobs["antigravity:sub-1"]
	if j.State != core.StateSkipped {
		t.Fatalf("expected subagent job skipped by reconcile, got %s", j.State)
	}
	if j.WaitingOpen || j.StaleCopyPID != 0 || !j.WaitingSince.IsZero() {
		t.Fatalf("jobstate transition did not clear wait flags: %+v", j)
	}
}

func TestRunJobBindsUniqueLiveWrapper(t *testing.T) {
	h := setup(t)
	wdir := filepath.Join(h.d.Paths.Wrappers, "w-codex-live")
	if err := os.MkdirAll(wdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wdir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	meta := `{"harness":"codex","cwd":"/work/ak","argv":["codex"],"started":"` + h.now.Add(-5*time.Minute).Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(wdir, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	h.d.Sessions["codex:01a21f5a-9699-7562-aed6-b0d6c171d2e5"] = &store.Session{
		Harness: "codex", SessionID: "01a21f5a-9699-7562-aed6-b0d6c171d2e5",
		Cwd: "/work/ak", LastSeen: h.now.Add(-2 * time.Minute),
	}
	h.d.Jobs["codex:01a21f5a-9699-7562-aed6-b0d6c171d2e5"] = &core.Job{
		Key: "codex:01a21f5a-9699-7562-aed6-b0d6c171d2e5", Harness: "codex",
		SessionID: "01a21f5a-9699-7562-aed6-b0d6c171d2e5", Cwd: "/work/ak",
		Kind: core.KindUsageLimit, State: core.StatePending,
		LimitAt: h.now.Add(-2 * time.Minute), DueAt: h.now,
	}
	h.d.runDue()
	if len(h.delivered) != 1 {
		t.Fatalf("expected 1 delivery, got %d", len(h.delivered))
	}
	if got := h.delivered[0].Location.String(); got != "wrapper:w-codex-live" {
		t.Fatalf("expected delivery via uniquely bound wrapper, got %s", got)
	}
}

func TestInstanceLifecycleAndCrashRecovery(t *testing.T) {
	h := setup(t)

	// 1. Wrapper start registers a first-class Instance and pre-populates ActiveSessionID from argv.
	wResumeDir := filepath.Join(h.d.Paths.Wrappers, "w-res")
	if err := os.MkdirAll(wResumeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wResumeDir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	metaRes := `{"harness":"codex","cwd":"/work/app","argv":["codex","resume","01a21f63-a12b-7451-8916-0dda329d4611"],"started":"` + h.now.Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(wResumeDir, "meta.json"), []byte(metaRes), 0o600); err != nil {
		t.Fatal(err)
	}
	h.d.Handle(core.Event{
		Harness: "codex", Kind: core.KindWrapperStart, Cwd: "/work/app", PID: os.Getpid(),
		At: h.now, Source: "wrapper", Location: &core.Location{Kind: "wrapper", Target: "w-res"},
	})
	inst := h.d.Instances["w-res"]
	if inst == nil || inst.ActiveSessionID != "01a21f63-a12b-7451-8916-0dda329d4611" {
		t.Fatalf("expected instance with resume ActiveSessionID, got %+v", inst)
	}

	// 2. One Instance hosting multiple conversations (/clear): ActiveSessionID updates and drives binding.
	wSwitchDir := filepath.Join(h.d.Paths.Wrappers, "w-sw")
	if err := os.MkdirAll(wSwitchDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wSwitchDir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	metaSw := `{"harness":"antigravity","cwd":"/work/multi","argv":["agy"],"started":"` + h.now.Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(wSwitchDir, "meta.json"), []byte(metaSw), 0o600); err != nil {
		t.Fatal(err)
	}
	locSw := &core.Location{Kind: "wrapper", Target: "w-sw"}
	h.d.Handle(core.Event{Harness: "antigravity", Kind: core.KindWrapperStart, Cwd: "/work/multi", PID: os.Getpid(), At: h.now, Source: "wrapper", Location: locSw})
	h.d.Handle(core.Event{Harness: "antigravity", Kind: core.KindSessionStart, SessionID: "conv-1", Cwd: "/work/multi", PID: os.Getpid(), At: h.now.Add(time.Second), Source: "hook:SessionStart", Location: locSw})
	h.d.Handle(core.Event{Harness: "antigravity", Kind: core.KindSessionStart, SessionID: "conv-2", Cwd: "/work/multi", PID: os.Getpid(), At: h.now.Add(2 * time.Second), Source: "hook:SessionStart", Location: locSw})
	if got := h.d.Instances["w-sw"].ActiveSessionID; got != "conv-2" {
		t.Fatalf("expected ActiveSessionID=conv-2 after second session start, got %q", got)
	}
	// Even if conv-1 had a newer LastSeen timestamp, Instance.ActiveSessionID binds conv-2.
	h.d.Sessions["antigravity:conv-1"].LastSeen = h.now.Add(10 * time.Second)
	bindings := ResolveWrapperBindings(h.d.Rules, h.d.Sessions, h.d.Instances, deliver.LiveWrappers(h.d.Paths), nil, nil)
	if bindings["antigravity:conv-2"].ID != "w-sw" {
		t.Fatalf("expected w-sw bound to ActiveSessionID conv-2, got %+v", bindings)
	}
	if _, ok := bindings["antigravity:conv-1"]; ok {
		t.Fatalf("old conversation conv-1 should not hold w-sw once conv-2 is active: %+v", bindings)
	}

	// 3. Instances persist across daemon restart (instances.json).
	h.d.save()
	reloaded, err := New(h.d.Paths)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Instances["w-sw"] == nil || reloaded.Instances["w-sw"].ActiveSessionID != "conv-2" {
		t.Fatalf("instances did not persist across reload: %+v", reloaded.Instances)
	}

	// 4. Crash recovery (kill -9): wrapper dir vanishes without KindWrapperExit -> reconcileInstances cleans up.
	h.d.Handle(core.Event{
		Harness: "antigravity", Kind: core.KindUsageLimit, Cwd: "/work/multi",
		At: h.now.Add(5 * time.Second), ResetAt: h.now.Add(2 * time.Hour),
		Source: "wrapper", Location: locSw, Message: "Individual quota reached. Resets in 2h.",
	})
	if err := os.RemoveAll(wSwitchDir); err != nil {
		t.Fatal(err)
	}
	h.d.reconcileInstances()
	if _, exists := h.d.Instances["w-sw"]; exists {
		t.Fatal("dead wrapper instance was not removed by reconcileInstances")
	}
	if s := h.d.Sessions["antigravity:conv-2"]; s == nil || !s.Ended || s.Location != nil {
		t.Fatalf("crash recovery did not end session or clear dead wrapper location: %+v", s)
	}
	if j := h.d.Jobs["antigravity@wrapper:w-sw"]; j == nil || j.State != core.StateSkipped {
		t.Fatalf("crash recovery did not skip wrapper-only job immediately: %+v", j)
	}
}

func TestJobPhaseAndTransitions(t *testing.T) {
	now := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	if got := JobPhaseOf(nil); got != PhaseNone {
		t.Fatalf("nil job phase = %q", got)
	}
	j := &core.Job{State: core.StatePending}
	if got := JobPhaseOf(j); got != PhaseScheduled {
		t.Fatalf("pending phase = %q", got)
	}
	if first := markJobWaitingOpen(j, "open in desktop", now); !first || JobPhaseOf(j) != PhaseWaitingOpen {
		t.Fatalf("waiting open transition failed: first=%v phase=%q", first, JobPhaseOf(j))
	}
	if first := markJobWaitingStaleCopy(j, 999, "stale copy", now); !first || JobPhaseOf(j) != PhaseWaitingStaleCopy {
		t.Fatalf("waiting stale copy transition failed: first=%v phase=%q", first, JobPhaseOf(j))
	}
	markJobDelivered(j, "wrapper", "typed", now)
	if JobPhaseOf(j) != PhaseDelivered || j.WaitingOpen || j.StaleCopyPID != 0 || !j.WaitingSince.IsZero() {
		t.Fatalf("delivered transition left stale wait flags: %+v", j)
	}
	markJobTurnFailed(j, "API error", now.Add(time.Second), now.Add(time.Second))
	if JobPhaseOf(j) != PhaseTurnFailed || !j.ResumeSettled {
		t.Fatalf("turn failed phase = %q settled=%v", JobPhaseOf(j), j.ResumeSettled)
	}
	markJobResumeNow(j, now.Add(2*time.Second))
	if JobPhaseOf(j) != PhaseScheduled || j.TurnFailed != "" || j.ResumeSettled {
		t.Fatalf("resume-now did not cleanly reset state: %+v", j)
	}
	markJobNeedsUser(j, "billing", true, now)
	if JobPhaseOf(j) != PhaseNeedsUser || !j.ManualResumeOnly {
		t.Fatalf("needs_user transition failed: %+v", j)
	}
	markJobIgnored(j, "not chosen", now)
	if JobPhaseOf(j) != PhaseIgnored {
		t.Fatalf("ignored phase = %q", JobPhaseOf(j))
	}
	markJobCancelled(j, "cancelled", now)
	if JobPhaseOf(j) != PhaseCancelled {
		t.Fatalf("cancelled phase = %q", JobPhaseOf(j))
	}
	markJobFailed(j, "gone", now)
	if JobPhaseOf(j) != PhaseFailed {
		t.Fatalf("failed phase = %q", JobPhaseOf(j))
	}
	markJobSkipped(j, "continued", now)
	if JobPhaseOf(j) != PhaseSkipped {
		t.Fatalf("skipped phase = %q", JobPhaseOf(j))
	}
}

func TestSnapshotCanonicalAndInvalidation(t *testing.T) {
	h := setup(t)
	h.d.Config.Harnesses = map[string]bool{"claude": false, "codex": true, "antigravity": false}
	if err := store.SaveJSON(h.d.Paths.Config, h.d.Config); err != nil {
		t.Fatal(err)
	}
	wdir := filepath.Join(h.d.Paths.Wrappers, "w-snap")
	if err := os.MkdirAll(wdir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wdir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	meta := `{"harness":"codex","cwd":"/work/snap","argv":["codex"],"started":"` + time.Now().Add(-2*time.Minute).Format(time.RFC3339) + `"}`
	if err := os.WriteFile(filepath.Join(wdir, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	h.d.Sessions["codex:01a21f5a-0000-0000-0000-000000000001"] = &store.Session{
		Harness: "codex", SessionID: "01a21f5a-0000-0000-0000-000000000001",
		Cwd: "/work/snap", LastSeen: time.Now().Add(-time.Minute),
	}
	h.d.dirty = true
	h.d.PublishSnapshot = true
	h.d.save()
	h.d.maybePublishSnapshot(true)

	// Saved snapshot should be read directly when daemonRunning=true and nothing changed.
	loaded, ok := LoadSnapshot(h.d.Paths)
	if !ok {
		t.Fatal("expected state.json snapshot to be persisted")
	}
	snap := LoadOrBuildSnapshot(h.d.Paths, h.d.Config, h.d.Rules, true)
	if !snap.GeneratedAt.Equal(loaded.GeneratedAt) {
		t.Fatalf("expected fresh snapshot cache hit (%s), got rebuilt (%s)", loaded.GeneratedAt, snap.GeneratedAt)
	}
	if len(snap.Sessions) != 1 || !snap.Sessions[0].Open || snap.Sessions[0].Location == nil || snap.Sessions[0].Location.Target != "w-snap" {
		t.Fatalf("expected bound codex session in snapshot, got %+v", snap.Sessions)
	}

	// Config change (track_closed_sessions toggled) invalidates snapshot immediately.
	off := false
	cfgOff := h.d.Config
	cfgOff.TrackClosedSessions = &off
	if snapshotFresh(h.d.Paths, cfgOff, loaded, time.Now()) {
		t.Fatal("expected TrackClosedSessions mismatch to invalidate snapshot")
	}
	// Dead DaemonPID invalidates snapshot.
	deadSnap := loaded
	deadSnap.DaemonPID = deadPID
	if snapshotFresh(h.d.Paths, h.d.Config, deadSnap, time.Now()) {
		t.Fatal("expected dead DaemonPID to invalidate snapshot")
	}
	// Expired snapshot invalidates cache.
	oldSnap := loaded
	oldSnap.GeneratedAt = time.Now().Add(-time.Minute)
	if snapshotFresh(h.d.Paths, h.d.Config, oldSnap, time.Now()) {
		t.Fatal("expected expired snapshot to be rejected")
	}
	// daemonRunning=false bypasses cached snapshot and rebuilds.
	rebuilt := LoadOrBuildSnapshot(h.d.Paths, h.d.Config, h.d.Rules, false)
	if rebuilt.GeneratedAt.Equal(loaded.GeneratedAt) {
		t.Fatal("expected daemonRunning=false to rebuild snapshot instead of trusting state.json")
	}
}

func TestClosedNonHeadlessSessionFailsCleanlyWhenDue(t *testing.T) {
	h := setup(t)
	h.d.Handle(core.Event{
		Harness: "antigravity", Kind: core.KindUsageLimit, SessionID: "ag-gone", Cwd: "/work/research",
		At: h.now, ResetAt: h.now.Add(30 * time.Minute), Source: "hook:Stop", PID: deadPID,
		Location: &core.Location{Kind: "wrapper", Target: "w-gone"},
		Message:  "Individual quota reached. Resets in 30m.",
	})
	h.d.Handle(core.Event{
		Harness: "antigravity", Kind: core.KindWrapperExit, Cwd: "/work/research",
		At: h.now.Add(2 * time.Minute), Source: "wrapper",
		Location: &core.Location{Kind: "wrapper", Target: "w-gone"},
	})
	// Even if another agy process is running in the same folder (ambiguous process scan),
	// ag-gone ended and has no route or native resume command.
	h.live = &live.State{Checked: true, Open: true, Source: "processes", ProcessMatch: false}
	h.advance(35 * time.Minute)

	j := h.d.Jobs["antigravity:ag-gone"]
	if j == nil || j.State != core.StateFailed || j.Result != deliver.ErrGone.Error() || j.Attempts != 0 {
		t.Fatalf("expected closed non-headless job to fail cleanly with ErrGone (0 attempts), got %+v", j)
	}
	if len(h.delivered) != 0 {
		t.Fatalf("closed job should not have been delivered, got %d deliveries", len(h.delivered))
	}
}

func TestStaleWrapperRedrawAndAntigravityUserInputDoNotCorruptPendingResume(t *testing.T) {
	h := setup(t)
	makeLiveWrapper(t, h.d.Paths, "w-reset")
	loc := &core.Location{Kind: "wrapper", Target: "w-reset"}

	// 1. Prompt #1 hits quota at t0; hook:Stop schedules resume at t0+47m (reset t0+46m).
	reset1 := h.now.Add(46 * time.Minute)
	h.d.Handle(core.Event{
		Harness: "antigravity", Kind: core.KindWrapperStart, Cwd: "/work/resetcalendar",
		At: h.now.Add(-time.Minute), Source: "wrapper", Location: loc, PID: os.Getpid(),
	})
	h.d.Handle(core.Event{
		Harness: "antigravity", Kind: core.KindUsageLimit, SessionID: "conv-reset", Cwd: "/work/resetcalendar",
		At: h.now, ResetAt: reset1, Source: "hook:Stop", Location: loc, PID: os.Getpid(),
		Message: "Individual quota reached. Resets in 46m.",
	})

	// 2. 43 minutes later, a stale wrapper redraw re-reports "Resets in 46m" (parsed as t0+89m).
	// It must merge into the existing authoritative job without pushing ResetAt out or spawning a duplicate.
	h.now = h.now.Add(43 * time.Minute)
	h.d.Handle(core.Event{
		Harness: "antigravity", Kind: core.KindUsageLimit, Cwd: "/work/resetcalendar",
		At: h.now, ResetAt: h.now.Add(46 * time.Minute), Source: "wrapper", Location: loc,
		Message: "Individual quota reached. Resets in 46m.", Hit: "quota reached",
	})
	if len(h.d.Jobs) != 1 {
		t.Fatalf("stale wrapper redraw should not spawn a duplicate job, got %+v", h.d.Jobs)
	}
	j := h.d.Jobs["antigravity:conv-reset"]
	if j == nil || j.State != core.StatePending || !j.ResetAt.Equal(reset1) {
		t.Fatalf("stale wrapper redraw corrupted authoritative job: %+v (want reset %v)", j, reset1)
	}

	// 2b. A stale wrapper redraw arriving while j is in retry backoff (even past
	// the original reset time) must merge without clobbering the backoff DueAt.
	backoffDue := h.now.Add(30 * time.Minute)
	j.Attempts = 2
	j.DueAt = backoffDue
	h.d.Handle(core.Event{
		Harness: "antigravity", Kind: core.KindUsageLimit, Cwd: "/work/resetcalendar",
		At: reset1.Add(5 * time.Minute), ResetAt: reset1.Add(51 * time.Minute), Source: "wrapper", Location: loc,
		Message: "Individual quota reached. Resets in 46m.", Hit: "quota reached",
	})
	if len(h.d.Jobs) != 1 || !j.DueAt.Equal(backoffDue) || !j.ResetAt.Equal(reset1) {
		t.Fatalf("stale wrapper redraw clobbered backoff/reset: jobs=%d due=%v (want %v) reset=%v (want %v)", len(h.d.Jobs), j.DueAt, backoffDue, j.ResetAt, reset1)
	}

	// 3. Even if a stale wrapper job already existed first and hook:Stop arrives after it,
	// hook:Stop's authoritative ResetAt, LimitAt, and Message must win.
	loc2 := &core.Location{Kind: "wrapper", Target: "w-reset2"}
	makeLiveWrapper(t, h.d.Paths, "w-reset2")
	bogusReset := h.now.Add(46 * time.Minute)
	h.d.Handle(core.Event{
		Harness: "antigravity", Kind: core.KindUsageLimit, Cwd: "/work/other",
		At: h.now, ResetAt: bogusReset, Source: "wrapper", Location: loc2,
		Message: "⣾\nIndividual quota reached. Resets in 46m.", Hit: "quota reached",
	})
	h.now = h.now.Add(90 * time.Second)
	realReset := h.now.Add(90 * time.Second)
	h.d.Handle(core.Event{
		Harness: "antigravity", Kind: core.KindUsageLimit, SessionID: "conv-reset2", Cwd: "/work/other",
		At: h.now, ResetAt: realReset, Source: "hook:Stop", Location: loc2, PID: os.Getpid(),
		Message: "Individual quota reached. Resets in 1m30s.",
	})
	if len(h.d.Jobs) != 2 {
		t.Fatalf("expected twin to be re-keyed with 2 total jobs, got %d: %+v", len(h.d.Jobs), h.d.Jobs)
	}
	j2 := h.d.Jobs["antigravity:conv-reset2"]
	if j2 == nil {
		t.Fatalf("expected antigravity:conv-reset2 job, got nil (jobs: %+v)", h.d.Jobs)
	}
	if j2.State != core.StatePending || !j2.ResetAt.Equal(realReset) || !j2.LimitAt.Equal(h.now) || j2.Message != "Individual quota reached. Resets in 1m30s." {
		t.Fatalf("authoritative hook:Stop should override screen-only twin: got state=%s reset=%v limit=%v msg=%q", j2.State, j2.ResetAt, j2.LimitAt, j2.Message)
	}

	// 4. A USER_INPUT transcript event (TurnStart=true) after LimitAt must NOT cancel the pending resume,
	// neither via Handle nor via sessionActiveSince before delivery.
	tp := filepath.Join(t.TempDir(), "brain", "conv-reset2", ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(tp), 0o700); err != nil {
		t.Fatal(err)
	}
	line1 := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"` + h.now.Add(-time.Minute).UTC().Format(time.RFC3339) + `","content":"first"}` + "\n"
	line2 := `{"step_index":2,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"` + h.now.Add(5*time.Second).UTC().Format(time.RFC3339) + `","content":"second"}` + "\n"
	if err := os.WriteFile(tp, []byte(line1+line2), 0o600); err != nil {
		t.Fatal(err)
	}
	j2.TranscriptPath = tp
	j2.TranscriptSize = int64(len(line1))
	h.d.Handle(core.Event{
		Harness: "antigravity", Kind: core.KindActivity, SessionID: "conv-reset2",
		TranscriptPath: tp, At: h.now.Add(5 * time.Second), Source: "transcript", TurnStart: true,
	})
	if j2.State != core.StatePending {
		t.Fatalf("USER_INPUT TurnStart should not cancel pending resume, got state=%s result=%q", j2.State, j2.Result)
	}
	if h.d.sessionActiveSince(j2) {
		t.Fatal("sessionActiveSince should ignore USER_INPUT TurnStart lines")
	}

	// 5. Once a PLANNER_RESPONSE DONE appears after LimitAt (even followed by another USER_INPUT),
	// sessionActiveSince MUST detect that the session continued.
	line3 := `{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"` + h.now.Add(10*time.Second).UTC().Format(time.RFC3339) + `","model":"gemini-4-neon-high","content":"done"}` + "\n"
	line4 := `{"step_index":4,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"` + h.now.Add(15*time.Second).UTC().Format(time.RFC3339) + `","content":"third"}` + "\n"
	if err := os.WriteFile(tp, []byte(line1+line2+line3+line4), 0o600); err != nil {
		t.Fatal(err)
	}
	if !h.d.sessionActiveSince(j2) {
		t.Fatal("sessionActiveSince should detect PLANNER_RESPONSE DONE even when followed by USER_INPUT")
	}
}

func TestFindTwinDistinguishesRedrawsFromFreshLimits(t *testing.T) {
	h := setup(t)
	loc := &core.Location{Kind: "wrapper", Target: "shared-wrapper"}
	j := &core.Job{
		Harness: "antigravity", SessionID: "conv", State: core.StatePending,
		Kind: core.KindUsageLimit, Location: loc, LimitAt: h.now,
		Message: "Individual quota reached. Resets in 20m.", Sources: []string{"hook:Stop"},
	}
	h.d.Jobs["antigravity:conv"] = j
	ev := core.Event{Harness: j.Harness, Kind: j.Kind, Location: loc, Source: "wrapper", At: h.now.Add(4 * time.Minute)}
	ev.Message = "Individual quota reached. Resets in 2h."
	if twin := h.d.findTwin(ev, jobKey(ev)); twin != nil {
		t.Fatalf("fresh limit matched a different notice: %+v", twin)
	}
	ev.Message = "⣾\n" + j.Message
	if twin := h.d.findTwin(ev, jobKey(ev)); twin != j {
		t.Fatalf("redraw failed to match: %+v", twin)
	}
}

func TestMergePrefersDelayedAuthoritativeTimeAndReset(t *testing.T) {
	h := setup(t)
	j := &core.Job{
		Harness: "antigravity", State: core.StatePending, Kind: core.KindUsageLimit,
		LimitAt: h.now, ResetAt: h.now.Add(time.Hour), ResetKnown: true, Sources: []string{"wrapper"},
	}
	at := h.now.Add(-time.Second)
	ev := core.Event{Harness: j.Harness, Kind: j.Kind, Source: "hook:Stop", At: at}
	h.d.merge(j, ev)
	if !j.LimitAt.Equal(at) || !j.ResetFromScreen {
		t.Fatalf("delayed hook adoption lost provenance or timestamp: %+v", j)
	}
	ev.ResetAt = at.Add(10 * time.Minute)
	h.d.merge(j, ev)
	if !j.ResetAt.Equal(ev.ResetAt) || j.ResetFromScreen {
		t.Fatalf("authoritative reset failed to replace a screen estimate: %+v", j)
	}
}
