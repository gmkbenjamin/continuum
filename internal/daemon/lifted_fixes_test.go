package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/scope"
)

// hookTurn: session s of harness, in a terminal, starts a turn at start
// (its prompt-submit hook) and its Stop hook reports it answered at end.
func hookTurn(h *harnessT, harness, s, model string, start, end time.Time) []core.Event {
	loc := &core.Location{Kind: "wrapper", Target: "w-" + s}
	return []core.Event{
		{Harness: harness, Kind: core.KindActivity, SessionID: s, Source: "hook:UserPromptSubmit", At: start, TurnStart: true, Model: model, Location: loc},
		{Harness: harness, Kind: core.KindActivity, SessionID: s, Source: "hook:Stop", At: end, Answered: true, Model: model, Location: loc},
	}
}

func (h *harnessT) handleAll(evs []core.Event) {
	for _, ev := range evs {
		h.now = ev.At
		h.d.Handle(ev)
	}
}

func grokLimit(h *harnessT, s string, resetIn time.Duration) core.Event {
	return core.Event{Harness: "grok", Kind: core.KindUsageLimit, SessionID: s, Cwd: "/work/app", Source: "hook:StopFailure",
		At: h.now, ResetAt: h.now.Add(resetIn), PID: deadPID, Location: &core.Location{Kind: "wrapper", Target: "w-" + s}}
}

// A turn whose request went out before the limit hit, however long it took
// to answer, doesn't show the limit lifted; a turn started after it does.
func TestHookAnswerCountsOnlyForATurnStartedAfterTheLimit(t *testing.T) {
	h := setup(t)
	limitAt := h.now
	h.d.Handle(grokLimit(h, "g1", 72*time.Hour))
	j := h.d.Jobs["grok:g1"]
	due := j.DueAt
	// Started a minute before the limit, streamed for 12 minutes.
	h.handleAll(hookTurn(h, "grok", "g2", "", limitAt.Add(-time.Minute), limitAt.Add(11*time.Minute)))
	if !j.DueAt.Equal(due) || !j.LiftedAt.IsZero() {
		t.Fatalf("a turn started before the limit lifted it: due %v", j.DueAt)
	}
	h.handleAll(hookTurn(h, "grok", "g2", "", limitAt.Add(20*time.Minute), limitAt.Add(21*time.Minute)))
	if j.LiftedAt.IsZero() || j.DueAt.After(h.now.Add(time.Minute)) {
		t.Fatalf("a turn started after the limit didn't lift it: %+v", j)
	}
}

// Without a known turn start, or from a session not running in a terminal
// (Cursor's editor runs the CLI's hooks too), an answer counts for nothing.
func TestHookAnswerWithoutTurnOrTerminal(t *testing.T) {
	h := setup(t)
	h.d.Handle(grokLimit(h, "g1", 72*time.Hour))
	j := h.d.Jobs["grok:g1"]
	due := j.DueAt
	h.now = h.now.Add(time.Hour)
	h.d.Handle(core.Event{Harness: "grok", Kind: core.KindActivity, SessionID: "g2", Source: "hook:Stop", At: h.now, Answered: true,
		Location: &core.Location{Kind: "wrapper", Target: "w-g2"}})
	editor := hookTurn(h, "grok", "e1", "", h.now.Add(time.Minute), h.now.Add(2*time.Minute))
	for i := range editor {
		editor[i].Location = nil
	}
	h.handleAll(editor)
	if !j.DueAt.Equal(due) || !j.LiftedAt.IsZero() {
		t.Fatalf("lifted: due %v", j.DueAt)
	}
}

// Cursor: an answer from another model (Auto after a limit on the premium
// models) doesn't lift the limit; one from the same model does.
func TestCursorAnswerFromAnotherModel(t *testing.T) {
	h := setup(t)
	h.handleAll(hookTurn(h, "cursor", "c1", "claude-fable-5-2", h.now.Add(-2*time.Minute), h.now.Add(-time.Minute)))
	h.now = h.now.Add(time.Minute)
	// Cursor's limits only show on screen: no session id.
	h.d.Handle(core.Event{Harness: "cursor", Kind: core.KindUsageLimit, Source: "wrapper", Hit: "You've hit your usage limit",
		At: h.now, ResetAt: h.now.Add(72 * time.Hour), Location: &core.Location{Kind: "wrapper", Target: "w-c1"}, Message: "You've hit your usage limit"})
	j := h.d.Jobs["cursor@wrapper:w-c1"]
	if j == nil || j.Model != "claude-fable-5-2" {
		t.Fatalf("job %+v", j)
	}
	due := j.DueAt
	h.handleAll(hookTurn(h, "cursor", "c2", "auto", h.now.Add(time.Hour), h.now.Add(61*time.Minute)))
	if !j.DueAt.Equal(due) {
		t.Fatalf("an answer from Auto lifted a limit hit on another model: due %v", j.DueAt)
	}
	h.handleAll(hookTurn(h, "cursor", "c2", "claude-fable-5-2", h.now.Add(time.Minute), h.now.Add(2*time.Minute)))
	if j.LiftedAt.IsZero() {
		t.Fatalf("an answer from the same model didn't lift it")
	}
}

// A limit read off the screen has no transcript; the hook answer that lifts
// it carries one. They are still the same account.
func TestScreenLimitLiftedByHookAnswerWithTranscript(t *testing.T) {
	h := setup(t)
	h.d.Handle(core.Event{Harness: "cursor", Kind: core.KindUsageLimit, Source: "wrapper", At: h.now, ResetAt: h.now.Add(72 * time.Hour),
		Location: &core.Location{Kind: "wrapper", Target: "w-x"}, Message: "You've hit your usage limit", Hit: "You've hit your usage limit"})
	var j *core.Job
	for _, o := range h.d.Jobs {
		j = o
	}
	if j == nil || j.TranscriptPath != "" {
		t.Fatalf("job %+v", j)
	}
	turn := hookTurn(h, "cursor", "c2", "", h.now.Add(time.Hour), h.now.Add(61*time.Minute))
	for i := range turn {
		turn[i].TranscriptPath = filepath.Join(h.claude, "..", "cursor", "projects", "w", "agent-transcripts", "c2.jsonl")
	}
	h.handleAll(turn)
	if j.LiftedAt.IsZero() {
		t.Fatalf("not lifted: %+v", j)
	}
}

// liftedThenFailed: g1 hit a limit resetting in 72h, was lifted by g2's
// answer and resumed, and hit the limit again straight away, with no reset
// time this time.
func liftedThenFailed(t *testing.T, h *harnessT) (core.Event, *core.Job) {
	t.Helper()
	limit := grokLimit(h, "g1", 72*time.Hour)
	h.d.Handle(limit)
	h.handleAll(hookTurn(h, "grok", "g2", "", h.now.Add(time.Hour), h.now.Add(61*time.Minute)))
	h.advance(10 * time.Minute)
	if len(h.delivered) != 1 {
		t.Fatalf("not resumed early: %d", len(h.delivered))
	}
	again := grokLimit(h, "g1", 0)
	again.ResetAt = time.Time{}
	again.At = h.now.Add(time.Minute)
	h.now = again.At
	h.d.Handle(again)
	return limit, h.d.Jobs["grok:g1"]
}

