package daemon

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/store"
)

// Limits lifted early.
//
// A usage limit can come back before the reset it announced: the user spends
// a banked limit reset (claude.ai Settings > Usage, the Desktop app, or
// Claude Code's /limit-reset). The waiting sessions would otherwise sit until
// the original reset, days away for a weekly limit. Two things show the
// account's limits are back:
//
//   - Claude Code reports a limit reset used in one of its sessions;
//   - another session of the same account gets an answer from the model, on
//     the plan (not usage credits), to a request sent after the limit hit.
//
// Either moves the account's waiting resumes up to now (plus the usual delay
// and the agent's own grace). If the limit wasn't lifted after all, the
// resume hits it again and waits for the reset that message names.

// liftMargin: an answer counts only for a request sent at least this long
// after the limit hit, so a request already under way when it hit doesn't.
const liftMargin = 30 * time.Second

// modelLimitRe finds a limit on one family of models ("You've hit your Fable
// limit"): an answer from another model doesn't lift it.
var modelLimitRe = regexp.MustCompile(`(?i)\b(fable|opus|sonnet|haiku)\b(?:\s+[\w.]+){0,2}?\s+limit\b`)

// limitFamily returns the model family a limit message is about, or "" for
// a limit on every model.
func limitFamily(msg string) string {
	if m := modelLimitRe.FindStringSubmatch(msg); m != nil {
		return strings.ToLower(m[1])
	}
	return ""
}

// accountKey names the account a transcript belongs to: its agent and config
// folder (see sameAccount). "" when there is no transcript to tell by.
func accountKey(harness, transcript string) string {
	if transcript == "" {
		return ""
	}
	return harness + "|" + filepath.Dir(filepath.Dir(filepath.Clean(transcript)))
}

// liftedFor reports whether ev shows that j's limit is lifted, and how.
func liftedFor(j *core.Job, ev core.Event) (string, bool) {
	if j.State != core.StatePending || j.Kind != core.KindUsageLimit {
		return "", false
	}
	if accountKey(j.Harness, j.TranscriptPath) != accountKey(ev.Harness, ev.TranscriptPath) || accountKey(ev.Harness, ev.TranscriptPath) == "" {
		return "", false
	}
	switch {
	case ev.Kind == core.KindLimitReset:
		if !ev.At.After(j.LimitAt) {
			return "", false
		}
		why := "a limit reset was used"
		if ev.SessionID != j.SessionID {
			why += " in session " + short(ev.SessionID)
		}
		return why, true
	case !ev.RequestAt.IsZero():
		if ev.SessionID == j.SessionID || !ev.RequestAt.After(j.LimitAt.Add(liftMargin)) {
			return "", false
		}
		if f := limitFamily(j.Message); f != "" && !strings.Contains(strings.ToLower(ev.Model), f) {
			return "", false
		}
		return fmt.Sprintf("session %s, on the same account, got an answer from %s", short(ev.SessionID), ev.Model), true
	}
	return "", false
}

// limitLifted moves up the resumes that ev shows are no longer limited.
func (d *Daemon) limitLifted(ev core.Event) {
	if !ev.RequestAt.IsZero() {
		if d.answered == nil {
			d.answered = map[string]core.Event{}
		}
		if k := accountKey(ev.Harness, ev.TranscriptPath); k != "" && ev.RequestAt.After(d.answered[k].RequestAt) {
			d.answered[k] = ev // for limits reported after this answer is read
		}
	}
	for _, j := range store.SortedJobs(d.Jobs) {
		if why, ok := liftedFor(j, ev); ok {
			d.lift(j, ev.At, why, false)
		}
	}
}

// liftIfAnswered: a new stall whose account was already seen answering a
// request sent after it (read before the stall itself) is lifted already.
func (d *Daemon) liftIfAnswered(j *core.Job) {
	ev, ok := d.answered[accountKey(j.Harness, j.TranscriptPath)]
	if !ok {
		return
	}
	if why, ok := liftedFor(j, ev); ok {
		d.lift(j, ev.At, why, true)
	}
}

// lift makes j due as if its limit had reset at `at`, if that is sooner.
func (d *Daemon) lift(j *core.Job, at time.Time, why string, quiet bool) {
	if !j.LiftedAt.IsZero() && !at.After(j.LiftedAt) {
		return
	}
	resetAt, known, due, state, result := j.ResetAt, j.ResetKnown, j.DueAt, j.State, j.Result
	j.ResetAt, j.ResetKnown = at, true
	d.reschedule(j)
	if j.State != core.StatePending || !j.DueAt.Before(due.Add(-time.Minute)) {
		j.ResetAt, j.ResetKnown, j.DueAt, j.State, j.Result = resetAt, known, due, state, result
		return // no sooner than it already was
	}
	if j.LiftedFrom.IsZero() && known {
		j.LiftedFrom = resetAt
	}
	j.LiftedAt = at
	j.UpdatedAt = d.Now()
	d.dirty = true
	d.history("lifted", j, map[string]any{"reason": why, "due": j.DueAt.Format(time.RFC3339), "was_due": due.Format(time.RFC3339)})
	if quiet {
		d.debugf("%s: the limit is back early: %s; resumes %s instead of %s", j.Key, why, when(j.DueAt, d.Now()), when(due, d.Now()))
		return
	}
	d.logf("⏩ %s · the limit is back early (%s; a banked reset?) · resumes %s instead of %s", d.describe(j), why, when(j.DueAt, d.Now()), when(due, d.Now()))
	d.notify(j, "Limit back early", "Will continue at "+j.DueAt.In(d.Local).Format("Mon 3:04 PM"))
}

// liftedNotice: a limit read off the screen of a session whose limit was
// lifted early, naming the reset it was lifted from (or none). It's the old
// notice, still on screen or redrawn, not a new limit; a real new one is
// reported by the transcript and hooks too.
func (d *Daemon) liftedNotice(ev core.Event) bool {
	if !screenSourced(ev.Source) || ev.Kind != core.KindUsageLimit || ev.Location == nil {
		return false
	}
	for _, j := range d.Jobs {
		if j.LiftedAt.IsZero() || j.Location == nil || j.Location.String() != ev.Location.String() {
			continue
		}
		if j.State != core.StatePending && !recentlyDelivered(j, ev.At) {
			continue
		}
		if ev.ResetAt.IsZero() || !j.LiftedFrom.IsZero() && !ev.ResetAt.After(j.LiftedFrom.Add(time.Minute)) {
			return true
		}
	}
	return false
}
