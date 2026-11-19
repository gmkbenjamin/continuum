package sources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/rules"
)

func loadRules(t *testing.T) *rules.Rules {
	r, err := rules.Load("")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func syd(t *testing.T) *time.Location {
	l, err := time.LoadLocation("Australia/Sydney")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func copyFixture(t *testing.T, dir, name string) string {
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, name)
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dst
}

func TestClaudeLimit(t *testing.T) {
	r := loadRules(t)
	dir := t.TempDir()
	path := copyFixture(t, dir, "claude_limit.jsonl")
	tl := NewTailer()
	got := tl.Scan(dir, time.Time{}, 0)
	evs := ParseClaude(r, path, got[path], syd(t))
	if len(evs) != 1 {
		t.Fatalf("want 1 event (the limit; earlier activity is superseded), got %+v", evs)
	}
	ev := evs[0]
	if ev.Kind != core.KindUsageLimit || ev.SessionID != "5b9e2f6a-1111-4c2a-9d7e-000000000001" || ev.Cwd != "/work/app" {
		t.Fatalf("bad event %+v", ev)
	}
	// 22:03Z on 10 Nov is 09:03 on 11 Nov in Sydney: resets 10:00 that day.
	want := time.Date(2026, 11, 11, 10, 0, 0, 0, syd(t))
	if !ev.ResetAt.Equal(want) {
		t.Fatalf("reset %v, want %v", ev.ResetAt, want)
	}
	if st, _ := os.Stat(path); ev.TranscriptSize != st.Size() {
		t.Fatalf("transcript size %d, file %d", ev.TranscriptSize, st.Size())
	}

	// Nothing new: nothing returned.
	if again := tl.Scan(dir, time.Time{}, 0); len(again) != 0 {
		t.Fatalf("re-read lines: %v", again)
	}

	// The session resumes: activity is reported after the limit.
	extra, _ := os.ReadFile(filepath.Join("testdata", "claude_resumed.jsonl"))
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write(extra)
	f.Close()
	got = tl.Scan(dir, time.Time{}, 0)
	evs = ParseClaude(r, path, got[path], syd(t))
	if len(evs) != 1 || evs[0].Kind != core.KindActivity || !evs[0].At.After(ev.At) {
		t.Fatalf("want one activity event, got %+v", evs)
	}
	// ReadFrom sees the same thing from the limit's offset.
	if lines := ReadFrom(path, ev.TranscriptSize); len(lines) != 1 {
		t.Fatalf("ReadFrom: %d lines", len(lines))
	}
}

func TestClaudeOtherErrors(t *testing.T) {
	r := loadRules(t)
	dir := t.TempDir()
	path := copyFixture(t, dir, "claude_other_errors.jsonl")
	got := NewTailer().Scan(dir, time.Time{}, 0)
	evs := ParseClaude(r, path, got[path], time.UTC)
	kinds := []string{}
	for _, e := range evs {
		kinds = append(kinds, e.Kind)
	}
	// overloaded -> transient; bad API key -> needs the user; prompt too long -> ignored
	if len(evs) != 2 || evs[0].Kind != core.KindTransient || evs[1].Kind != core.KindNeverResume {
		t.Fatalf("got kinds %v", kinds)
	}
}

func TestCodexRollout(t *testing.T) {
	r := loadRules(t)
	dir := t.TempDir()
	day := filepath.Join(dir, "2026", "11", "04")
	os.MkdirAll(day, 0o700)
	name := "rollout-2026-11-04T20-25-51-0c0de000-0000-7000-8000-000000000024.jsonl"
	path := copyFixture(t, day, name)
	tl := NewTailer()
	got := tl.Scan(dir, time.Time{}, 5)
	st := &CodexState{}
	evs := ParseCodex(r, path, got[path], st, syd(t))
	if len(evs) != 1 {
		t.Fatalf("want 1 event, got %+v", evs)
	}
	ev := evs[0]
	if ev.Kind != core.KindUsageLimit || ev.SessionID != "0c0de000-0000-7000-8000-000000000024" || ev.Cwd != "/work/api" || ev.Entrypoint != "codex-tui" {
		t.Fatalf("bad event %+v", ev)
	}
	// The exhausted window's exact reset wins over the minute-precision text.
	if ev.ResetAt.Unix() != 1793861580 {
		t.Fatalf("reset %v (%d)", ev.ResetAt, ev.ResetAt.Unix())
	}
	if st.Originator != "codex-tui" {
		t.Fatalf("state %+v", st)
	}
}

func TestCodexWithoutStructuredLimits(t *testing.T) {
	r := loadRules(t)
	path := filepath.Join(t.TempDir(), "rollout-x-0c0de000-0000-7000-8000-000000000099.jsonl")
	line := `{"timestamp":"2026-11-04T09:25:51.350Z","type":"event_msg","payload":{"type":"task_complete","error":{"message":"You've hit your usage limit. Try again at 5:53 PM.","codex_error_info":"usage_limit_exceeded"}}}` + "\n"
	os.WriteFile(path, []byte(line), 0o600)
	lines := ReadFrom(path, 0)
	evs := ParseCodex(r, path, lines, &CodexState{}, syd(t))
	if len(evs) != 1 || evs[0].SessionID != "0c0de000-0000-7000-8000-000000000099" {
		t.Fatalf("got %+v", evs)
	}
	// 09:25Z = 20:25 Sydney, so 5:53 PM is the next day.
	want := time.Date(2026, 11, 5, 17, 53, 0, 0, syd(t))
	if !evs[0].ResetAt.Equal(want) {
		t.Fatalf("reset %v want %v", evs[0].ResetAt, want)
	}
}

func TestTailerStartsMidFileOnFirstSight(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.jsonl")
	f, _ := os.Create(path)
	for i := 0; i < 2000; i++ {
		f.WriteString(`{"type":"user","timestamp":"2026-11-10T22:00:00Z","message":{"content":"padding padding padding padding padding"}}` + "\n")
	}
	f.Close()
	tl := NewTailer()
	tl.InitialBytes = 4096
	lines := tl.Scan(dir, time.Time{}, 0)[path]
	if len(lines) == 0 || len(lines) > 40 {
		t.Fatalf("read %d lines", len(lines))
	}
	for _, l := range lines {
		if l.Data[0] != '{' {
			t.Fatalf("partial line returned: %q", l.Data[:20])
		}
	}
}

func TestPartialLastLineWaits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	os.WriteFile(path, []byte(`{"a":1}`+"\n"+`{"b":`), 0o600)
	tl := NewTailer()
	if l := tl.Scan(dir, time.Time{}, 0)[path]; len(l) != 1 {
		t.Fatalf("got %d lines", len(l))
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("2}\n")
	f.Close()
	l := tl.Scan(dir, time.Time{}, 0)[path]
	if len(l) != 1 || string(l[0].Data) != `{"b":2}` {
		t.Fatalf("got %v", l)
	}
}

func TestClaudeNativeGaveUpDropsPriorActivity(t *testing.T) {
	r := loadRules(t)
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	content := strings.Join([]string{
		`{"type":"assistant","sessionId":"s1","cwd":"/work/app","timestamp":"2026-11-11T09:00:00Z","message":{"content":[{"type":"text","text":"Working on it"}]}}`,
		`{"type":"system","sessionId":"s1","cwd":"/work/app","timestamp":"2026-11-11T09:01:00Z","content":"Claude Code exited during the wait, so the task will not resume on its own when the usage limit resets."}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	lines := ReadFrom(path, 0)
	evs := ParseClaude(r, path, lines, time.UTC)
	if len(evs) != 1 || evs[0].Kind != core.KindNativeGaveUp {
		t.Fatalf("got events %+v, want single NativeGaveUp without trailing activity", evs)
	}
}

// Claude Code's grace stop: on reaching the usage limit it adds a note to the
// turn and lets it finish on a short allowance. No limit error is written; the
// turn simply ends, and that end is the stall.
var graceStopLines = []string{
	`{"type":"user","sessionId":"g1","cwd":"/work/app","timestamp":"2026-11-19T09:20:00Z","message":{"role":"user","content":"ship it"}}`,
	`{"type":"assistant","sessionId":"g1","cwd":"/work/app","timestamp":"2026-11-19T09:27:00Z","message":{"content":[{"type":"text","text":"Building"}]}}`,
	`{"type":"user","sessionId":"g1","cwd":"/work/app","timestamp":"2026-11-19T09:28:06Z","isMeta":true,"turnCompanion":true,"usageLimitNote":"wrap_up","message":{"role":"user","content":"[Usage limit reached; a short grace allowance remains, then this turn is cut off without warning. Either way, end by telling the user their usage limit was reached, what's done and what's left.]"}}`,
	`{"type":"assistant","sessionId":"g1","cwd":"/work/app","timestamp":"2026-11-19T09:28:20Z","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{}}]}}`,
	`{"type":"user","sessionId":"g1","cwd":"/work/app","timestamp":"2026-11-19T09:28:22Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
	`{"type":"assistant","sessionId":"g1","cwd":"/work/app","timestamp":"2026-11-19T09:28:31Z","message":{"content":[{"type":"text","text":"Your usage limit was reached, so I had to stop partway."}]}}`,
	`{"type":"system","subtype":"stop_hook_summary","sessionId":"g1","cwd":"/work/app","timestamp":"2026-11-19T09:28:31.321Z"}`,
	`{"type":"system","subtype":"turn_duration","sessionId":"g1","cwd":"/work/app","timestamp":"2026-11-19T09:28:31.326Z","isMeta":false}`,
}

func writeLines(t *testing.T, path string, lines []string) []Line {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return ReadFrom(path, 0)
}

func TestClaudeGraceStop(t *testing.T) {
	r := loadRules(t)
	path := filepath.Join(t.TempDir(), "g1.jsonl")
	evs := ParseClaude(r, path, writeLines(t, path, graceStopLines), time.UTC)
	if len(evs) != 1 || evs[0].Kind != core.KindUsageLimit {
		t.Fatalf("want one usage limit at the turn's end, got %+v", evs)
	}
	ev := evs[0]
	if want := time.Date(2026, 11, 19, 9, 28, 31, 321e6, time.UTC); !ev.At.Equal(want) {
		t.Fatalf("limit at %v, want the turn's end %v", ev.At, want)
	}
	if !ev.ResetAt.IsZero() || !strings.Contains(ev.Message, "Usage limit reached") || ev.SessionID != "g1" {
		t.Fatalf("bad event %+v", ev)
	}
}

// The wrapping-up turn usually spans several scans: its work in between is not
// the session moving on, and the stall is reported when the turn ends.
func TestClaudeGraceStopAcrossScans(t *testing.T) {
	r := loadRules(t)
	path := filepath.Join(t.TempDir(), "g1.jsonl")
	lines := writeLines(t, path, graceStopLines)
	st := &ClaudeState{}
	first := ParseClaudeState(r, path, lines[:4], st, time.UTC)
	for _, ev := range first {
		if ev.At.After(time.Date(2026, 11, 19, 9, 28, 6, 0, time.UTC)) {
			t.Fatalf("the wrapping-up turn reported %+v", ev)
		}
	}
	if st.WrapUp == "" {
		t.Fatal("the grace note was not carried over")
	}
	second := ParseClaudeState(r, path, lines[4:], st, time.UTC)
	if len(second) != 1 || second[0].Kind != core.KindUsageLimit || st.WrapUp != "" {
		t.Fatalf("want the limit at the turn's end, got %+v (state %+v)", second, st)
	}
}

func TestClaudeGraceStopThenPrompt(t *testing.T) {
	r := loadRules(t)
	path := filepath.Join(t.TempDir(), "g1.jsonl")
	lines := append(append([]string{}, graceStopLines...),
		`{"type":"user","sessionId":"g1","cwd":"/work/app","timestamp":"2026-11-19T11:45:00Z","message":{"role":"user","content":"continue"}}`)
	evs := ParseClaude(r, path, writeLines(t, path, lines), time.UTC)
	if len(evs) != 2 || evs[0].Kind != core.KindUsageLimit || evs[1].Kind != core.KindActivity {
		t.Fatalf("want the limit, then activity, got %+v", evs)
	}
}

// The allowance ran out mid-turn: Claude Code's own limit error, with its
// reset time, is the stall; the turn's end doesn't add a second one.
func TestClaudeGraceStopCutOff(t *testing.T) {
	r := loadRules(t)
	path := filepath.Join(t.TempDir(), "g1.jsonl")
	lines := append(append([]string{}, graceStopLines[:5]...),
		`{"type":"assistant","sessionId":"g1","cwd":"/work/app","timestamp":"2026-11-19T09:29:00Z","isApiErrorMessage":true,"error":"rate_limit","message":{"content":[{"type":"text","text":"You've hit your limit · resets 10:40pm (Australia/Sydney)"}]}}`,
		graceStopLines[7])
	evs := ParseClaude(r, path, writeLines(t, path, lines), syd(t))
	if len(evs) != 1 || evs[0].Kind != core.KindUsageLimit || evs[0].ResetAt.IsZero() {
		t.Fatalf("want the limit error with its reset time only, got %+v", evs)
	}
}