// The failed early resume waits for the reset it was lifted from, rather
// than retrying on backoff until it gives up.
func TestFailedLiftWaitsForTheResetItWasLiftedFrom(t *testing.T) {
	h := setup(t)
	limit, j := liftedThenFailed(t, h)
	if !j.LiftFailed || j.State != core.StatePending || !j.ResetKnown || !j.ResetAt.Equal(limit.ResetAt) || j.DueAt.Before(limit.ResetAt) {
		t.Fatalf("want a wait for %v: %+v", limit.ResetAt, j)
	}
}

// Once the failed stall is continued by hand, answers still don't lift the
// account's other stalls on the same limit before its reset.
func TestFailedLiftOutlivesAContinuedSession(t *testing.T) {
	h := setup(t)
	limit, j := liftedThenFailed(t, h)
	h.d.Handle(grokLimit(h, "g3", 72*time.Hour))
	other := h.d.Jobs["grok:g3"]
	due := other.DueAt
	// The user continues g1 by hand (a turn that ends normally).
	h.handleAll(hookTurn(h, "grok", "g1", "", h.now.Add(time.Minute), h.now.Add(2*time.Minute)))
	if j.State != core.StateSkipped {
		t.Fatalf("g1 not continued: %s", j.State)
	}
	h.handleAll(hookTurn(h, "grok", "g2", "", h.now.Add(time.Minute), h.now.Add(2*time.Minute)))
	if !other.DueAt.Equal(due) || !other.LiftedAt.IsZero() {
		t.Fatalf("lifted before %v: due %v", limit.ResetAt, other.DueAt)
	}
}

// A failed lift of one limit doesn't stop answers lifting another.
func TestFailedLiftOnlyBlocksTheSameLimit(t *testing.T) {
	h := setup(t)
	fable := weeklyLimit(h, "s1", "You've hit your Fable limit · resets Nov 14, 9am")
	h.d.Handle(fable)
	h.now = h.now.Add(time.Hour)
	h.d.Handle(answer(h, "s2", "claude-fable-5-2", h.now.Add(-time.Minute)))
	h.advance(2 * time.Minute)
	again := weeklyLimit(h, "s1", fable.Message)
	again.At, again.ResetAt = h.now.Add(time.Minute), fable.ResetAt
	h.now = again.At
	h.d.Handle(again)
	if !h.d.Jobs["claude:s1"].LiftFailed {
		t.Fatalf("not marked: %+v", h.d.Jobs["claude:s1"])
	}
	h.d.Handle(weeklyLimit(h, "s3", "You've hit your weekly limit · resets Nov 14, 9am"))
	weekly := h.d.Jobs["claude:s3"]
	h.now = h.now.Add(time.Hour)
	h.d.Handle(answer(h, "s2", "claude-opus-5-5", h.now.Add(-time.Minute)))
	if weekly.LiftedAt.IsZero() {
		t.Fatalf("a failed lift of the Fable limit blocked the weekly one: %+v", weekly)
	}
	if s1 := h.d.Jobs["claude:s1"]; !s1.ResetAt.Equal(fable.ResetAt) {
		t.Fatalf("the Fable limit was lifted again: %+v", s1)
	}
}

// Cursor: an early resume that hits the limit again shows only on screen.
// A turn started at that place after the resume makes it a new hit, not the
// old notice redrawn: it waits for the reset it was lifted from.
func TestScreenOnlyReHitAfterAnEarlyResume(t *testing.T) {
	h := setup(t)
	loc := &core.Location{Kind: "wrapper", Target: "w-c1"}
	h.handleAll(hookTurn(h, "cursor", "c1", "", h.now.Add(-2*time.Minute), h.now.Add(-time.Minute)))
	h.now = h.now.Add(time.Minute)
	notice := core.Event{Harness: "cursor", Kind: core.KindUsageLimit, Source: "wrapper", Hit: "You've hit your usage limit",
		At: h.now, ResetAt: h.now.Add(72 * time.Hour), Location: loc, Message: "You've hit your usage limit"}
	h.d.Handle(notice)
	h.handleAll(hookTurn(h, "cursor", "c2", "", h.now.Add(time.Hour), h.now.Add(61*time.Minute)))
	h.advance(10 * time.Minute)
	j := h.d.Jobs["cursor@wrapper:w-c1"]
	if len(h.delivered) != 1 || j.State != core.StateDelivered {
		t.Fatalf("not resumed early: %d %s", len(h.delivered), j.State)
	}
	// The old notice redrawn before any new turn is still ignored.
	redraw := notice
	redraw.At, redraw.ResetAt = h.now, time.Time{}
	h.d.Handle(redraw)
	if h.d.Jobs["cursor@wrapper:w-c1"].State != core.StateDelivered {
		t.Fatalf("the old notice was taken for a new limit")
	}
	// The resume's prompt starts a turn, which hits the limit again.
	h.now = h.now.Add(time.Second)
	h.d.Handle(core.Event{Harness: "cursor", Kind: core.KindActivity, SessionID: "c1", Source: "hook:beforeSubmitPrompt", At: h.now, TurnStart: true, Location: loc})
	again := notice
	again.At, again.ResetAt = h.now.Add(20*time.Second), time.Time{}
	h.now = again.At
	h.d.Handle(again)
	nj := h.d.Jobs["cursor@wrapper:w-c1"]
	if nj.State != core.StatePending || !nj.LiftFailed || !nj.ResetAt.Equal(notice.ResetAt) {
		t.Fatalf("want a wait for %v: %+v", notice.ResetAt, nj)
	}
}

// A prompt submitted in a waiting session (/usage, /model) doesn't drop its
// resume: no turn ran.
func TestPromptSubmitDoesntDropAResume(t *testing.T) {
	h := setup(t)
	h.d.Handle(grokLimit(h, "g1", time.Hour))
	h.now = h.now.Add(time.Minute)
	h.d.Handle(core.Event{Harness: "grok", Kind: core.KindActivity, SessionID: "g1", Source: "hook:UserPromptSubmit", At: h.now, TurnStart: true})
	if j := h.d.Jobs["grok:g1"]; j.State != core.StatePending {
		t.Fatalf("dropped: %s %s", j.State, j.Result)
	}
}

// An early resume that hits a different limit (another model family's) isn't
// a failed lift: the lifted limit may well be back.
func TestEarlyResumeHittingAnotherLimitIsNotAFailedLift(t *testing.T) {
	h := setup(t)
	h.d.Handle(weeklyLimit(h, "s1", "You've hit your Fable limit · resets Nov 14, 9am"))
	h.now = h.now.Add(time.Hour)
	h.d.Handle(answer(h, "s2", "claude-fable-5-2", h.now.Add(-time.Minute)))
	h.advance(2 * time.Minute)
	if len(h.delivered) != 1 {
		t.Fatalf("not resumed early")
	}
	again := weeklyLimit(h, "s1", "You've hit your weekly limit · resets Nov 15, 9am")
	again.At = h.now.Add(time.Minute)
	h.now = again.At
	h.d.Handle(again)
	if j := h.d.Jobs["claude:s1"]; j.LiftFailed {
		t.Fatalf("marked lift-failed on another limit: %+v", j)
	}
}

