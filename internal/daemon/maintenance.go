package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/messages"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/scope"
	"github.com/continuum-dev/continuum/internal/store"
)

// wrapperStarted registers a running `continuum run` PTY wrapper instance as a
// first-class Instance distinct from any Conversation (Session) it hosts.
func (d *Daemon) wrapperStarted(ev core.Event) {
	if ev.Location == nil || ev.Location.Kind != "wrapper" || ev.Location.Target == "" {
		return
	}
	wid := ev.Location.Target
	now := d.Now()
	started := ev.At
	if started.IsZero() {
		started = now
	}
	inst := d.Instances[wid]
	if inst == nil {
		inst = &store.Instance{ID: wid, Location: ev.Location}
		d.Instances[wid] = inst
	}
	if ev.Harness != "" {
		inst.Harness = ev.Harness
	}
	if ev.Cwd != "" {
		inst.Cwd = ev.Cwd
	}
	if ev.PID > 0 {
		inst.PID = ev.PID
	}
	if inst.StartedAt.IsZero() {
		inst.StartedAt = started
	}
	if info, ok := deliver.ReadWrapperInfo(d.Paths, wid); ok {
		if len(info.Argv) > 0 {
			inst.Argv = info.Argv
		}
		if !info.Started.IsZero() {
			inst.StartedAt = info.Started
		}
		if info.Cwd != "" && inst.Cwd == "" {
			inst.Cwd = info.Cwd
		}
		if info.PID > 0 && inst.PID == 0 {
			inst.PID = info.PID
		}
	}
	if inst.ActiveSessionID == "" {
		if id := WrapperResumeSessionID(inst.Harness, inst.Argv); id != "" {
			inst.ActiveSessionID = id
		}
	}
	inst.UpdatedAt = now
	d.dirty = true
}

// wrapperExited handles an explicit exit event from `continuum run`: deletes
// the tracked Instance and clears dead wrapper routes immediately.
func (d *Daemon) wrapperExited(ev core.Event) {
	if ev.Location == nil || ev.Location.Kind != "wrapper" || ev.Location.Target == "" {
		return
	}
	wid := ev.Location.Target
	at := ev.At
	if at.IsZero() {
		at = d.Now()
	}
	if _, existed := d.Instances[wid]; existed {
		delete(d.Instances, wid)
		d.dirty = true
	}
	d.cleanupDeadWrapper(wid, ev.PID, at)
}

// cleanupDeadWrapper clears references to a wrapper instance that exited or
// vanished (kill -9 / reboot), marks its sessions ended, and skips wrapper-only
// or closed-untracked jobs immediately.
func (d *Daemon) cleanupDeadWrapper(wid string, exitPID int, at time.Time) {
	now := d.Now()
	if at.IsZero() {
		at = now
	}
	for _, s := range d.Sessions {
		if s == nil || s.Location == nil || s.Location.Kind != "wrapper" || s.Location.Target != wid {
			continue
		}
		s.Location = nil
		if (exitPID > 0 && s.PID == exitPID) || (s.PID > 0 && !proc.Alive(s.PID)) {
			s.PID = 0
		}
		if !s.Ended {
			s.Ended = true
			s.EndedAt = at
		}
		d.dirty = true
	}
	for _, j := range store.SortedJobs(d.Jobs) {
		if j.State != core.StatePending && j.State != core.StateNeedsUser {
			continue
		}
		if j.Location == nil || j.Location.Kind != "wrapper" || j.Location.Target != wid {
			continue
		}
		if j.SessionID == "" {
			if o := d.sameStall(j); o != nil {
				d.skip(j, "its terminal closed; most likely the same stall as "+o.Key+" (same folder and time), which is still to be resumed")
			} else {
				d.skip(j, "its terminal closed, and without a session id nothing else can continue it")
				d.notify(j, "Won't resume", "Its terminal closed, and nothing else can continue it.")
			}
			continue
		}
		if !d.Config.ClosedSessionsTracked() {
			var st *live.State
			if d.LiveFn != nil {
				st = d.LiveFn(j)
			}
			if st == nil || !st.Open || (!st.Authoritative && !st.ProcessMatch && st.DesktopApp == "" && !st.Background) {
				d.skip(j, "the session closed (track_closed_sessions is off)")
				continue
			}
		}
		d.debugf("%s: its wrapper %s exited; forgetting its pane", j.Key, wid)
		j.Location = nil
		if (exitPID > 0 && j.PID == exitPID) || (j.PID > 0 && !proc.Alive(j.PID)) {
			j.PID = 0
		}
		j.UpdatedAt = now
		d.dirty = true
	}
}

