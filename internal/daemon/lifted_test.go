package daemon

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
)

// weeklyLimit: session s hit a weekly limit (reset 3 days away), read from
// its transcript under h's Claude config folder.
func weeklyLimit(h *harnessT, s, msg string) core.Event {
	ev := limitEvent(h, s, 72*time.Hour)
	ev.Message = msg
	ev.Source = "transcript"
	ev.TranscriptPath = filepath.Join(h.claude, "projects", "-work-app", s+".jsonl")
	return ev
}

// answer: session s got an answer from model, on the plan, to a request
// sent at req.
func answer(h *harnessT, s, model string, req time.Time) core.Event {
	return core.Event{
		Harness: "claude", Kind: core.KindActivity, SessionID: s, Cwd: "/work/other", Source: "transcript",
		TranscriptPath: filepath.Join(h.claude, "projects", "-work-other", s+".jsonl"),
		At:             req.Add(20 * time.Second), RequestAt: req, Model: model,
	}
}

// A banked reset used on claude.ai lifts the weekly limit early. Another
// session of the account getting an answer shows it, and the waiting session
// continues then, not 3 days later.
func TestBankedResetSeenFromAnotherSessionsAnswer(t *testing.T) {
	h := setup(t)
	limit := weeklyLimit(h, "s1", "You've hit your weekly limit · resets Nov 14, 9am")
	h.d.Handle(limit)
	j := h.d.Jobs["claude:s1"]
	if j == nil || j.State != core.StatePending || j.DueAt.Before(h.now.Add(72*time.Hour)) {
		t.Fatalf("want a resume after the weekly reset: %+v", j)
	}

	h.advance(2 * time.Hour)
	h.d.Handle(answer(h, "s2", "claude-opus-5-5", h.now.Add(-time.Minute)))
	if !j.LiftedAt.Equal(h.now.Add(-40*time.Second)) || !j.LiftedFrom.Equal(limit.ResetAt) {
		t.Fatalf("not lifted: lifted %v from %v", j.LiftedAt, j.LiftedFrom)
	}
	if j.DueAt.After(h.now.Add(time.Minute)) {
		t.Fatalf("due %v, want within a minute of %v", j.DueAt, h.now)
	}
	// A late report of the same stall doesn't put it back.
	dup := limit
	dup.Source = "hook:StopFailure"
	h.d.Handle(dup)
	if j.DueAt.After(h.now.Add(time.Minute)) {
		t.Fatalf("a late report of the stall put the resume back to %v", j.DueAt)
	}
	h.advance(time.Minute)
	if len(h.delivered) != 1 || j.State != core.StateDelivered {
		t.Fatalf("not resumed early: delivered=%d state=%s", len(h.delivered), j.State)
	}

	// The old notice redrawn on screen afterwards isn't a new limit…
	h.advance(15 * time.Minute)
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindUsageLimit, Message: limit.Message, Hit: "You've hit your weekly limit",
		At: h.now, ResetAt: limit.ResetAt, Source: "pane-scan", Location: limit.Location})
	for k, o := range h.d.Jobs {
		if o.State == core.StatePending {
			t.Fatalf("the old notice on screen scheduled %s again", k)
		}
	}
	// …but hitting the limit again (it wasn't lifted after all) waits for its reset.
	again := weeklyLimit(h, "s1", limit.Message)
	again.At, again.ResetAt = h.now, limit.ResetAt
	h.d.Handle(again)
	if j2 := h.d.Jobs["claude:s1"]; j2.State != core.StatePending || !j2.ResetAt.Equal(limit.ResetAt) {
		t.Fatalf("a real new limit: %+v", j2)
	}
}

