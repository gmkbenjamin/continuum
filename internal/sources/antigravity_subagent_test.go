package sources

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/rules"
)

func TestAntigravitySubagentIgnored(t *testing.T) {
	r, err := rules.Load("")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	subPath := filepath.Join(dir, "brain", "sub-1", ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(subPath), 0o700); err != nil {
		t.Fatal(err)
	}
	step0 := `{"step_index":0,"source":"SYSTEM","type":"SYSTEM_MESSAGE","status":"DONE","created_at":"2026-11-28T09:08:46Z","content":"The following is a <SYSTEM_MESSAGE>\n\n<SYSTEM_MESSAGE>\n[Message] timestamp=2026-11-28T09:08:46Z sender=8b4d2012-a267-4b0c-abb3-251fd6f7d076 priority=MESSAGE_PRIORITY_HIGH content=Review this repo\n</SYSTEM_MESSAGE>"}`
	step1 := `{"step_index":1,"source":"MODEL","type":"ERROR_MESSAGE","status":"ERROR","created_at":"2026-11-28T09:08:47Z","model":"claude-opus-5-5-high","error_message":"Third-party quota reached. Resets in 122h39m51s."}`
	if err := os.WriteFile(subPath, []byte(step0+"\n"+step1+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if !IsAntigravitySubagent(subPath) {
		t.Fatal("expected subagent transcript to be detected")
	}

	// Fresh scan from step 0 produces zero events.
	st := &AntigravityState{}
	evs := ParseAntigravityState(r, subPath, []Line{{Data: []byte(step0)}, {Data: []byte(step1)}}, st, time.UTC)
	if len(evs) != 0 || !st.Subagent {
		t.Fatalf("expected subagent ignored from step 0, got evs=%+v st=%+v", evs, st)
	}

	// Mid-file tail (step 0 not in lines slice) still inspects file header and ignores.
	stMid := &AntigravityState{}
	evsMid := ParseAntigravityState(r, subPath, []Line{{Data: []byte(step1)}}, stMid, time.UTC)
	if len(evsMid) != 0 || !stMid.Subagent {
		t.Fatalf("expected subagent ignored on mid-file tail, got evs=%+v st=%+v", evsMid, stMid)
	}

	// Normal user conversation is NOT flagged as a subagent.
	userPath := filepath.Join(dir, "brain", "user-1", ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(userPath), 0o700); err != nil {
		t.Fatal(err)
	}
	userStep0 := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-28T09:00:00Z","content":"<USER_REQUEST>\nhello\n</USER_REQUEST>"}`
	if err := os.WriteFile(userPath, []byte(userStep0+"\n"+step1+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if IsAntigravitySubagent(userPath) {
		t.Fatal("user conversation falsely detected as subagent")
	}
	stUser := &AntigravityState{}
	evsUser := ParseAntigravityState(r, userPath, []Line{{Data: []byte(userStep0)}, {Data: []byte(step1)}}, stUser, time.UTC)
	if len(evsUser) != 1 || stUser.Subagent {
		t.Fatalf("expected 1 limit event for user conversation, got evs=%+v st=%+v", evsUser, stUser)
	}
}