// A failed lift is kept past the usual 72 hours while its reset (a weekly
// one) is still ahead: it still keeps answers from lifting that limit.
func TestFailedLiftOutlivesPruning(t *testing.T) {
	h := setup(t)
	_, j := liftedThenFailed(t, h)
	j.State, j.UpdatedAt = core.StateSkipped, h.now
	h.now = h.now.Add(4 * 24 * time.Hour)
	j.ResetAt = h.now.Add(48 * time.Hour)
	h.d.prune()
	if h.d.Jobs["grok:g1"] == nil {
		t.Fatalf("pruned before its reset")
	}
	h.now = j.ResetAt.Add(73 * time.Hour)
	h.d.prune()
	if h.d.Jobs["grok:g1"] != nil {
		t.Fatalf("kept after its reset")
	}
}

// A resume no route can ever deliver fails at once, instead of retrying
// (and keeping the computer awake) until the attempts run out.
func TestGoneSessionFailsAtOnce(t *testing.T) {
	h := setup(t)
	h.d.Handle(core.Event{Harness: "antigravity", Kind: core.KindUsageLimit, Source: "wrapper", At: h.now, ResetAt: h.now.Add(10 * time.Minute),
		Location: &core.Location{Kind: "wrapper", Target: "dead0000"}, Message: "Individual quota reached", Hit: "Individual quota reached"})
	h.fail = fmt.Errorf("%w; %w (wrapper: wrapper dead0000 is no longer running)", deliver.ErrNoRoute, deliver.ErrGone)
	h.advance(12 * time.Minute)
	j := h.d.Jobs["antigravity@wrapper:dead0000"]
	if j == nil || j.State != core.StateFailed || j.Attempts != 1 {
		t.Fatalf("want failed after one attempt: %+v", j)
	}
}

// A resume known only by a wrapper that has since exited can never be
// delivered: housekeeping drops it instead of listing it until it is due.
// One with a session id is kept: the agent's own command can continue it.
func TestPruneSkipsResumeOfExitedWrapper(t *testing.T) {
	h := setup(t)
	h.d.Handle(core.Event{Harness: "antigravity", Kind: core.KindUsageLimit, Source: "wrapper", At: h.now, ResetAt: h.now.Add(48 * time.Hour),
		Location: &core.Location{Kind: "wrapper", Target: "dead0001"}, Message: "Individual quota reached", Hit: "Individual quota reached"})
	h.d.Handle(core.Event{Harness: "codex", SessionID: "c1", Kind: core.KindUsageLimit, Source: "rollout", At: h.now, ResetAt: h.now.Add(48 * time.Hour),
		Location: &core.Location{Kind: "wrapper", Target: "dead0002"}, Message: "You've hit your usage limit."})
	h.d.prune()
	if j := h.d.Jobs["antigravity@wrapper:dead0001"]; j == nil || j.State != core.StatePending {
		t.Fatalf("skipped before an event with its session id could adopt it: %+v", j)
	}
	h.now = h.now.Add(6 * time.Minute)
	h.d.prune()
	if j := h.d.Jobs["antigravity@wrapper:dead0001"]; j == nil || j.State != core.StateSkipped {
		t.Fatalf("want skipped: %+v", j)
	}
	if j := h.d.Jobs["codex:c1"]; j == nil || j.State != core.StatePending {
		t.Fatalf("a session with an id must stay pending: %+v", j)
	}
}

// The conversation was continued in a second process, which has since
// exited; only the first, out-of-date copy is still open. Continuum doesn't
// type into it, and continues the conversation once that copy closes.
func TestNoResumeIntoOutOfDateCopy(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute)) // recorded process: deadPID
	copyPID := os.Getpid()
	h.live = &live.State{Checked: true, Open: true, Authoritative: true, Listed: true, Source: "registry",
		Session: &live.Session{SessionID: "s1", PID: copyPID}}
	h.advance(32 * time.Minute)
	j := h.d.Jobs["claude:s1"]
	if len(h.delivered) != 0 || j.State != core.StatePending || j.StaleCopyPID != copyPID || !j.WaitingOpen {
		t.Fatalf("typed into the out-of-date copy or not held: delivered=%d %+v", len(h.delivered), j)
	}
	// The copy is closed: the conversation is continued.
	h.live = &live.State{Checked: true, Open: false, Authoritative: true, Source: "registry"}
	h.advance(3 * time.Minute)
	if len(h.delivered) != 1 || j.State != core.StateDelivered || j.StaleCopyPID != 0 {
		t.Fatalf("not continued once the copy closed: delivered=%d %+v", len(h.delivered), j)
	}
}

// The listed process is the one the hooks last heard from: resumed as usual.
func TestResumeIntoRecordedProcess(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute))
	h.live = &live.State{Checked: true, Open: true, Authoritative: true, Listed: true, Source: "registry",
		Session: &live.Session{SessionID: "s1", PID: deadPID}}
	h.advance(32 * time.Minute)
	if len(h.delivered) != 1 {
		t.Fatalf("not resumed: %+v", h.d.Jobs["claude:s1"])
	}
}

// Closing an out-of-date copy (another process holding the same
// conversation) doesn't mark the session closed or drop its route.
func TestCopyQuittingLeavesSessionOpen(t *testing.T) {
	h := setup(t)
	h.d.HoldsFn = func(_, _ string, pid int) bool { return pid == 200 }
	ev := limitEvent(h, "s1", 30*time.Minute)
	ev.PID, ev.Location = 200, &core.Location{Kind: "wrapper", Target: "current"}
	h.d.Handle(ev)
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindSessionEnd, SessionID: "s1", Source: "hook:SessionEnd", At: h.now,
		PID: 100, Location: &core.Location{Kind: "wrapper", Target: "old"}})
	s, j := h.d.Sessions["claude:s1"], h.d.Jobs["claude:s1"]
	if s.Ended || s.PID != 200 || s.Location.Target != "current" {
		t.Fatalf("the copy's exit changed the session: %+v", s)
	}
	if j.Location == nil || j.Location.Target != "current" || j.PID != 200 {
		t.Fatalf("the copy's exit dropped the resume's route: %+v", j)
	}
	// The current process quitting is the session closing.
	h.d.HoldsFn = func(string, string, int) bool { return false }
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindSessionEnd, SessionID: "s1", Source: "hook:SessionEnd", At: h.now, PID: 200})
	if !s.Ended || j.Location != nil {
		t.Fatalf("the session's own exit not recorded: %+v %+v", s, j)
	}
	// A late event from before it ended doesn't reopen it.
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindReply, SessionID: "s1", Source: "hook:Stop", At: h.now.Add(-time.Second), PID: 200,
		Location: &core.Location{Kind: "wrapper", Target: "current"}})
	if !s.Ended || s.Location.Target == "current" && j.Location != nil {
		t.Fatalf("a late event reopened it: %+v", s)
	}
	// Anything a process reports afterwards means it runs again.
	h.now = h.now.Add(time.Minute)
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindReply, SessionID: "s1", Source: "hook:Stop", At: h.now, PID: 300})
	if s.Ended || s.PID != 300 {
		t.Fatalf("still ended after its process reported: %+v", s)
	}
}

