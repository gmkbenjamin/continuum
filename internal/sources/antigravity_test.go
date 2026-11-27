package sources

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/rules"
)

func TestParseAntigravityTranscript(t *testing.T) {
	r, err := rules.Load("")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "brain", "da14676e-8895-4a42-aec7-55e22b462917", ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	lines := writeLines(t, path, []string{
		`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-27T06:11:07Z","content":"hi"}`,
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-27T06:11:09Z","model":"gemini-4-neon-high","content":"Hello!"}`,
		`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-27T06:11:10Z","content":"test file says: Third-party quota reached. Resets in 2h30m50s."}`,
		`{"step_index":3,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-27T06:12:00Z","content":"continue"}`,
		`{"step_index":4,"source":"SYSTEM","type":"ERROR_MESSAGE","status":"ERROR","created_at":"2026-11-27T06:12:02Z","content":"⚠ Third-party quota reached. Please switch to Gemini models or upgrade your subscription to increase your limits. Resets in 2h30m50s."}`,
	})

	st := &AntigravityState{}
	evs := ParseAntigravityState(r, path, lines, st, time.UTC)
	if len(evs) < 2 {
		t.Fatalf("expected at least 2 events in chronological order, got %+v", evs)
	}
	if evs[len(evs)-1].Kind != core.KindUsageLimit {
		t.Fatalf("last event must be the trailing stall (chronological order for lastStall), got %+v", evs)
	}
	var limitEv, answerEv *core.Event
	for i := range evs {
		switch evs[i].Kind {
		case core.KindUsageLimit:
			limitEv = &evs[i]
		case core.KindActivity:
			if !evs[i].RequestAt.IsZero() {
				answerEv = &evs[i]
			}
		}
	}
	if answerEv == nil || answerEv.Model != "gemini-4-neon-high" || answerEv.SessionID != "da14676e-8895-4a42-aec7-55e22b462917" {
		t.Fatalf("expected answered activity event, got %+v (all: %+v)", answerEv, evs)
	}
	if limitEv == nil {
		t.Fatalf("expected usage_limit event, got %+v", evs)
	}
	wantReset := time.Date(2026, 11, 27, 6, 12, 2, 0, time.UTC).Add(2*time.Hour + 30*time.Minute + 50*time.Second)
	if !limitEv.ResetAt.Equal(wantReset) {
		t.Fatalf("ResetAt = %v, want %v", limitEv.ResetAt, wantReset)
	}
	if limitEv.SessionID != "da14676e-8895-4a42-aec7-55e22b462917" || limitEv.Source != "transcript" {
		t.Fatalf("limit event metadata: %+v", limitEv)
	}
}

func TestParseAntigravityReplyAndNeverResumeAndToolNoise(t *testing.T) {
	r, err := rules.Load("")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "brain", "conv-42", ".system_generated", "logs", "transcript.jsonl")

	// Tool output (GENERIC), even when status is ERROR, must not trigger a stall or never_resume.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	toolOnly := writeLines(t, path, []string{
		`{"step_index":0,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-11-27T06:11:10Z","content":"⚠ Third-party quota reached. Resets in 2h30m50s."}`,
		`{"step_index":1,"source":"MODEL","type":"GENERIC","status":"ERROR","created_at":"2026-11-27T06:11:11Z","content":"billing error: Third-party quota reached. Resets in 2h30m50s."}`,
	})
	if evs := ParseAntigravity(r, path, toolOnly, time.UTC); len(evs) != 0 {
		t.Fatalf("GENERIC tool output triggered events: %+v", evs)
	}

	// Normal assistant reply mentioning a quota message produces KindReply, not KindUsageLimit.
	replyPath := filepath.Join(dir, "brain", "conv-43", ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(replyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	replyLines := writeLines(t, replyPath, []string{
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-27T06:15:00Z","model":"gemini-4-neon-high","content":"The error printed was: Third-party quota reached."}`,
	})
	evs := ParseAntigravity(r, replyPath, replyLines, time.UTC)
	if len(evs) != 1 || evs[0].Kind != core.KindReply {
		t.Fatalf("expected KindReply, got %+v", evs)
	}

	// Billing error produces KindNeverResume, transient error produces KindTransient.
	errPath := filepath.Join(dir, "brain", "conv-44", ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(errPath), 0o700); err != nil {
		t.Fatal(err)
	}
	errLines := writeLines(t, errPath, []string{
		`{"step_index":1,"source":"SYSTEM","type":"ERROR_MESSAGE","status":"ERROR","created_at":"2026-11-27T06:16:00Z","content":"Your AI credits balance is too low to continue."}`,
		`{"step_index":2,"source":"SYSTEM","type":"ERROR_MESSAGE","status":"ERROR","created_at":"2026-11-27T06:17:00Z","error":"MODEL_CAPACITY_EXHAUSTED"}`,
	})
	evs = ParseAntigravity(r, errPath, errLines, time.UTC)
	if len(evs) != 2 || evs[0].Kind != core.KindNeverResume || evs[1].Kind != core.KindTransient {
		t.Fatalf("expected never_resume + transient, got %+v", evs)
	}
}
