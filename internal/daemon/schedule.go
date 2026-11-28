package daemon

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/opencode"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/resettime"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/store"
)

// continued drops pending resumes of ev's session (or place) whose stall came
// before ev: the session did something since. only, when set, picks which.
func (d *Daemon) continued(ev core.Event, only func(*core.Job) bool) {
	for _, j := range d.Jobs {
		if j.State != core.StatePending || !ev.At.After(j.LimitAt.Add(time.Second)) {
			continue
		}
		if ev.SessionID != "" && j.SessionID != "" {
			// A pane can be reused, and Codex's shared backend used to
			// report its launcher's wrapper for unrelated conversations.
			if ev.SessionID != j.SessionID || ev.Harness != j.Harness {
				continue
			}
		} else if ev.Location == nil || j.Location == nil || ev.Location.String() != j.Location.String() {
			continue
		}
		if only != nil && !only(j) {
			continue
		}
		if screenSourced(ev.Source) && !onlyScreenSources(j.Sources) {
			// The screen changing isn't the session moving on: Claude Code
			// clears its limit notice by itself when the limit resets. The
			// transcript or hooks that reported this stall report activity.
			d.debugf("%s: the screen changed, but the stall came from %s; waiting for the agent itself to show activity", j.Key, strings.Join(j.Sources, ", "))
			continue
		}
		markJobSkipped(j, "the session continued on its own or you continued it", d.Now())
		d.dirty = true
		d.history("skipped", j, map[string]any{"reason": j.Result})
		d.logf("%s: %s", j.Key, j.Result)
	}
}

// A reply that mentions limits, kept for a few minutes per place.
type replySeen struct {
	text string
	at   time.Time
}

const replyWindow = 3 * time.Minute

func screenSourced(sources ...string) bool {
	for _, s := range sources {
		if s != "wrapper" && s != "pane-scan" {
			return false
		}
	}
	return len(sources) > 0
}

// sameStall: a pending resume of a session (from its hooks) for the stall a
// screen-only resume j saw: same agent and folder, about the same time.
func (d *Daemon) sameStall(j *core.Job) *core.Job {
	for _, o := range d.Jobs {
		if o == j || o.SessionID == "" || o.State != core.StatePending || o.Harness != j.Harness ||
			o.Cwd == "" || j.Cwd == "" || filepath.Clean(o.Cwd) != filepath.Clean(j.Cwd) {
			continue
		}
		if dt := o.LimitAt.Sub(j.LimitAt); dt > -3*time.Minute && dt < 3*time.Minute {
			return o
		}
	}
	return nil
}

// endedSession: the session's process has quit.
func (d *Daemon) endedSession(harness, id string) bool {
	if id == "" {
		return false
	}
	s := d.Sessions[store.SessionKey(harness, id)]
	return s != nil && s.Ended
}

func replyHas(reply, hit string) bool {
	hit = strings.ToLower(strings.Join(strings.Fields(hit), " "))
	return hit != "" && strings.Contains(strings.ToLower(strings.Join(strings.Fields(reply), " ")), hit)
}

// reply records a normal turn end whose reply mentions limits, and drops a
// resume that was scheduled from that same text on screen.
func (d *Daemon) reply(ev core.Event, sess *store.Session) {
	loc := ev.Location
	if loc == nil && sess != nil {
		loc = sess.Location
	}
	if loc == nil || ev.Message == "" {
		return
	}
	if d.replies == nil {
		d.replies = map[string]replySeen{}
	}
	d.replies[loc.String()] = replySeen{text: ev.Message, at: ev.At}
	for _, j := range d.Jobs {
		if j.State != core.StatePending || j.Location == nil || j.Location.String() != loc.String() {
			continue
		}
		if !onlyScreenSources(j.Sources) || j.LimitAt.Sub(ev.At).Abs() > replyWindow || !replyHas(ev.Message, j.Hit) {
			continue
		}
		markJobSkipped(j, "the limit text on screen was part of the agent's reply, not an error", d.Now())
		d.dirty = true
		d.history("skipped", j, map[string]any{"reason": j.Result})
		d.logf("%s: %s", j.Key, j.Result)
	}
}

func onlyScreenSources(list []string) bool {
	for _, s := range list {
		if !screenSourced(s) {
			return false
		}
	}
	return len(list) > 0
}

// partOfReply: limit text read off the screen that the agent's own reply,
// just finished, contains.
func (d *Daemon) partOfReply(ev core.Event, sess *store.Session) bool {
	if !screenSourced(ev.Source) || ev.Location == nil || ev.Hit == "" {
		return false
	}
	r, ok := d.replies[ev.Location.String()]
	return ok && ev.At.Sub(r.at).Abs() <= replyWindow && replyHas(r.text, ev.Hit)
}