// A weekly limit: the wait for an out-of-date copy to close lasts 24 hours
// from when it began, not from when the limit was hit days earlier.
func TestStaleCopyWaitCountsFromItsStart(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 72*time.Hour))
	h.live = &live.State{Checked: true, Open: true, Authoritative: true, Listed: true, Source: "registry",
		Session: &live.Session{SessionID: "s1", PID: os.Getpid()}}
	h.advance(73 * time.Hour)
	j := h.d.Jobs["claude:s1"]
	if j.State != core.StatePending || j.StaleCopyPID == 0 {
		t.Fatalf("gave up at once: %+v", j)
	}
	h.advance(23 * time.Hour)
	if j.State != core.StatePending {
		t.Fatalf("gave up before 24 hours of waiting: %+v", j)
	}
	h.advance(2 * time.Hour)
	if j.State != core.StateNeedsUser {
		t.Fatalf("still waiting after 24 hours: %+v", j)
	}
}

// When the copy closes the wait ends: a failed attempt after that is a retry,
// not "waiting for the copy".
func TestStaleCopyWaitEndsWhenCopyCloses(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute))
	h.live = &live.State{Checked: true, Open: true, Authoritative: true, Listed: true, Source: "registry",
		Session: &live.Session{SessionID: "s1", PID: os.Getpid()}}
	h.advance(32 * time.Minute)
	j := h.d.Jobs["claude:s1"]
	if !j.WaitingOpen || j.StaleCopyPID == 0 {
		t.Fatalf("not waiting: %+v", j)
	}
	h.live = &live.State{Checked: true, Open: false, Authoritative: true, Source: "registry"}
	h.fail = fmt.Errorf("%w (test)", deliver.ErrNoRoute)
	h.advance(3 * time.Minute)
	if j.WaitingOpen || j.StaleCopyPID != 0 || !j.WaitingSince.IsZero() || j.Attempts != 1 {
		t.Fatalf("still marked waiting for the copy: %+v", j)
	}
}

// The recorded process moved on to another conversation (still running):
// the old copy left holding this one is still out of date.
func TestStaleCopyWhenRecordedProcessMovedOn(t *testing.T) {
	h := setup(t)
	ev := limitEvent(h, "s1", 30*time.Minute)
	ev.PID = os.Getpid() // recorded, and alive
	h.d.Handle(ev)
	h.live = &live.State{Checked: true, Open: true, Authoritative: true, Listed: true, Source: "registry",
		Session: &live.Session{SessionID: "s1", PID: 4242}}
	h.advance(40 * time.Minute) // a live process gets a grace period first
	if j := h.d.Jobs["claude:s1"]; len(h.delivered) != 0 || j.StaleCopyPID != 4242 {
		t.Fatalf("typed into the out-of-date copy: %+v", j)
	}
}

// The listed process isn't the recorded one, but it has been active since
// the recorded process last reported (an old Continuum let a copy's exit
// overwrite the record): it is the copy in use, and is resumed.
func TestResumeIntoCopyActiveSinceRecord(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute)) // recorded: deadPID, now
	h.live = &live.State{Checked: true, Open: true, Authoritative: true, Listed: true, Source: "registry",
		Session: &live.Session{SessionID: "s1", PID: os.Getpid(), Status: "idle", StatusSince: h.now.Add(time.Minute)}}
	h.advance(32 * time.Minute)
	if j := h.d.Jobs["claude:s1"]; len(h.delivered) != 1 || j.StaleCopyPID != 0 {
		t.Fatalf("an active copy was treated as out of date: %+v", j)
	}
}

// Typing in the old copy (Claude's prompt-submit hook) makes it the current
// process at once: its resume goes there, and the other process is the copy.
func TestPromptMovesSessionToThatCopy(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute)) // recorded: deadPID
	h.now = h.now.Add(time.Minute)
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindActivity, SessionID: "s1", Source: "hook:UserPromptSubmit", At: h.now,
		PID: 4242, TurnStart: true, Location: &core.Location{Kind: "wrapper", Target: "old"}})
	if s := h.d.Sessions["claude:s1"]; s.PID != 4242 || s.Location.Target != "old" {
		t.Fatalf("the prompt didn't move the session: %+v", s)
	}
	hit := limitEvent(h, "s1", 30*time.Minute) // its turn hits the limit
	hit.PID, hit.Location = 4242, &core.Location{Kind: "wrapper", Target: "old"}
	h.d.Handle(hit)
	// Idle since long before: only the record says it is the one in use.
	h.live = &live.State{Checked: true, Open: true, Authoritative: true, Listed: true, Source: "registry",
		Session: &live.Session{SessionID: "s1", PID: 4242, Status: "idle", StatusSince: h.now.Add(-time.Hour)}}
	h.advance(40 * time.Minute)
	if j := h.d.Jobs["claude:s1"]; len(h.delivered) != 1 || j.StaleCopyPID != 0 {
		t.Fatalf("not resumed in the copy now in use: %+v", j)
	}
}

// The resumed turn fails at once with an error that schedules nothing (the
// API refused the request): the resume is reported as not taken. Later, or
// for another session, it is not.
func TestResumedTurnFailing(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute))
	h.advance(32 * time.Minute)
	j := h.d.Jobs["claude:s1"]
	if j.State != core.StateDelivered {
		t.Fatalf("not resumed: %+v", j)
	}
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindTurnFailed, SessionID: "other", Source: "hook:StopFailure", At: h.now.Add(time.Second), Message: "refused"})
	if j.TurnFailed != "" {
		t.Fatalf("another session's failure counted: %+v", j)
	}
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindTurnFailed, SessionID: "s1", Source: "hook:StopFailure", At: h.now.Add(5 * time.Second),
		Message: "API Error: the request was refused\nDetails: ..."})
	if j.TurnFailed != "API Error: the request was refused" || j.State != core.StateDelivered {
		t.Fatalf("resume failure not recorded: %+v", j)
	}
	h2 := setup(t)
	h2.d.Handle(limitEvent(h2, "s1", 30*time.Minute))
	h2.advance(32 * time.Minute)
	h2.d.Handle(core.Event{Harness: "claude", Kind: core.KindTurnFailed, SessionID: "s1", Source: "hook:StopFailure", At: h2.now.Add(time.Hour), Message: "refused"})
	if j := h2.d.Jobs["claude:s1"]; j.TurnFailed != "" {
		t.Fatalf("a failure an hour later counted as the resume failing: %+v", j)
	}
}

// A limit read off the screen of the out-of-date copy's pane (no session id):
// at due time the pane's conversation is looked up, and the resume gets the
// same check as the conversation's own, instead of being typed into the copy.
func TestScreenResumeOfOutOfDateCopyHeld(t *testing.T) {
	h := setup(t)
	pane := &core.Location{Kind: "wrapper", Target: "oldpane"}
	liveWrapper(t, h, "oldpane")
	h.d.PaneSessionFn = func(loc *core.Location) paneProcess {
		if loc.String() == pane.String() {
			return paneProcess{id: "s1", pid: 4242}
		}
		return paneProcess{}
	}
	// The conversation's record: last continued in pid 200, which quit.
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindSessionEnd, SessionID: "s1", Source: "hook:SessionEnd", At: h.now, PID: 200})
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindUsageLimit, Source: "wrapper", At: h.now.Add(time.Second), ResetAt: h.now.Add(30 * time.Minute),
		Location: pane, Message: "You've hit your 5-hour limit", Hit: "You've hit your"})
	h.live = &live.State{Checked: true, Open: true, Authoritative: true, Listed: true, Source: "registry",
		Session: &live.Session{SessionID: "s1", PID: 4242, Status: "idle", StatusSince: h.now.Add(-time.Hour)}}
	h.advance(40 * time.Minute) // a live wrapper gets a grace period first
	if len(h.delivered) != 0 {
		t.Fatalf("typed into the out-of-date copy: %+v", h.d.Jobs)
	}
	if j := h.d.Jobs["claude:s1"]; j == nil || j.StaleCopyPID != 4242 {
		for k, v := range h.d.Jobs {
			t.Logf("%s: %+v", k, *v)
		}
		t.Fatalf("not held as that conversation's resume")
	}
}