// reconcileInstances syncs d.Instances against currently live wrappers on disk:
// registers wrappers launched before the daemon started, and cleans up tracked
// instances whose wrapper process vanished without emitting wrapper_exit
// (e.g. kill -9 or reboot).
func (d *Daemon) reconcileInstances() {
	liveWrappers := deliver.LiveWrappers(d.Paths)
	alive := make(map[string]deliver.WrapperInfo, len(liveWrappers))
	now := d.Now()
	for _, w := range liveWrappers {
		alive[w.ID] = w
		inst := d.Instances[w.ID]
		if inst == nil {
			inst = &store.Instance{
				ID:        w.ID,
				Harness:   w.Harness,
				Location:  &core.Location{Kind: "wrapper", Target: w.ID},
				Cwd:       w.Cwd,
				Argv:      w.Argv,
				PID:       w.PID,
				StartedAt: w.Started,
				UpdatedAt: now,
			}
			if id := WrapperResumeSessionID(w.Harness, w.Argv); id != "" {
				inst.ActiveSessionID = id
			}
			d.Instances[w.ID] = inst
			d.dirty = true
			continue
		}
		changed := false
		if inst.Harness == "" && w.Harness != "" {
			inst.Harness, changed = w.Harness, true
		}
		if inst.Cwd == "" && w.Cwd != "" {
			inst.Cwd, changed = w.Cwd, true
		}
		if len(inst.Argv) == 0 && len(w.Argv) > 0 {
			inst.Argv, changed = w.Argv, true
			if inst.ActiveSessionID == "" {
				if id := WrapperResumeSessionID(w.Harness, w.Argv); id != "" {
					inst.ActiveSessionID = id
				}
			}
		}
		if inst.PID != w.PID {
			inst.PID, changed = w.PID, true
		}
		if inst.StartedAt.IsZero() && !w.Started.IsZero() {
			inst.StartedAt, changed = w.Started, true
		}
		if changed {
			inst.UpdatedAt = now
			d.dirty = true
		}
	}
	for wid, inst := range d.Instances {
		if _, ok := alive[wid]; ok {
			continue
		}
		delete(d.Instances, wid)
		d.dirty = true
		pid := 0
		if inst != nil {
			pid = inst.PID
		}
		d.cleanupDeadWrapper(wid, pid, now)
	}
}

// boundWrapper resolves a uniquely matching live Continuum wrapper for
// (harness, sessionID), sharing the exact same binding logic as the CLI.
func (d *Daemon) boundWrapper(harness, sessionID string) (deliver.WrapperInfo, bool) {
	if sessionID == "" {
		return deliver.WrapperInfo{}, false
	}
	wrappers := deliver.LiveWrappers(d.Paths)
	if len(wrappers) == 0 {
		return deliver.WrapperInfo{}, false
	}
	procs, _ := proc.List()
	bindings := ResolveWrapperBindings(d.Rules, d.Sessions, d.Instances, wrappers, procs, proc.Cwd)
	w, ok := bindings[store.SessionKey(harness, sessionID)]
	return w, ok
}

// sessionClosed reports whether j's session is known to be closed.
func (d *Daemon) sessionClosed(j *core.Job, sess *store.Session, st *live.State) bool {
	return SessionClosed(d.Paths, j, sess, st)
}