func (d *Daemon) touchSession(ev core.Event, copyQuit bool) *store.Session {
	if ev.SessionID == "" {
		return nil
	}
	key := store.SessionKey(ev.Harness, ev.SessionID)
	s := d.Sessions[key]
	if s == nil {
		s = &store.Session{Harness: ev.Harness, SessionID: ev.SessionID}
		d.Sessions[key] = s
	}
	if copyQuit {
		// Another process holding the same conversation quit (an out-of-
		// date copy): the session still runs where the hooks last heard
		// from it, so its record stays as it is.
		d.debugf("%s:%s: a copy in pid %d quit; the session is still open in pid %d", ev.Harness, short(ev.SessionID), ev.PID, s.PID)
		return s
	}
	if s.Ended && ev.Kind != core.KindSessionStart && !ev.At.After(s.EndedAt) {
		// From before it ended, delivered late (hooks run concurrently): it
		// doesn't reopen the session or move it back to its old pane.
		return s
	}
	if ev.Cwd != "" {
		s.Cwd = ev.Cwd
	}
	if ev.TranscriptPath != "" {
		s.TranscriptPath = ev.TranscriptPath
	}
	if ev.Location != nil && ev.Location.Kind != "" {
		s.Location = ev.Location
	}
	if ev.PID > 0 {
		s.PID = ev.PID
	}
	if ev.Entrypoint != "" {
		s.Entrypoint = ev.Entrypoint
	}
	if ev.Model != "" {
		s.Model = ev.Model
	}
	if ev.TurnStart && ev.At.After(s.TurnAt) {
		s.TurnAt = ev.At
	}
	switch {
	case ev.Kind == core.KindSessionEnd:
		s.Ended, s.EndedAt = true, ev.At
	case ev.Kind == core.KindSessionStart, ev.PID > 0:
		// A start, or anything its process reports: it runs.
		s.Ended = false
	}
	if ev.At.After(s.LastSeen) {
		s.LastSeen = ev.At
	}
	// Keep the tracked wrapper instance's active conversation pointer in sync
	// (an instance can host multiple conversations over its lifetime).
	if ev.Location != nil && ev.Location.Kind == "wrapper" && ev.Location.Target != "" &&
		!DriverFor(ev.Harness).IgnoreTranscript(ev.TranscriptPath) {
		if inst := d.Instances[ev.Location.Target]; inst != nil {
			if ev.Kind == core.KindSessionEnd {
				if inst.ActiveSessionID == ev.SessionID {
					inst.ActiveSessionID = ""
				}
			} else {
				inst.ActiveSessionID = ev.SessionID
			}
			if ev.Cwd != "" {
				inst.Cwd = ev.Cwd
			}
			inst.UpdatedAt = d.Now()
		}
	}
	d.dirty = true
	return s
}

func jobKey(ev core.Event) string {
	if ev.SessionID != "" {
		return ev.Harness + ":" + ev.SessionID
	}
	if ev.Location != nil {
		return ev.Harness + "@" + ev.Location.String()
	}
	return ""
}

func (d *Daemon) jobFor(ev core.Event) *core.Job {
	if k := jobKey(ev); k != "" {
		return d.Jobs[k]
	}
	return nil
}

// resumeTakeWindow: how long after a resume a failed turn is the resumed
// one failing.
const resumeTakeWindow = 15 * time.Minute

// turnFailed: a turn ended on an error that schedules nothing. Right after a
// resume of that session, the resume didn't take: say so instead of leaving
// it reported as resumed.
func (d *Daemon) turnFailed(ev core.Event) {
	j := d.jobFor(ev)
	if j == nil || j.State != core.StateDelivered || j.TurnFailed != "" || j.ResumeSettled ||
		!ev.At.After(resumeStart(j)) || ev.At.Sub(resumeStart(j)) > resumeTakeWindow {
		return
	}
	errText := strings.TrimRight(firstLine(ev.Message), ". ")
	if errText == "" {
		errText = "an error"
	}
	markJobTurnFailed(j, errText, ev.At, d.Now())
	d.dirty = true
	d.history("resume_failed", j, map[string]any{"error": j.TurnFailed})
	d.warnf("%s · resumed, but its turn failed: %s", d.describe(j), j.TurnFailed)
	d.notify(j, "Resumed, but it failed", j.TurnFailed+". Continue it yourself.")
}

// resumeStart: when the delivery of j's resume began. Typing can take a few
// seconds to return (a hidden terminal, a launcher), and the resumed turn may
// end, or fail, before it does.
func resumeStart(j *core.Job) time.Time {
	if !j.DeliveringAt.IsZero() && j.DeliveringAt.Before(j.DeliveredAt) {
		return j.DeliveringAt
	}
	return j.DeliveredAt
}