// The pane's conversation has its own resume (from its hooks): the screen's
// copy of it is dropped rather than typed in a second time.
func TestScreenResumeCoveredBySessionResume(t *testing.T) {
	h := setup(t)
	pane := &core.Location{Kind: "wrapper", Target: "p1"}
	liveWrapper(t, h, "p1")
	h.d.PaneSessionFn = func(*core.Location) paneProcess { return paneProcess{id: "s1", pid: 4242} }
	h.d.Handle(limitEvent(h, "s1", 60*time.Minute))
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindUsageLimit, Source: "wrapper", At: h.now.Add(10 * time.Minute), ResetAt: h.now.Add(30 * time.Minute),
		Location: pane, Message: "You've hit your 5-hour limit", Hit: "You've hit your"})
	h.advance(40 * time.Minute) // a live wrapper gets a grace period first
	if j := h.d.Jobs["claude@wrapper:p1"]; j == nil || j.State != core.StateSkipped || !strings.Contains(j.Result, "has its own resume") || len(h.delivered) != 0 {
		t.Fatalf("want the screen's resume dropped for the conversation's own: delivered=%d %+v", len(h.delivered), j)
	}
}

// liveWrapper makes wrapper id look running (its pid file names this process).
func liveWrapper(t *testing.T, h *harnessT, id string) {
	t.Helper()
	dir := filepath.Join(h.d.Paths.Wrappers, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pid"), []byte(fmt.Sprint(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Another agent's hooks are trusted to name the exact process only once that
// was seen to hold here, for its current build; then its out-of-date copies
// are recognised like Claude Code's.
func TestExactPIDsLearnedPerBuild(t *testing.T) {
	h := setup(t)
	h.d.Compat = &Compat{Agents: map[string]*AgentCompat{"grok": {Fingerprint: "grok-1"}}, Desktop: map[string]*DesktopCompat{}}
	if h.d.Compat.ExactPIDs("grok") || !h.d.Compat.ExactPIDs("claude") {
		t.Fatal("grok trusted before seen, or claude not trusted")
	}
	hook := func(pid int) core.Event {
		return core.Event{Harness: "grok", Kind: core.KindActivity, SessionID: "g1", Source: "hook:Stop", At: h.now, PID: pid}
	}
	// The agent lists another process for it: not learned.
	h.live = &live.State{Checked: true, Open: true, Authoritative: true, Listed: true, Source: "registry", Session: &live.Session{SessionID: "g1", PID: 78}}
	h.d.Handle(hook(77))
	if h.d.Compat.ExactPIDs("grok") {
		t.Fatal("learned from a mismatch")
	}
	// The same process: one agreement, not yet trusted.
	h.now = h.now.Add(2 * time.Minute)
	h.live.Session.PID = 77
	h.d.Handle(hook(77))
	if h.d.Compat.ExactPIDs("grok") {
		t.Fatal("trusted after a single agreement")
	}
	// A second, different process agrees: trusted.
	h.now = h.now.Add(time.Minute)
	h.live.Session.PID = 91
	h.d.Handle(hook(91))
	if !h.d.Compat.ExactPIDs("grok") {
		t.Fatal("not learned from two agreeing processes")
	}
	// An update: learned again from scratch.
	h.d.Compat.Agents["grok"].Fingerprint = "grok-2"
	if h.d.Compat.ExactPIDs("grok") {
		t.Fatal("still trusted after an update")
	}
}

// #1: a session the agent's list doesn't name (a process-scan guess, or the
// "process is alive" stand-in) teaches nothing and judges no copy.
func TestUnlistedSessionIsNoEvidence(t *testing.T) {
	h := setup(t)
	h.d.Compat = &Compat{Agents: map[string]*AgentCompat{"grok": {Fingerprint: "g"}}, Desktop: map[string]*DesktopCompat{}}
	h.live = &live.State{Checked: true, Open: true, Authoritative: true, Source: "registry", Session: &live.Session{SessionID: "g1", PID: 77}}
	h.d.Handle(core.Event{Harness: "grok", Kind: core.KindActivity, SessionID: "g1", Source: "hook:Stop", At: h.now, PID: 77})
	if h.d.Compat.ExactPIDs("grok") {
		t.Fatal("learned from an entry the agent's list doesn't name")
	}
	// Claude: an unlisted stand-in for another process is no out-of-date copy.
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute))
	h.live = &live.State{Checked: true, Open: true, Authoritative: true, Source: "registry", Session: &live.Session{SessionID: "s1", PID: 4242}}
	if pid := h.d.staleCopy(h.d.Jobs["claude:s1"], h.d.Sessions["claude:s1"], h.live); pid != 0 {
		t.Fatalf("judged an unlisted entry a copy: %d", pid)
	}
}

// #2: the pane's agent started after the limit: a different conversation,
// not adopted (and not typed into).
func TestNoAdoptionOfLaterConversation(t *testing.T) {
	h := setup(t)
	pane := &core.Location{Kind: "wrapper", Target: "p2"}
	liveWrapper(t, h, "p2")
	h.d.PaneSessionFn = func(*core.Location) paneProcess {
		return paneProcess{id: "c2", pid: 5151, started: h.now.Add(10 * time.Minute)}
	}
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindUsageLimit, Source: "wrapper", At: h.now, ResetAt: h.now.Add(30 * time.Minute),
		Location: pane, Message: "You've hit your 5-hour limit", Hit: "You've hit your"})
	h.advance(40 * time.Minute)
	if j := h.d.Jobs["claude@wrapper:p2"]; len(h.delivered) != 0 || j == nil || j.State != core.StateSkipped {
		t.Fatalf("adopted a conversation started after the limit: delivered=%d %+v", len(h.delivered), j)
	}
}

// #3: the conversation is open where it continued too: this pane holds a
// copy, and its screen's limit isn't typed into it.
func TestNoAdoptionWhenConversationOpenElsewhere(t *testing.T) {
	h := setup(t)
	pane := &core.Location{Kind: "wrapper", Target: "p3"}
	liveWrapper(t, h, "p3")
	h.d.HoldsFn = func(_, _ string, pid int) bool { return pid == 200 }
	h.d.PaneSessionFn = func(*core.Location) paneProcess { return paneProcess{id: "s1", pid: 4242} }
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindActivity, SessionID: "s1", Source: "hook:Stop", At: h.now, PID: 200})
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindUsageLimit, Source: "wrapper", At: h.now.Add(time.Second), ResetAt: h.now.Add(30 * time.Minute),
		Location: pane, Message: "You've hit your 5-hour limit", Hit: "You've hit your"})
	h.advance(40 * time.Minute)
	if j := h.d.Jobs["claude@wrapper:p3"]; len(h.delivered) != 0 || j == nil || j.State != core.StateSkipped {
		t.Fatalf("typed into the copy: delivered=%d %+v", len(h.delivered), j)
	}
}