func (d *Daemon) prune() {
	d.pruneClosed()
	cutoff := d.Now().Add(-72 * time.Hour)
	for _, j := range d.Jobs {
		// A resume known only by its wrapper (no session id for the agent's
		// own resume command) can never be delivered once that wrapper has
		// exited: wrapper ids are never reused. Don't list it, keep the
		// computer awake for it, or retry it. Not straight away: an event
		// with its session id may still arrive and adopt it (findTwin).
		if j.State == core.StatePending && j.SessionID == "" && j.Location != nil &&
			d.Now().Sub(j.UpdatedAt) > 5*time.Minute &&
			j.Location.Kind == "wrapper" && !deliver.WrapperAlive(d.Paths, j.Location.Target) {
			if o := d.sameStall(j); o != nil {
				// Most likely its conversation's own resume (from its hooks)
				// covers it; once the terminal is gone that can't be checked.
				d.skip(j, "its terminal closed; most likely the same stall as "+o.Key+" (same folder and time), which is still to be resumed")
				continue
			}
			d.skip(j, "its terminal closed, and without a session id nothing else can continue it")
			d.notify(j, "Won't resume", "Its terminal closed, and nothing else can continue it.")
		}
	}
	for k, j := range d.Jobs {
		if j.LiftFailed && j.ResetKnown && j.ResetAt.After(d.Now()) {
			continue // still keeps answers from lifting its limit (lifted.go)
		}
		if j.State != core.StatePending && j.UpdatedAt.Before(cutoff) {
			delete(d.Jobs, k)
			d.dirty = true
		}
	}
	scut := d.Now().Add(-14 * 24 * time.Hour)
	for k, s := range d.Sessions {
		if s.LastSeen.Before(scut) {
			delete(d.Sessions, k)
			d.dirty = true
		}
	}
	// The resume history: keep the current file and one older one.
	if st, err := os.Stat(d.Paths.History); err == nil && st.Size() > 5<<20 {
		_ = os.Rename(d.Paths.History, d.Paths.History+".1")
	}
	// Payload files Continuum 0.15.9 handed to hook workers that never ran.
	if old, _ := filepath.Glob(filepath.Join(d.Paths.Home, "hook-input-*.json")); len(old) > 0 {
		for _, f := range old {
			if info, err := os.Stat(f); err == nil && time.Since(info.ModTime()) > time.Hour {
				_ = os.Remove(f)
			}
		}
	}
	// Folders of wrappers that were killed (kill -9) and couldn't clean up.
	if entries, err := os.ReadDir(d.Paths.Wrappers); err == nil {
		for _, e := range entries {
			dir := filepath.Join(d.Paths.Wrappers, e.Name())
			info, err := e.Info()
			if err != nil || !e.IsDir() || time.Since(info.ModTime()) < 10*time.Minute || deliver.WrapperAlive(d.Paths, e.Name()) {
				continue
			}
			_ = os.RemoveAll(dir)
		}
	}
}

// pruneClosed drops pending and needs-user resumes whose sessions are closed
// when track_closed_sessions is off.
func (d *Daemon) pruneClosed() {
	if d.Config.ClosedSessionsTracked() {
		d.lastClosedCheck = time.Time{}
		return
	}
	now := d.Now()
	full := d.lastClosedCheck.IsZero() || now.Sub(d.lastClosedCheck) >= 15*time.Second
	checkedAny := false
	for _, j := range store.SortedJobs(d.Jobs) {
		if j.State != core.StatePending && j.State != core.StateNeedsUser {
			continue
		}
		if DriverFor(j.Harness).ManagesOwnLifecycle(j) {
			continue
		}
		if j.Location != nil && j.Location.Kind == "wrapper" && deliver.WrapperAlive(d.Paths, j.Location.Target) {
			continue
		}
		sess := d.Sessions[store.SessionKey(j.Harness, j.SessionID)]
		suspect := (sess != nil && sess.Ended) ||
			(j.Location != nil && j.Location.Kind == "wrapper") ||
			(j.PID > 0 && !proc.Alive(j.PID))
		if !suspect && !full {
			continue
		}
		var st *live.State
		if j.SessionID != "" && d.LiveFn != nil {
			st = d.LiveFn(j)
			checkedAny = true
		}
		if d.sessionClosed(j, sess, st) {
			d.skip(j, "the session closed (track_closed_sessions is off)")
		}
	}
	if full && checkedAny {
		d.lastClosedCheck = now
	}
}

// keepAwake holds off idle sleep while a resume is due soon or a resumed
// session is still working, so a locked, unattended computer keeps going.
func (d *Daemon) keepAwake() {
	if d.Awake == nil {
		return
	}
	on, why := d.wantAwake()
	changed, err := d.Awake.Set(on, why)
	if err != nil {
		d.warnf("keep awake: %v", err)
	} else if changed && on {
		d.logf("keeping the computer awake: %s", why)
	} else if changed {
		d.logf("no longer keeping the computer awake")
	}
}