// settleResume: after a resume, the first turn that ends (or a prompt typed
// a while after it, which is the user's own) means the resumed turn is over:
// a failure after that is not the resume failing. A turn that ends normally
// after a failed resume means the user carried on: it no longer needs them.
func (d *Daemon) settleResume(ev core.Event) {
	j := d.jobFor(ev)
	if j == nil || j.State != core.StateDelivered || !ev.At.After(resumeStart(j)) {
		return
	}
	// A turn's end, as its hooks report it (transcript lines arrive mid-turn
	// too, so they don't count).
	hook := strings.HasPrefix(ev.Source, "hook:")
	turnEnd := ev.Kind == core.KindReply || ev.Kind == core.KindTurnFailed || ev.Kind == core.KindActivity && hook && !ev.TurnStart
	laterPrompt := ev.Kind == core.KindActivity && hook && ev.TurnStart && ev.At.Sub(resumeStart(j)) >= time.Minute
	if !j.ResumeSettled && (turnEnd || laterPrompt) {
		markJobSettled(j)
		d.dirty = true
	}
	if j.TurnFailed != "" && ev.Kind != core.KindTurnFailed && turnEnd && ev.At.After(j.TurnFailedAt) {
		clearJobTurnFailed(j)
		d.dirty = true
	}
}