// #4: a resume of that conversation the user cancelled for this stall stays
// cancelled; an excluded conversation isn't resumed through its pane.
func TestAdoptionRespectsCancelAndScope(t *testing.T) {
	h := setup(t)
	pane := &core.Location{Kind: "wrapper", Target: "p4"}
	liveWrapper(t, h, "p4")
	h.d.PaneSessionFn = func(*core.Location) paneProcess { return paneProcess{id: "s1", pid: 4242} }
	h.d.Jobs["claude:s1"] = &core.Job{Key: "claude:s1", Harness: "claude", SessionID: "s1", State: core.StateCancelled, Result: "cancelled by you", UpdatedAt: h.now, LimitAt: h.now}
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindUsageLimit, Source: "wrapper", At: h.now.Add(time.Second), ResetAt: h.now.Add(30 * time.Minute),
		Location: pane, Message: "You've hit your 5-hour limit", Hit: "You've hit your"})
	h.advance(40 * time.Minute)
	if len(h.delivered) != 0 || h.d.Jobs["claude:s1"].State != core.StateCancelled {
		t.Fatalf("a cancelled resume came back through the pane: delivered=%d %+v", len(h.delivered), h.d.Jobs["claude:s1"])
	}
}

// #5/#6: a failure after the resumed turn ended is the user's own; a resume
// tried again starts clean; carrying on clears the failure.
func TestResumeFailureWindowAndClearing(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute))
	h.advance(32 * time.Minute)
	j := h.d.Jobs["claude:s1"]
	at := j.DeliveredAt
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindActivity, SessionID: "s1", Source: "hook:Stop", At: at.Add(2 * time.Minute)})
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindTurnFailed, SessionID: "s1", Source: "hook:StopFailure", At: at.Add(9 * time.Minute), Message: "Prompt is too long"})
	if j.TurnFailed != "" {
		t.Fatalf("the user's later failed turn counted as the resume failing: %+v", j)
	}
	// Another resume: its own window.
	h2 := setup(t)
	h2.d.Handle(limitEvent(h2, "s1", 30*time.Minute))
	h2.advance(32 * time.Minute)
	j2 := h2.d.Jobs["claude:s1"]
	h2.d.Handle(core.Event{Harness: "claude", Kind: core.KindTurnFailed, SessionID: "s1", Source: "hook:StopFailure", At: j2.DeliveredAt.Add(5 * time.Second), Message: "first error."})
	if j2.TurnFailed != "first error" {
		t.Fatalf("not recorded (or trailing period kept): %q", j2.TurnFailed)
	}
	h2.d.control("resume-now s1")
	if j2.TurnFailed != "" || j2.ResumeSettled {
		t.Fatalf("resume-now kept the old failure: %+v", j2)
	}
	h2.advance(time.Minute)
	h2.d.Handle(core.Event{Harness: "claude", Kind: core.KindTurnFailed, SessionID: "s1", Source: "hook:StopFailure", At: j2.DeliveredAt.Add(5 * time.Second), Message: "second error"})
	if j2.TurnFailed != "second error" {
		t.Fatalf("the second resume's failure not recorded: %+v", j2)
	}
	// The user carries on: a turn ends normally.
	h2.d.Handle(core.Event{Harness: "claude", Kind: core.KindActivity, SessionID: "s1", Source: "hook:Stop", At: j2.DeliveredAt.Add(10 * time.Minute)})
	if j2.TurnFailed != "" {
		t.Fatalf("still reported failed after a turn ended normally: %+v", j2)
	}
}

// matchPane: the one interactive session whose process runs in the pane; a
// print-mode run started from it (same pane) doesn't count; two candidates
// can't be told apart.
func TestMatchPane(t *testing.T) {
	h := setup(t).d.Rules.Harness("claude")
	pane := &core.Location{Kind: "tmux", Target: "%3"}
	at := func(pid int) *core.Location {
		if pid == 1 || pid == 2 || pid == 3 {
			return pane
		}
		return &core.Location{Kind: "tmux", Target: "%9"}
	}
	start := time.Unix(1000, 0)
	one := []live.Session{{SessionID: "a", PID: 1, StartedAt: start}, {SessionID: "z", PID: 9}}
	if p := matchPane(h, one, pane, at); p.id != "a" || p.pid != 1 || !p.started.Equal(start) {
		t.Fatalf("one in the pane: %+v", p)
	}
	withPrint := append(one, live.Session{SessionID: "p", PID: 2, Entrypoint: "sdk-cli"})
	if p := matchPane(h, withPrint, pane, at); p.id != "a" {
		t.Fatalf("a print-mode run counted: %+v", p)
	}
	two := append(one, live.Session{SessionID: "b", PID: 3})
	if p := matchPane(h, two, pane, at); p.id != "" {
		t.Fatalf("two candidates: want none, got %+v", p)
	}
}

// Round 2, #1: an adopted pane resume that hits the limit again right away
// is resumed again, as one more attempt of the same resume.
func TestReLimitAfterAdoptedResume(t *testing.T) {
	h := setup(t)
	pane := &core.Location{Kind: "wrapper", Target: "r1"}
	liveWrapper(t, h, "r1")
	h.d.PaneSessionFn = func(*core.Location) paneProcess { return paneProcess{id: "s1", pid: 4242} }
	screen := func(reset time.Duration) {
		h.d.Handle(core.Event{Harness: "claude", Kind: core.KindUsageLimit, Source: "wrapper", At: h.now, ResetAt: h.now.Add(reset),
			Location: pane, Message: "You've hit your 5-hour limit", Hit: "You've hit your"})
	}
	screen(30 * time.Minute)
	h.advance(40 * time.Minute)
	if len(h.delivered) != 1 {
		t.Fatalf("first resume: delivered=%d %+v", len(h.delivered), h.d.Jobs)
	}
	h.now = h.now.Add(20 * time.Second)
	screen(60 * time.Minute) // back on a limit, a new reset
	h.advance(70 * time.Minute)
	j := h.d.Jobs["claude:s1"]
	if len(h.delivered) != 2 || j.Attempts != 2 {
		t.Fatalf("re-limit not resumed as a further attempt: delivered=%d %+v", len(h.delivered), j)
	}
}

// Round 2, #2: a days-old "needs you" resume of the conversation doesn't
// block a new stall in its pane.
func TestOldNeedsUserDoesNotBlockAdoption(t *testing.T) {
	h := setup(t)
	pane := &core.Location{Kind: "wrapper", Target: "r2"}
	liveWrapper(t, h, "r2")
	h.d.PaneSessionFn = func(*core.Location) paneProcess { return paneProcess{id: "s1", pid: 4242} }
	h.d.Jobs["claude:s1"] = &core.Job{Key: "claude:s1", Harness: "claude", SessionID: "s1", State: core.StateNeedsUser,
		UpdatedAt: h.now.Add(-48 * time.Hour), LimitAt: h.now.Add(-50 * time.Hour)}
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindUsageLimit, Source: "wrapper", At: h.now, ResetAt: h.now.Add(30 * time.Minute),
		Location: pane, Message: "You've hit your 5-hour limit", Hit: "You've hit your"})
	h.advance(40 * time.Minute)
	if len(h.delivered) != 1 {
		t.Fatalf("blocked by an old needs-you resume: %+v", h.d.Jobs)
	}
}

