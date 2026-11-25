package daemon

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/deliver"
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