func addSource(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

func (d *Daemon) limitEvent(ev core.Event, sess *store.Session) {
	now := d.Now()
	key := jobKey(ev)
	if ev.At.Before(now.Add(-d.Config.CatchUp())) {
		// Older than the catch-up window: Continuum wasn't running when it
		// happened (stopped, or the computer was off). It still counts if the
		// session is open and has sat on that limit since; later activity
		// (from the spool or the transcript) drops it again.
		if !d.stillOnLimit(ev, sess) {
			d.debugf("%s: ignoring a %s from %s at %s: older than the catch-up window, and the session is closed or has moved on", key, ev.Kind, ev.Source, ev.At.Local().Format("Jan 2 15:04"))
			return
		}
		if d.Jobs[key] == nil && !strings.HasSuffix(ev.Source, "+adopted") {
			d.logf("%s · %s: hit a limit at %s while Continuum wasn't running, and is still waiting on it: scheduling its resume", d.name(ev.Harness), key, ev.At.Local().Format("Jan 2 15:04"))
		}
	}
	if key == "" {
		d.logf("ignoring %s %s event with no session or location", ev.Harness, ev.Kind)
		return
	}
	if d.liftedNotice(ev) {
		d.debugf("%s: the limit notice on screen is the one already lifted early; ignoring it", key)
		return
	}
	if h := d.Rules.Harness(ev.Harness); h.IgnoredEntrypoint(ev.Entrypoint) || sess != nil && h.IgnoredEntrypoint(sess.Entrypoint) {
		d.debugf("%s: a print-mode or SDK run; not resuming it", key)
		return
	}
	if DriverFor(ev.Harness).IgnoreTranscript(ev.TranscriptPath) || (sess != nil && DriverFor(ev.Harness).IgnoreTranscript(sess.TranscriptPath)) {
		d.debugf("%s: ignoring %s from %s: subagent conversation", key, ev.Kind, ev.Source)
		return
	}
	if ev.Source == "wrapper" && ev.Message != "" && d.Rules != nil && d.Rules.Harness(ev.Harness).Classify(ev.Message, d.Rules.Defaults.TailLines) == nil {
		d.debugf("%s: ignoring wrapper event from user input prompt: %q", key, firstLine(ev.Message))
		return
	}
	if ev.Kind == core.KindUsageLimit && ev.ResetAt.IsZero() && ev.Message != "" {
		if t, ok := resettime.Parse(ev.Message, ev.At, d.Local); ok {
			ev.ResetAt = t
		}
	}
	// Where the session runs, as its hooks recorded it: a transcript or
	// rollout line doesn't say, and without it a wrapper's job could be
	// matched to another session in the same folder.
	if sess != nil && !sess.Ended {
		if ev.Location == nil && sess.Location != nil {
			ev.Location = sess.Location
		}
		if ev.Cwd == "" && sess.Cwd != "" {
			ev.Cwd = sess.Cwd
		}
		if ev.PID == 0 && sess.PID > 0 {
			ev.PID = sess.PID
		}
	}
	if ev.Location == nil && ev.SessionID != "" {
		if w, ok := d.boundWrapper(ev.Harness, ev.SessionID); ok {
			ev.Location = &core.Location{Kind: "wrapper", Target: w.ID}
		}
	}
	j := d.Jobs[key]
	if twin := d.findTwin(ev, key); twin != nil {
		if ev.SessionID != "" && twin.SessionID == "" {
			// Re-key the pane/wrapper job under the session it belongs to.
			delete(d.Jobs, twin.Key)
			twin.Key, twin.SessionID, twin.Harness = key, ev.SessionID, ev.Harness
			if old := d.Jobs[key]; old != nil {
				if old.State == core.StatePending || recentlyDelivered(old, ev.At) {
					twin.Attempts = old.Attempts
				}
				if recentlyDelivered(old, ev.At) {
					if staleReset(old, twin.ResetAt) {
						twin.ResetAt, twin.ResetKnown = time.Time{}, false
					}
					if staleReset(old, ev.ResetAt) {
						ev.ResetAt = time.Time{}
						if onlyScreenSources(twin.Sources) && !screenSourced(ev.Source) {
							twin.ResetAt, twin.ResetKnown = time.Time{}, false
						}
					}
				}
			}
			d.Jobs[key] = twin
		}
		d.merge(twin, ev)
		return
	}
	if !d.Config.ClosedSessionsTracked() {
		probe := &core.Job{
			Harness: ev.Harness, SessionID: ev.SessionID, Cwd: ev.Cwd,
			Location: ev.Location, PID: ev.PID, Entrypoint: ev.Entrypoint,
		}
		var st *live.State
		if probe.SessionID != "" && d.LiveFn != nil {
			st = d.LiveFn(probe)
		}
		if d.sessionClosed(probe, sess, st) {
			d.debugf("%s: ignoring %s from %s: session is closed (track_closed_sessions is off)", key, ev.Kind, ev.Source)
			return
		}
	}
	if j == nil && ev.Kind != core.KindNeverResume && ev.Location == nil && ev.PID == 0 && ev.Cwd == "" &&
		d.Rules != nil && len(d.Rules.Harness(ev.Harness).NativeCommands) == 0 {
		// An agent without headless resume commands cannot be resumed from a
		// transcript-only event that has no known wrapper, pane, PID or folder
		// (e.g. a subagent transcript or a session started without hooks).
		d.debugf("%s: ignoring %s from %s with no reachable session route", key, ev.Kind, ev.Source)
		return
	}
	if j != nil {
		switch {
		case j.State == core.StatePending && ev.Source != opencode.LocationKind &&
			ev.Kind != core.KindNeverResume && !ev.At.After(j.LimitAt.Add(2*time.Minute)):
			// The same limit seen by another observer (hook + transcript).
			d.merge(j, ev)
			return
		case (j.State == core.StateDelivered) && !ev.At.After(j.DeliveredAt) &&
			// The agent's own report (hook, transcript, rollout) of a turn
			// that hit a limit while a slow delivery was still returning is
			// new, not an old line; a screen can still show the old notice.
			!(!screenSourced(ev.Source) && ev.At.After(resumeStart(j))):
			d.debugf("%s: ignoring %s from %s at %s (already delivered at %s)", key, ev.Kind, ev.Source, ev.At.Format(time.RFC3339), j.DeliveredAt.Format(time.RFC3339))
			return // an old line about a limit already handled
		case j.State == core.StateCancelled && !ev.At.After(j.UpdatedAt):
			d.debugf("%s: ignoring %s from %s (cancelled at %s)", key, ev.Kind, ev.Source, j.UpdatedAt.Format(time.RFC3339))
			return
		case j.State == core.StateNeedsUser && !ev.At.After(j.LimitAt.Add(2*time.Minute)):
			d.debugf("%s: ignoring %s from %s (already marked needs_user)", key, ev.Kind, ev.Source)
			return
		case j.State == core.StateIgnored && !ev.At.After(j.LimitAt.Add(2*time.Minute)):
			d.merge(j, ev)
			return
		case !ev.At.After(j.LimitAt):
			d.debugf("%s: ignoring older %s from %s (%s <= %s)", key, ev.Kind, ev.Source, ev.At.Format(time.RFC3339), j.LimitAt.Format(time.RFC3339))
			return
		}
	}
	attempts := 0
	stale := false
	if j != nil && j.State == core.StatePending {
		attempts = j.Attempts
		if ev.ResetAt.IsZero() && j.ResetKnown && j.ResetAt.After(ev.At) {
			ev.ResetAt = j.ResetAt
		}
	} else if recentlyDelivered(j, ev.At) {
		attempts = j.Attempts // the resume hit a limit again: keep counting
		// The same reset time as the limit already resumed means either an old
		// message seen again (a redraw) or a reset that has not happened yet.
		// Either way, do not retry straight away.
		stale = staleReset(j, ev.ResetAt)
		if stale {
			d.debugf("%s: reset time %s from %s is not newer than prior delivered reset %s; using retry backoff", key, ev.ResetAt.Format(time.RFC3339), ev.Source, j.ResetAt.Format(time.RFC3339))
		}
	}
	nj := &core.Job{
		Key: key, Harness: ev.Harness, SessionID: ev.SessionID, Cwd: ev.Cwd,
		TranscriptPath: ev.TranscriptPath, TranscriptSize: ev.TranscriptSize,
		Location: ev.Location, PID: ev.PID, Entrypoint: ev.Entrypoint,
		Kind: ev.Kind, Message: ev.Message, ResetAt: ev.ResetAt, ResetKnown: !ev.ResetAt.IsZero(),
		ResetFromScreen: !ev.ResetAt.IsZero() && screenSourced(ev.Source),
		LimitAt:         ev.At, CreatedAt: now, UpdatedAt: now, Attempts: attempts,
		State: core.StatePending, Sources: []string{ev.Source}, Hit: ev.Hit, LimitID: ev.LimitID,
	}
	if nj.Model == "" {
		if s := d.sessionAt(ev.Harness, ev.Location, sess); s != nil {
			nj.Model = s.Model
		}
	}
	if j != nil && j.State == core.StateDelivered && !j.LiftedAt.IsZero() && ev.Kind == core.KindUsageLimit && ev.At.Sub(j.DeliveredAt) < liftCheck && sameLimitOf(j, nj) {
		// Resumed early because the limit looked lifted, and hit the same
		// limit again straight away: it wasn't. Wait for the reset this
		// time: the one it was lifted from, if this report names none.
		nj.LiftFailed = true
		if nj.ResetAt.IsZero() && j.LiftedFrom.After(ev.At) {
			nj.ResetAt, nj.ResetKnown, nj.ResetFromScreen = j.LiftedFrom, true, j.ResetFromScreen
			stale = false
		}
		d.logf("%s: the limit wasn't lifted after all; waiting for its reset", key)
	} else if j != nil && j.LiftFailed && j.ResetKnown && j.ResetAt.After(ev.At) && ev.Kind == core.KindUsageLimit && sameLimitOf(j, nj) {
		// The same limit again before the reset a failed lift is waiting
		// for (the session was continued by hand meanwhile): answers still
		// don't lift it, and it waits for that reset.
		nj.LiftFailed = true
		if nj.ResetAt.IsZero() {
			nj.ResetAt, nj.ResetKnown, nj.ResetFromScreen = j.ResetAt, true, j.ResetFromScreen
		}
	}
	if sess != nil {
		if nj.Location == nil {
			nj.Location = sess.Location
		}
		if nj.PID == 0 {
			nj.PID = sess.PID
		}
		if nj.Cwd == "" {
			nj.Cwd = sess.Cwd
		}
		if nj.Entrypoint == "" {
			nj.Entrypoint = sess.Entrypoint
		}
	}
	d.Jobs[key] = nj
	d.dirty = true

	if ev.Kind == core.KindNeverResume {
		markJobNeedsUser(nj, "this limit needs you (billing, credits, spend cap or sign-in); Continuum will not resume it", false, now)
		d.history("needs_user", nj, map[string]any{"message": ev.Message})
		d.notify(nj, "Needs you", firstLine(ev.Message))
		return
	}
	if d.stopAtMaxAttempts(nj) {
		return
	}
	if stale {
		nj.ResetAt, nj.ResetKnown, nj.ResetFromScreen = time.Time{}, false, false
	}
	if !nj.ResetKnown && ev.Kind == core.KindUsageLimit {
		if t, ok := d.resetHint(ev.Harness); ok && t.After(now) {
			nj.ResetAt, nj.ResetKnown, nj.ResetFromScreen = t, true, false
		}
	}
	if !nj.ResetKnown && !stale && ev.Kind == core.KindUsageLimit {
		if o := d.sameLimit(nj); o != nil {
			nj.ResetAt, nj.ResetKnown, nj.ResetFromScreen = o.ResetAt, true, o.ResetFromScreen
			d.debugf("%s: no reset time of its own; using %s from %s, which hit the same limit", key, o.ResetAt.Local().Format("15:04"), o.Key)
		}
	}
	if nj.ResetKnown && ev.Kind == core.KindUsageLimit {
		d.shareReset(nj)
	}
	d.reschedule(nj)
	d.liftIfAnswered(nj)
	if nj.State == core.StatePending {
		if ok, why := d.Scope.Covers(nj); !ok {
			markJobIgnored(nj, why, now)
			d.history("ignored", nj, map[string]any{"reason": why})
			d.logf("·  %s · %s, not resuming: %s", d.describe(nj), strings.ReplaceAll(nj.Kind, "_", " "), why)
			return
		}
		histExtra := map[string]any{
			"due":         nj.DueAt.Format(time.RFC3339),
			"kind":        nj.Kind,
			"source":      ev.Source,
			"reset_known": nj.ResetKnown,
			"attempt":     nj.Attempts + 1,
		}
		if nj.Hit != "" {
			histExtra["hit"] = nj.Hit
		}
		if nj.ResetKnown {
			histExtra["reset_at"] = nj.ResetAt.Format(time.RFC3339)
		}
		d.history("scheduled", nj, histExtra)
		if nj.ResetKnown {
			d.logf("⏸  %s · %s · reset %s · resumes %s (from %s)", d.describe(nj), strings.ReplaceAll(nj.Kind, "_", " "), nj.ResetAt.In(d.Local).Format("15:04"), when(nj.DueAt, d.Now()), ev.Source)
		} else {
			d.logf("⏸  %s · %s · reset time unknown (backoff #%d) · resumes %s (from %s, hit %q)", d.describe(nj), strings.ReplaceAll(nj.Kind, "_", " "), nj.Attempts+1, when(nj.DueAt, d.Now()), ev.Source, nj.Hit)
		}
		d.debugf("%s: scheduled from %s (hit=%q reset_known=%v message=%q)", nj.Key, ev.Source, nj.Hit, nj.ResetKnown, firstLine(nj.Message))
		if nj.Kind == core.KindUsageLimit {
			d.notify(nj, "Paused until reset", "Will continue at "+nj.DueAt.In(d.Local).Format("Mon 3:04 PM"))
		}
	}
}

// findTwin finds a pending job for the same stall reported under another key:
// a hook or transcript knows the session id, while the wrapper or a pane scan
// knows only where the agent runs. Matching on location (or directory) and
// time keeps one stall to one resume.
func (d *Daemon) findTwin(ev core.Event, key string) *core.Job {
	for k, j := range d.Jobs {
		if k == key || j.State != core.StatePending {
			continue
		}
		sameHarness := j.Harness == ev.Harness
		// The wrapper and pane scanner may only guess "generic"; the hook knows better.
		if !sameHarness && j.Harness != "generic" && ev.Harness != "generic" {
			continue
		}
		if ev.SessionID != "" && j.SessionID != "" {
			continue // two different sessions
		}
		if d.endedSession(j.Harness, j.SessionID) && screenSourced(ev.Source) ||
			d.endedSession(ev.Harness, ev.SessionID) && screenSourced(j.Sources...) {
			// A session that has ended isn't what a pane's screen shows now:
			// another conversation in that folder hit a limit.
			continue
		}
		bothLoc := ev.Location != nil && j.Location != nil
		sameLoc := bothLoc && ev.Location.String() == j.Location.String()
		if bothLoc && !sameLoc {
			continue // two different panes
		}
		dt := ev.At.Sub(j.LimitAt)
		if dt < -3*time.Minute || dt > 3*time.Minute {
			// A redraw on the same wrapper while its session is still waiting on
			// an authoritative limit belongs to that pending stall, not a new one.
			samePendingWrapper := sameLoc && ev.Location.Kind == "wrapper" &&
				ev.SessionID == "" && screenSourced(ev.Source) &&
				j.SessionID != "" && !onlyScreenSources(j.Sources) &&
				ev.At.After(j.LimitAt) && j.Kind == ev.Kind &&
				limitNotice(d.Rules.Harness(j.Harness), j.Message) == limitNotice(d.Rules.Harness(ev.Harness), ev.Message) && ev.Message != ""
			if samePendingWrapper {
				if inst := d.Instances[ev.Location.Target]; inst != nil && inst.ActiveSessionID != "" && inst.ActiveSessionID != j.SessionID {
					samePendingWrapper = false
				}
			}
			if !samePendingWrapper {
				continue
			}
		}
		sameDir := ev.Cwd != "" && j.Cwd != "" && filepath.Clean(ev.Cwd) == filepath.Clean(j.Cwd)
		if sameLoc || (sameDir && sameHarness) {
			return j
		}
	}
	return nil
}

func limitNotice(h *rules.Harness, message string) string {
	if m := h.Classify(message, strings.Count(message, "\n")+1); m != nil {
		if _, after, ok := strings.Cut(message, m.Line); ok {
			message = m.Line + after
		}
	}
	return strings.Join(strings.Fields(message), " ")
}

func (d *Daemon) merge(j *core.Job, ev core.Event) {
	wasScreenOnly := onlyScreenSources(j.Sources)
	evAuthoritative := !screenSourced(ev.Source)
	j.Sources = addSource(j.Sources, ev.Source)
	if j.Harness == "generic" && ev.Harness != "generic" {
		j.Harness = ev.Harness
	}
	if ev.Location != nil && ev.Location.Kind != "" && (j.Location == nil || ev.Location.Kind == "wrapper") {
		j.Location = ev.Location
	}
	if ev.PID > 0 {
		j.PID = ev.PID
	}
	if j.Cwd == "" {
		j.Cwd = ev.Cwd
	}
	if j.TranscriptPath == "" {
		j.TranscriptPath = ev.TranscriptPath
	}
	if ev.TranscriptSize > j.TranscriptSize && ev.TranscriptPath == j.TranscriptPath {
		j.TranscriptSize = ev.TranscriptSize
	}
	if ev.Entrypoint != "" {
		j.Entrypoint = ev.Entrypoint
	}
	if ev.Kind == core.KindUsageLimit && j.Kind != core.KindUsageLimit {
		j.Kind = core.KindUsageLimit
	}
	// When an authoritative source (hook, transcript, rollout) adopts a
	// screen-only twin, trust its timing and message over the screen scrape
	// (which may have parsed a stale relative countdown from a redraw).
	if wasScreenOnly && evAuthoritative && !ev.At.IsZero() {
		j.LimitAt = ev.At
	}
	// A report of the stall read after its limit was lifted early names the
	// reset it was lifted from: it doesn't put the resume back.
	resetUpgraded := false
	resetFromScreen := j.ResetFromScreen || wasScreenOnly
	if !ev.ResetAt.IsZero() && (j.LiftedAt.IsZero() || ev.At.After(j.LiftedAt)) &&
		(!j.ResetKnown || evAuthoritative && resetFromScreen ||
			(evAuthoritative || resetFromScreen) && ev.ResetAt.After(j.ResetAt)) {
		j.ResetAt, j.ResetKnown, j.ResetFromScreen = ev.ResetAt, true, !evAuthoritative
		resetUpgraded = true
	} else if j.ResetKnown && wasScreenOnly {
		j.ResetFromScreen = true
	}
	if ev.Message != "" && (j.Message == "" || resetUpgraded || (wasScreenOnly && evAuthoritative)) {
		j.Message = ev.Message
	}
	if j.LimitID == "" {
		j.LimitID = ev.LimitID
	}
	if j.Model == "" {
		j.Model = ev.Model
	}
	j.UpdatedAt = d.Now()
	d.dirty = true
	if j.State == core.StatePending && d.stopAtMaxAttempts(j) {
		return
	}
	if resetUpgraded || j.DueAt.IsZero() || (wasScreenOnly && evAuthoritative) ||
		(j.Attempts == 0 && !j.WaitingOpen && j.StaleCopyPID == 0) {
		d.reschedule(j)
	}
	d.debugf("%s: merged %s from %s (reset_known=%v due=%s)", j.Key, ev.Kind, ev.Source, j.ResetKnown, j.DueAt.Format(time.RFC3339))
}

func recentlyDelivered(j *core.Job, at time.Time) bool {
	return j != nil && j.State == core.StateDelivered && at.Sub(j.DeliveredAt) < 6*time.Hour
}

func staleReset(prev *core.Job, resetAt time.Time) bool {
	return !resetAt.IsZero() && prev != nil && prev.ResetKnown && !resetAt.After(prev.ResetAt)
}

func (d *Daemon) stopAtMaxAttempts(j *core.Job) bool {
	if j.Attempts < d.Rules.Defaults.MaxAttempts {
		return false
	}
	markJobNeedsUser(j, fmt.Sprintf("stopped after %d attempts", j.Attempts), false, d.Now())
	d.history("needs_user", j, nil)
	d.notify(j, "Stopped retrying", j.Result)
	d.dirty = true
	return true
}

func backoff(list []int, attempt int) time.Duration {
	if len(list) == 0 {
		return 15 * time.Minute
	}
	if attempt >= len(list) {
		attempt = len(list) - 1
	}
	return time.Duration(list[attempt]) * time.Second
}

func (d *Daemon) delay() time.Duration {
	if d.Config.DelayAfterReset != nil {
		return time.Duration(*d.Config.DelayAfterReset) * time.Second
	}
	return time.Duration(d.Rules.Defaults.DelayAfterResetSeconds) * time.Second
}

func (d *Daemon) grace(j *core.Job) time.Duration {
	if j.Kind != core.KindUsageLimit || j.NativeGaveUp {
		return 0
	}
	h := d.Rules.Harness(j.Harness)
	g := h.Grace(d.Rules.Defaults)
	if v, ok := d.Config.NativeGrace[j.Harness]; ok {
		g = v
	}
	if g <= 0 {
		return 0
	}
	// The agent can only resume by itself while its process is alive.
	if j.PID > 0 && !proc.Alive(j.PID) {
		return 0
	}
	return time.Duration(g) * time.Second
}

// reschedule computes DueAt from the reset time, attempt count and grace.
func (d *Daemon) reschedule(j *core.Job) {
	now := d.Now()
	switch {
	case j.ResetKnown:
		j.DueAt = j.ResetAt.Add(max(d.delay(), d.grace(j)))
	case j.Kind == core.KindTransient:
		j.DueAt = j.LimitAt.Add(backoff(d.Rules.Defaults.TransientBackoffSeconds, j.Attempts))
	default:
		j.DueAt = j.LimitAt.Add(backoff(d.Rules.Defaults.UnknownResetBackoffSeconds, j.Attempts))
	}
	if j.DueAt.Sub(now) > d.Config.MaxWait() {
		markJobNeedsUser(j, fmt.Sprintf("reset is %s away, beyond max_wait_hours", j.DueAt.Sub(now).Round(time.Hour)), false, now)
	}
}

// sameAccount: two jobs of one agent whose transcripts live under the same
// config folder, so their usage limits are the same account's. A job without
// a transcript (read off the screen) can't tell, and counts as the same.
func sameAccount(a, b *core.Job) bool {
	if a.Harness != b.Harness {
		return false
	}
	// An OpenCode server can host several providers and credentials. Its
	// final outcome does not identify the account that reported the limit.
	if DriverFor(a.Harness).ManagesOwnLifecycle(a) || DriverFor(b.Harness).ManagesOwnLifecycle(b) {
		return false
	}
	if a.TranscriptPath == "" || b.TranscriptPath == "" {
		return true
	}
	return accountRoot(a.TranscriptPath) == accountRoot(b.TranscriptPath)
}

// limitWindow: how long before a known reset another stall of the same
// account is taken to be the same limit (Claude's session limit lasts 5 hours).
const limitWindow = 5 * time.Hour

// sameLimit finds the latest stall of j's account whose reset is known and
// still ahead of j's: a limit is the account's, so j waits for that reset too.
// Claude Code's grace stop, for one, ends a turn on a limit without saying
// when it resets.
func (d *Daemon) sameLimit(j *core.Job) *core.Job {
	var best *core.Job
	for _, o := range d.Jobs {
		if o == j || o.Kind != core.KindUsageLimit || !o.ResetKnown || !sameAccount(o, j) {
			continue
		}
		if !o.ResetAt.After(j.LimitAt) || o.LimitAt.After(j.LimitAt.Add(2*time.Minute)) || o.LimitAt.Before(j.LimitAt.Add(-limitWindow)) {
			continue
		}
		if best == nil || o.LimitAt.After(best.LimitAt) {
			best = o
		}
	}
	return best
}

// shareReset gives j's reset time to the same account's pending stalls that
// hit the limit shortly before j without learning when it resets.
func (d *Daemon) shareReset(j *core.Job) {
	for _, o := range d.Jobs {
		if o == j || o.State != core.StatePending || o.Kind != core.KindUsageLimit || o.ResetKnown || !sameAccount(o, j) {
			continue
		}
		if o.LimitAt.After(j.LimitAt.Add(2*time.Minute)) || o.LimitAt.Before(j.LimitAt.Add(-limitWindow)) || !j.ResetAt.After(o.LimitAt) {
			continue
		}
		o.ResetAt, o.ResetKnown, o.ResetFromScreen = j.ResetAt, true, j.ResetFromScreen
		o.UpdatedAt = d.Now()
		d.reschedule(o)
		d.dirty = true
		d.logf("⏸  %s · reset %s, learned from %s · resumes %s", d.describe(o), o.ResetAt.In(d.Local).Format("15:04"), d.describe(j), when(o.DueAt, d.Now()))
	}
}

// resetHint asks an external command (for example a quota tracker) when the
// harness's limit resets. Expected output: {"claude": "<RFC3339 or epoch>", ...}.
func (d *Daemon) resetHint(harness string) (time.Time, bool) {
	if len(d.Config.ResetHintsCommand) == 0 {
		return time.Time{}, false
	}
	out, err := proc.Run(15*time.Second, "", d.Config.ResetHintsCommand...)
	if err != nil {
		d.logf("reset hints command: %v", err)
		return time.Time{}, false
	}
	var m map[string]any
	if json.Unmarshal([]byte(out), &m) != nil {
		return time.Time{}, false
	}
	switch v := m[harness].(type) {
	case string:
		if t, err := time.Parse(time.RFC3339, v); err == nil && t.After(d.Now()) {
			return t, true
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Unix(n, 0), true
		}
	case float64:
		return time.Unix(int64(v), 0), true
	}
	return time.Time{}, false
}