func TestAnswersThatDontLiftALimit(t *testing.T) {
	cases := map[string]func(h *harnessT) core.Event{
		"request sent before the limit hit": func(h *harnessT) core.Event {
			return answer(h, "s2", "claude-opus-5-5", h.now.Add(-10*time.Second))
		},
		"another account": func(h *harnessT) core.Event {
			ev := answer(h, "s2", "claude-opus-5-5", h.now.Add(time.Hour))
			ev.TranscriptPath = filepath.Join(h.claude+"-work", "projects", "-work-other", "s2.jsonl")
			return ev
		},
		"another model than the limit's": func(h *harnessT) core.Event {
			return answer(h, "s2", "claude-opus-5-5", h.now.Add(time.Hour))
		},
	}
	for name, mk := range cases {
		h := setup(t)
		msg := "You've hit your weekly limit · resets Nov 14, 9am"
		if name == "another model than the limit's" {
			msg = "You've hit your Fable limit · resets Nov 14, 9am"
		}
		h.d.Handle(weeklyLimit(h, "s1", msg))
		due := h.d.Jobs["claude:s1"].DueAt
		ev := mk(h)
		h.now = ev.At
		h.d.Handle(ev)
		if j := h.d.Jobs["claude:s1"]; !j.DueAt.Equal(due) || !j.LiftedAt.IsZero() {
			t.Errorf("%s: moved the resume to %v", name, j.DueAt)
		}
	}
}

func TestModelLimitLiftedByTheSameModel(t *testing.T) {
	h := setup(t)
	h.d.Handle(weeklyLimit(h, "s1", "You've hit your Fable limit · resets Nov 14, 9am"))
	h.now = h.now.Add(time.Hour)
	h.d.Handle(answer(h, "s2", "claude-fable-5-2", h.now.Add(-time.Minute)))
	if j := h.d.Jobs["claude:s1"]; j.LiftedAt.IsZero() || j.DueAt.After(h.now.Add(time.Minute)) {
		t.Fatalf("not lifted: %+v", j)
	}
}

// /limit-reset used in Claude Code, even in another session of the account.
func TestLimitResetUsedLiftsTheAccountsLimits(t *testing.T) {
	h := setup(t)
	h.d.Handle(weeklyLimit(h, "s1", "You've hit your weekly limit · resets Nov 14, 9am"))
	h.d.Handle(weeklyLimit(h, "s3", "You've hit your weekly limit · resets Nov 14, 9am"))
	h.now = h.now.Add(time.Hour)
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindLimitReset, SessionID: "s3", Message: "Reset used · 1 left",
		TranscriptPath: filepath.Join(h.claude, "projects", "-work-app", "s3.jsonl"), At: h.now, Source: "transcript"})
	for _, k := range []string{"claude:s1", "claude:s3"} {
		if j := h.d.Jobs[k]; j.LiftedAt.IsZero() || j.DueAt.After(h.now.Add(time.Minute)) {
			t.Fatalf("%s not lifted: %+v", k, j)
		}
	}
}

// The answer can be read before the stall it lifts (two transcripts read in
// one scan, in either order).
func TestAnswerReadBeforeTheStall(t *testing.T) {
	h := setup(t)
	limit := weeklyLimit(h, "s1", "You've hit your weekly limit · resets Nov 14, 9am")
	h.now = h.now.Add(5 * time.Minute)
	h.d.Handle(answer(h, "s2", "claude-opus-5-5", limit.At.Add(2*time.Minute)))
	h.d.Handle(limit)
	if j := h.d.Jobs["claude:s1"]; j.LiftedAt.IsZero() || j.DueAt.After(h.now.Add(time.Minute)) {
		t.Fatalf("not lifted: %+v", j)
	}
}

// A resume already due sooner isn't moved.
func TestLiftNeverPostponesAResume(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", time.Minute))
	j := h.d.Jobs["claude:s1"]
	j.TranscriptPath = filepath.Join(h.claude, "projects", "-work-app", "s1.jsonl")
	due := j.DueAt
	h.d.Handle(answer(h, "s2", "claude-opus-5-5", h.now.Add(time.Minute)))
	if !j.DueAt.Equal(due) || !j.LiftedAt.IsZero() {
		t.Fatalf("due %v (was %v), lifted %v", j.DueAt, due, j.LiftedAt)
	}
}