func (d *Daemon) wantAwake() (bool, string) {
	horizon := d.Config.KeepAwake()
	if horizon <= 0 || d.Config.DryRun {
		return false, ""
	}
	now := d.Now()
	for _, j := range d.Jobs {
		switch j.State {
		case core.StatePending:
			// A session held open in a desktop app waits for the user, who
			// is at the computer anyway: no reason to keep it awake. Same for
			// a closed session whose agent has no headless resume command.
			if j.DueAt.Sub(now) <= horizon && !j.WaitingOpen {
				if !d.resumesClosed(j.Harness) {
					sess := d.Sessions[store.SessionKey(j.Harness, j.SessionID)]
					if d.sessionClosed(j, sess, nil) {
						continue
					}
				}
				return true, "a resume is due at " + j.DueAt.Local().Format("15:04")
			}
		case core.StateDelivered:
			since := now.Sub(j.DeliveredAt)
			if since >= horizon {
				continue
			}
			s := d.Sessions[store.SessionKey(j.Harness, j.SessionID)]
			if !d.Config.ClosedSessionsTracked() && s != nil && s.Ended {
				continue
			}
			if since < 10*time.Minute {
				return true, "a session was just resumed"
			}
			if s != nil && now.Sub(s.LastSeen) < 10*time.Minute {
				return true, "a resumed session is still working"
			}
		}
	}
	return false, ""
}

// applyScope follows changes to the scope: a session chosen after it stalled
// is scheduled, and a pending resume that is no longer in scope is dropped.
func (d *Daemon) applyScope() {
	_ = messages.Update(d.Paths, func(m *messages.Messages) (bool, error) {
		return m.PruneWrappers(func(id string) bool { return deliver.WrapperAlive(d.Paths, id) }), nil
	})
	// Read afresh inside a file lock so pruning never undoes a concurrent `continuum choose`.
	if sc, err := scope.Update(d.Paths, func(cur scope.Scope) (scope.Scope, bool, error) {
		changed := cur.PruneWrappers(func(id string) bool { return deliver.WrapperAlive(d.Paths, id) })
		return cur, changed, nil
	}); err == nil {
		d.Scope = sc
	} else {
		d.logf("scope: %v", err)
	}
	now := d.Now()
	for _, j := range d.Jobs {
		switch j.State {
		case core.StatePending:
			if ok, why := d.Scope.Covers(j); !ok {
				markJobIgnored(j, why, now)
				d.history("ignored", j, map[string]any{"reason": why})
				d.logf("%s: %s", j.Key, why)
				d.dirty = true
			}
		case core.StateIgnored:
			if ok, _ := d.Scope.Covers(j); ok && j.LimitAt.After(now.Add(-d.Config.CatchUp())) {
				if !d.Config.ClosedSessionsTracked() {
					sess := d.Sessions[store.SessionKey(j.Harness, j.SessionID)]
					var st *live.State
					if j.SessionID != "" && d.LiveFn != nil {
						st = d.LiveFn(j)
					}
					if d.sessionClosed(j, sess, st) {
						continue
					}
				}
				markJobPending(j, now, now)
				d.reschedule(j)
				d.dirty = true
				if j.State == core.StatePending {
					d.history("scheduled", j, map[string]any{"due": j.DueAt.Format(time.RFC3339), "kind": j.Kind, "source": "chosen"})
					d.logf("%s: chosen, resume at %s", j.Key, j.DueAt.Format(time.RFC1123))
					d.notify(j, "Chosen", "Will continue at "+j.DueAt.In(d.Local).Format("Mon 3:04 PM"))
				}
			}
		}
	}
}

// applyTiming re-plans scheduled resumes when the wait settings change
// (`continuum grace`, delay_after_reset_seconds), so a change takes effect
// for sessions that are already waiting.
func (d *Daemon) applyTiming() {
	sig := fmt.Sprint(d.delay(), d.Config.NativeGrace)
	if d.timingSig == sig {
		return
	}
	first := d.timingSig == ""
	d.timingSig = sig
	if first {
		return
	}
	for _, j := range d.Jobs {
		if j.State == core.StatePending && j.ResetKnown && j.Attempts == 0 {
			old := j.DueAt
			d.reschedule(j)
			if !j.DueAt.Equal(old) {
				d.dirty = true
				d.logf("%s: wait settings changed, resume now at %s", j.Key, j.DueAt.Format(time.RFC1123))
			}
		}
	}
}