// Round 2, #3: the resumed turn fails while the delivery is still returning
// (a hidden terminal takes seconds): still the resume failing.
func TestResumeFailsDuringDelivery(t *testing.T) {
	h := setup(t)
	var failAt time.Time
	h.d.DeliverFn = func(o deliver.Options, j *core.Job) (deliver.Result, error) {
		failAt = h.now.Add(1500 * time.Millisecond)
		h.now = h.now.Add(3 * time.Second) // typing, then waiting before it returns
		return deliver.Result{Via: "test"}, nil
	}
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute))
	h.advance(32 * time.Minute)
	j := h.d.Jobs["claude:s1"]
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindTurnFailed, SessionID: "s1", Source: "hook:StopFailure", At: failAt, Message: "Prompt is too long"})
	if j.TurnFailed != "Prompt is too long" {
		t.Fatalf("a failure during delivery was missed: %+v", j)
	}
}

// screenLimit: a limit read off pane's screen (no session id).
func screenLimit(h *harnessT, pane *core.Location, reset time.Duration) {
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindUsageLimit, Source: "wrapper", At: h.now, ResetAt: h.now.Add(reset),
		Location: pane, Message: "You've hit your 5-hour limit", Hit: "You've hit your"})
}

// adoptionSetup: a live wrapper pane holding conversation s1 (pid 4242).
func adoptionSetup(t *testing.T, id string) (*harnessT, *core.Location) {
	h := setup(t)
	pane := &core.Location{Kind: "wrapper", Target: id}
	liveWrapper(t, h, id)
	h.d.PaneSessionFn = func(*core.Location) paneProcess { return paneProcess{id: "s1", pid: 4242} }
	return h, pane
}

// An excluded conversation isn't resumed through its pane either.
func TestAdoptionRespectsScope(t *testing.T) {
	h, pane := adoptionSetup(t, "sc1")
	sc := scope.Scope{Mode: scope.ModeAll}
	sc.Excluded, _ = scope.Add(nil, scope.Entry{Session: "s1"})
	if err := sc.Save(h.d.Paths); err != nil {
		t.Fatal(err)
	}
	h.d.reload()
	screenLimit(h, pane, 30*time.Minute)
	h.advance(40 * time.Minute)
	if j := h.d.Jobs["claude:s1"]; len(h.delivered) != 0 || j == nil || j.State != core.StateIgnored {
		t.Fatalf("an excluded conversation was resumed through its pane: delivered=%d %+v", len(h.delivered), j)
	}
}

// Its resume was ignored (not chosen) for this stall: stays so.
func TestAdoptionRespectsIgnored(t *testing.T) {
	h, pane := adoptionSetup(t, "ig1")
	// Only chosen sessions are resumed: this pane was chosen, the
	// conversation's own resume was not.
	sc := scope.Scope{Mode: scope.ModeChosen}
	sc.Chosen, _ = scope.Add(nil, scope.Entry{Wrapper: "ig1"})
	if err := sc.Save(h.d.Paths); err != nil {
		t.Fatal(err)
	}
	h.d.reload()
	h.d.Jobs["claude:s1"] = &core.Job{Key: "claude:s1", Harness: "claude", SessionID: "s1", State: core.StateIgnored, Result: "not chosen", UpdatedAt: h.now, LimitAt: h.now}
	screenLimit(h, pane, 30*time.Minute)
	h.advance(40 * time.Minute)
	if j := h.d.Jobs["claude@wrapper:ig1"]; len(h.delivered) != 0 || j == nil || j.State != core.StateSkipped {
		t.Fatalf("an ignored resume came back through the pane: delivered=%d %+v", len(h.delivered), j)
	}
}

// The conversation was resumed after this pane's limit: that covered it.
func TestAdoptionCoveredByLaterDelivery(t *testing.T) {
	h, pane := adoptionSetup(t, "dl1")
	screenLimit(h, pane, 30*time.Minute)
	h.d.Jobs["claude:s1"] = &core.Job{Key: "claude:s1", Harness: "claude", SessionID: "s1", State: core.StateDelivered,
		DeliveredAt: h.now.Add(10 * time.Minute), LimitAt: h.now.Add(-time.Hour), UpdatedAt: h.now.Add(10 * time.Minute)}
	h.advance(40 * time.Minute)
	if j := h.d.Jobs["claude@wrapper:dl1"]; len(h.delivered) != 0 || j == nil || j.State != core.StateSkipped {
		t.Fatalf("resumed twice: delivered=%d %+v", len(h.delivered), j)
	}
}

// The same notice right after a resume, with the reset already used (the
// limit hadn't lifted yet when it was typed): retried with backoff, not at
// the used reset, and not dropped for good.
func TestAdoptionRetriesRepeatedNotice(t *testing.T) {
	h, pane := adoptionSetup(t, "rn1")
	reset := h.now.Add(-time.Minute)
	h.d.Jobs["claude:s1"] = &core.Job{Key: "claude:s1", Harness: "claude", SessionID: "s1", State: core.StateDelivered, Attempts: 1,
		DeliveredAt: h.now.Add(-20 * time.Second), LimitAt: h.now.Add(-time.Hour), ResetAt: reset, ResetKnown: true, UpdatedAt: h.now}
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindUsageLimit, Source: "wrapper", At: h.now, ResetAt: reset,
		Location: pane, Message: "You've hit your 5-hour limit", Hit: "You've hit your"})
	h.advance(2 * time.Minute)
	if len(h.delivered) != 0 {
		t.Fatalf("typed again at once, at the used reset: %+v", h.d.Jobs)
	}
	h.advance(4 * time.Hour)
	if j := h.d.Jobs["claude:s1"]; len(h.delivered) != 1 || j.Attempts != 2 {
		t.Fatalf("not retried: delivered=%d %+v", len(h.delivered), j)
	}
}

// Back on a limit with the reset that was just used: not trusted again, so
// not resumed at it (rescheduled instead).
func TestAdoptionClearsUsedReset(t *testing.T) {
	h, pane := adoptionSetup(t, "ur1")
	reset := h.now.Add(10 * time.Minute)
	h.d.Jobs["claude:s1"] = &core.Job{Key: "claude:s1", Harness: "claude", SessionID: "s1", State: core.StateDelivered, Attempts: 1,
		DeliveredAt: h.now.Add(-5 * time.Minute), LimitAt: h.now.Add(-time.Hour), ResetAt: reset, ResetKnown: true, UpdatedAt: h.now}
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindUsageLimit, Source: "wrapper", At: h.now, ResetAt: reset,
		Location: pane, Message: "You've hit your 5-hour limit", Hit: "You've hit your"})
	h.advance(16 * time.Minute) // past the old reset's due time
	j := h.d.Jobs["claude:s1"]
	if len(h.delivered) != 0 || j == nil || j.ResetKnown || j.State != core.StatePending {
		t.Fatalf("resumed at a used reset: delivered=%d %+v", len(h.delivered), j)
	}
}