// Codex keeps rollouts in dated folders: sessions on other days are still one
// account. Codex records the plan's limits with each answer.
func TestCodexBankedReset(t *testing.T) {
	h := setup(t)
	rollout := func(day, id string) string {
		return filepath.Join(h.codex, "sessions", "2026", "11", day, "rollout-2026-11-"+day+"T08-00-00-"+id+".jsonl")
	}
	h.d.Handle(core.Event{Harness: "codex", Kind: core.KindUsageLimit, SessionID: "c1", Cwd: "/work/app", Source: "rollout",
		TranscriptPath: rollout("10", "c1"), At: h.now, ResetAt: h.now.Add(96 * time.Hour), LimitID: "codex", PID: deadPID,
		Message: "You've hit your usage limit."})
	j := h.d.Jobs["codex:c1"]
	due := j.DueAt
	h.now = h.now.Add(time.Hour)
	other := core.Event{Harness: "codex", Kind: core.KindActivity, SessionID: "c2", Source: "rollout",
		TranscriptPath: rollout("11", "c2"), At: h.now, RequestAt: h.now.Add(-time.Minute), LimitID: "codex-other"}
	h.d.Handle(other)
	if !j.DueAt.Equal(due) {
		t.Fatalf("an answer counted against another limit lifted it: %v", j.DueAt)
	}
	other.LimitID = "codex"
	h.d.Handle(other)
	if j.LiftedAt.IsZero() || j.DueAt.After(h.now.Add(time.Minute)) {
		t.Fatalf("not lifted: %+v", j)
	}
}

// Grok Build, Muse Code and the Cursor CLI report answers through Stop hooks.
func TestStopHookAnswerLiftsAHookAgentsLimit(t *testing.T) {
	h := setup(t)
	h.d.Handle(core.Event{Harness: "grok", Kind: core.KindUsageLimit, SessionID: "g1", Cwd: "/work/app", Source: "hook:StopFailure",
		At: h.now, ResetAt: h.now.Add(72 * time.Hour), PID: deadPID, Location: &core.Location{Kind: "wrapper", Target: "w1"}})
	j := h.d.Jobs["grok:g1"]
	h.handleAll(hookTurn(h, "grok", "g2", "", h.now.Add(50*time.Minute), h.now.Add(time.Hour)))
	if j.LiftedAt.IsZero() || j.DueAt.After(h.now.Add(time.Minute)) {
		t.Fatalf("not lifted: %+v", j)
	}
	// Another agent's answer says nothing about Grok's limits.
	h.d.Handle(core.Event{Harness: "muse", Kind: core.KindUsageLimit, SessionID: "m1", Cwd: "/work/app", Source: "hook:StopFailure",
		At: h.now, ResetAt: h.now.Add(72 * time.Hour), PID: deadPID})
	h.handleAll(hookTurn(h, "grok", "g2", "", h.now.Add(50*time.Minute), h.now.Add(time.Hour)))
	if m := h.d.Jobs["muse:m1"]; !m.LiftedAt.IsZero() {
		t.Fatalf("Grok's answer lifted Muse's limit: %+v", m)
	}
}

// An early resume that hits the limit again straight away: the answers were
// paid some other way. They don't lift the limit again before its reset.
func TestFailedLiftIsNotRepeated(t *testing.T) {
	h := setup(t)
	limit := weeklyLimit(h, "s1", "You've hit your weekly limit · resets Nov 14, 9am")
	h.d.Handle(limit)
	h.now = h.now.Add(time.Hour)
	h.d.Handle(answer(h, "s2", "claude-opus-5-5", h.now.Add(-time.Minute)))
	h.advance(2 * time.Minute)
	if len(h.delivered) != 1 {
		t.Fatalf("not resumed early: %d", len(h.delivered))
	}
	again := weeklyLimit(h, "s1", limit.Message)
	again.At, again.ResetAt = h.now.Add(time.Minute), limit.ResetAt
	h.now = again.At
	h.d.Handle(again)
	j := h.d.Jobs["claude:s1"]
	if !j.LiftFailed || j.State != core.StatePending || !j.ResetAt.Equal(limit.ResetAt) {
		t.Fatalf("want a resume at the reset, marked lift-failed: %+v", j)
	}
	h.now = h.now.Add(10 * time.Minute)
	h.d.Handle(answer(h, "s2", "claude-opus-5-5", h.now.Add(-time.Minute)))
	if !j.ResetAt.Equal(limit.ResetAt) || j.DueAt.Before(limit.ResetAt) {
		t.Fatalf("lifted again: due %v", j.DueAt)
	}
	// A reset the agent reports used still counts.
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindLimitReset, SessionID: "s2", Message: "Reset used · 0 left",
		TranscriptPath: filepath.Join(h.claude, "projects", "-work-other", "s2.jsonl"), At: h.now, Source: "transcript"})
	if j.DueAt.After(h.now.Add(time.Minute)) {
		t.Fatalf("a reset used didn't lift it: due %v", j.DueAt)
	}
}