// Round 3: a pane that keeps going back on a limit after each resume stops
// at the attempt limit, as a session's own re-limits do.
func TestAdoptionStopsAtMaxAttempts(t *testing.T) {
	h, pane := adoptionSetup(t, "ma1")
	max := h.d.Rules.Defaults.MaxAttempts
	h.d.Jobs["claude:s1"] = &core.Job{Key: "claude:s1", Harness: "claude", SessionID: "s1", State: core.StateDelivered, Attempts: max,
		DeliveredAt: h.now.Add(-5 * time.Minute), LimitAt: h.now.Add(-time.Hour), UpdatedAt: h.now}
	screenLimit(h, pane, 30*time.Minute)
	h.advance(40 * time.Minute)
	if j := h.d.Jobs["claude:s1"]; len(h.delivered) != 0 || j.State != core.StateNeedsUser {
		t.Fatalf("kept resuming past the attempt limit: delivered=%d %+v", len(h.delivered), j)
	}
}

// A print-mode or SDK run in the pane isn't resumed.
func TestAdoptionSkipsPrintMode(t *testing.T) {
	h, pane := adoptionSetup(t, "pm1")
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindActivity, SessionID: "s1", Source: "hook:Stop", At: h.now, Entrypoint: "sdk-cli"})
	h.now = h.now.Add(time.Second)
	screenLimit(h, pane, 30*time.Minute)
	h.advance(40 * time.Minute)
	if j := h.d.Jobs["claude@wrapper:pm1"]; len(h.delivered) != 0 || j == nil || j.State != core.StateSkipped {
		t.Fatalf("resumed a print-mode run: delivered=%d %+v", len(h.delivered), j)
	}
}

// A prompt typed a while after the resume is the user's own: it ends the
// resume's window. The resume's own prompt (seconds after) doesn't.
func TestLaterPromptSettlesResume(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute))
	h.advance(32 * time.Minute)
	j := h.d.Jobs["claude:s1"]
	prompt := func(at time.Time) {
		h.d.Handle(core.Event{Harness: "claude", Kind: core.KindActivity, SessionID: "s1", Source: "hook:UserPromptSubmit", At: at, TurnStart: true})
	}
	prompt(j.DeliveredAt.Add(2 * time.Second))
	if j.ResumeSettled {
		t.Fatal("the resume's own prompt settled it")
	}
	prompt(j.DeliveredAt.Add(5 * time.Minute))
	if !j.ResumeSettled {
		t.Fatal("a later prompt didn't settle it")
	}
}

// A hook-reported limit of the resumed turn, while a slow delivery was still
// returning, is a new limit, not an old line.
func TestReLimitDuringSlowDelivery(t *testing.T) {
	h := setup(t)
	var hitAt time.Time
	h.d.DeliverFn = func(o deliver.Options, j *core.Job) (deliver.Result, error) {
		hitAt = h.now.Add(1500 * time.Millisecond)
		h.now = h.now.Add(3 * time.Second)
		return deliver.Result{Via: "test"}, nil
	}
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute))
	h.advance(32 * time.Minute)
	ev := limitEvent(h, "s1", 60*time.Minute)
	ev.At = hitAt
	h.d.Handle(ev)
	if j := h.d.Jobs["claude:s1"]; j.State != core.StatePending {
		t.Fatalf("the resumed turn's new limit was dropped as old: %+v", j)
	}
}

// matchPane leaves out background sessions too.
func TestMatchPaneSkipsBackground(t *testing.T) {
	hr := setup(t).d.Rules.Harness("claude")
	if hr.LiveSessions == nil || len(hr.LiveSessions.BackgroundKinds) == 0 {
		t.Skip("no background kinds in the rules")
	}
	pane := &core.Location{Kind: "tmux", Target: "%3"}
	at := func(int) *core.Location { return pane }
	list := []live.Session{{SessionID: "a", PID: 1}, {SessionID: "bg", PID: 2, Kind: hr.LiveSessions.BackgroundKinds[0]}}
	if p := matchPane(hr, list, pane, at); p.id != "a" {
		t.Fatalf("a background session counted: %+v", p)
	}
}

// A limit on a pane's screen isn't merged into a resume of a conversation in
// the same folder that has ended: it is another conversation's limit, and
// the ended one keeps its own resume (not typed into that pane).
func TestScreenLimitNotMergedIntoEndedSession(t *testing.T) {
	h := setup(t)
	liveWrapper(t, h, "other")
	h.d.Handle(limitEvent(h, "x", 30*time.Minute))
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindSessionEnd, SessionID: "x", Source: "hook:SessionEnd", At: h.now.Add(time.Second), PID: deadPID})
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindUsageLimit, Source: "wrapper", At: h.now.Add(time.Minute), ResetAt: h.now.Add(30 * time.Minute),
		Cwd: "/work/app", Location: &core.Location{Kind: "wrapper", Target: "other"}, Message: "You've hit your 5-hour limit", Hit: "You've hit your"})
	if j := h.d.Jobs["claude:x"]; j.Location != nil {
		t.Fatalf("the ended conversation's resume took the other pane: %+v", j)
	}
	if j := h.d.Jobs["claude@wrapper:other"]; j == nil || j.State != core.StatePending {
		t.Fatalf("the other pane's limit has no resume of its own: %+v", h.d.Jobs)
	}
}

// The other direction: a late line (hook or transcript) of an ended session
// isn't merged into another pane's screen-only resume in that folder.
func TestEndedSessionLineNotMergedIntoScreenResume(t *testing.T) {
	h := setup(t)
	liveWrapper(t, h, "other2")
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindSessionEnd, SessionID: "x", Source: "hook:SessionEnd", At: h.now, PID: deadPID})
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindUsageLimit, Source: "wrapper", At: h.now.Add(time.Second), ResetAt: h.now.Add(30 * time.Minute),
		Cwd: "/work/app", Location: &core.Location{Kind: "wrapper", Target: "other2"}, Message: "You've hit your 5-hour limit", Hit: "You've hit your"})
	late := limitEvent(h, "x", 30*time.Minute)
	late.At, late.Location, late.Source, late.PID = h.now.Add(2*time.Second), nil, "transcript", 0 // transcript lines carry no pid
	h.d.Handle(late)
	if j := h.d.Jobs["claude@wrapper:other2"]; j == nil || j.SessionID != "" {
		t.Fatalf("the screen-only resume was taken over by the ended session: %+v", h.d.Jobs)
	}
}

// A screen-only resume whose wrapper exited, for a stall its conversation's
// own resume covers: dropped without a "won't resume" notice.
func TestPruneQuietWhenConversationResumeCovers(t *testing.T) {
	h := setup(t)
	var notes []string
	h.d.Config.Notify = boolPtr(true)
	h.d.NotifyFn = func(title, body string) { notes = append(notes, body) }
	h.d.Handle(limitEvent(h, "s1", 2*time.Hour))
	h.d.Jobs["claude@wrapper:gone"] = &core.Job{Key: "claude@wrapper:gone", Harness: "claude", Cwd: "/work/app", State: core.StatePending,
		Location: &core.Location{Kind: "wrapper", Target: "gone"}, LimitAt: h.now, UpdatedAt: h.now, DueAt: h.now.Add(2 * time.Hour)}
	h.now = h.now.Add(6 * time.Minute)
	notes = nil // the "paused until reset" notice of the limit itself
	h.d.prune()
	if j := h.d.Jobs["claude@wrapper:gone"]; j.State != core.StateSkipped || len(notes) != 0 {
		t.Fatalf("want a quiet skip: %+v notes=%v", j, notes)
	}
}

func boolPtr(b bool) *bool { return &b }
