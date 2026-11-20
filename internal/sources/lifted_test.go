package sources

import (
	"strings"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
)

func rawLines(raw ...string) []Line {
	var out []Line
	for i, r := range raw {
		out = append(out, Line{Data: []byte(r), EndPos: int64(i + 1)})
	}
	return out
}

const (
	askLine    = `{"type":"user","sessionId":"b1","cwd":"/w","timestamp":"2026-11-20T10:00:00Z","message":{"role":"user","content":"go on"}}`
	answerLine = `{"type":"assistant","sessionId":"b1","cwd":"/w","timestamp":"2026-11-20T10:00:20Z","message":{"model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"Done."}],"usage":{"output_tokens":3,"fallback_credit":null}}}`
)

func TestPlanAnswerCarriesItsRequestTime(t *testing.T) {
	r := loadRules(t)
	evs := ParseClaude(r, "/c/projects/-w/b1.jsonl", rawLines(askLine, answerLine), time.UTC)
	if len(evs) != 1 || evs[0].Kind != core.KindActivity {
		t.Fatalf("events %+v", evs)
	}
	if want := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC); !evs[0].RequestAt.Equal(want) || evs[0].Model != "claude-opus-5-5" {
		t.Fatalf("request %v model %q, want %v claude-opus-5-5", evs[0].RequestAt, evs[0].Model, want)
	}

	// The question and its answer read in two scans.
	st := &ClaudeState{}
	first := ParseClaudeState(r, "/c/projects/-w/b1.jsonl", rawLines(askLine), st, time.UTC)
	if len(first) != 1 || !first[0].RequestAt.IsZero() || !st.Keep() {
		t.Fatalf("first scan %+v state %+v", first, st)
	}
	second := ParseClaudeState(r, "/c/projects/-w/b1.jsonl", rawLines(answerLine), st, time.UTC)
	if len(second) != 1 || second[0].RequestAt.IsZero() || st.Keep() {
		t.Fatalf("second scan %+v state %+v", second, st)
	}
}

func TestAnswersThatDontShowTheLimitLifted(t *testing.T) {
	r := loadRules(t)
	for name, answer := range map[string]string{
		"usage credits":          strings.Replace(answerLine, `"fallback_credit":null`, `"fallback_credit":{"amount":0.12}`, 1),
		"Claude Code's own line": strings.Replace(answerLine, `claude-opus-5-5`, `<synthetic>`, 1),
		"no model":               strings.Replace(answerLine, `"model":"claude-opus-5-5",`, ``, 1),
	} {
		evs := ParseClaude(r, "/c/projects/-w/b1.jsonl", rawLines(askLine, answer), time.UTC)
		for _, ev := range evs {
			if !ev.RequestAt.IsZero() {
				t.Errorf("%s: counted as an answer on the plan: %+v", name, ev)
			}
		}
	}
	// A turn finishing on the grace allowance after the limit is reached.
	wrap := `{"type":"user","sessionId":"b1","cwd":"/w","timestamp":"2026-11-20T10:00:10Z","isMeta":true,"usageLimitNote":"wrap_up","message":{"role":"user","content":"[Usage limit reached; a short grace allowance remains.]"}}`
	tool := `{"type":"user","sessionId":"b1","cwd":"/w","timestamp":"2026-11-20T10:00:15Z","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`
	for _, ev := range ParseClaude(r, "/c/projects/-w/b1.jsonl", rawLines(askLine, wrap, tool, answerLine), time.UTC) {
		if !ev.RequestAt.IsZero() {
			t.Errorf("an answer on the grace allowance counted: %+v", ev)
		}
	}
}

func TestLimitResetUsed(t *testing.T) {
	r := loadRules(t)
	for _, line := range []string{
		`{"type":"system","subtype":"local_command","sessionId":"b1","cwd":"/w","timestamp":"2026-11-20T10:00:00Z","content":"<local-command-stdout>Reset used · 1 left</local-command-stdout>"}`,
		`{"type":"user","sessionId":"b1","cwd":"/w","timestamp":"2026-11-20T10:00:00Z","message":{"role":"user","content":"<local-command-stdout>Reset used · 0 left</local-command-stdout>"}}`,
	} {
		evs := ParseClaude(r, "/c/projects/-w/b1.jsonl", rawLines(line), time.UTC)
		if len(evs) != 1 || evs[0].Kind != core.KindLimitReset || !strings.HasPrefix(evs[0].Message, "Reset used") {
			t.Fatalf("events %+v", evs)
		}
	}
	// Saying it, or another command's output, isn't using one.
	for _, line := range []string{
		`{"type":"user","sessionId":"b1","cwd":"/w","timestamp":"2026-11-20T10:00:00Z","message":{"role":"user","content":"I got 'Reset used' earlier, why?"}}`,
		`{"type":"system","subtype":"local_command","sessionId":"b1","cwd":"/w","timestamp":"2026-11-20T10:00:00Z","content":"<local-command-stdout>Session renamed to: reset-used</local-command-stdout>"}`,
	} {
		for _, ev := range ParseClaude(r, "/c/projects/-w/b1.jsonl", rawLines(line), time.UTC) {
			if ev.Kind == core.KindLimitReset {
				t.Fatalf("not a reset used: %s", line)
			}
		}
	}
}

func rolloutLine(ts, typ, payload string) string {
	return `{"timestamp":"2026-11-20T10:` + ts + `Z","type":"` + typ + `","payload":` + payload + `}`
}

// Codex writes the plan's limits after each answer: room left after another
// session's limit means it is back.
func TestCodexAnswerOnThePlan(t *testing.T) {
	r := loadRules(t)
	path := "/c/sessions/2026/11/20/rollout-2026-11-20T10-00-00-0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000.jsonl"
	turn := func(used string) []string {
		return []string{
			rolloutLine("00:00", "event_msg", `{"type":"task_started"}`),
			rolloutLine("00:01", "response_item", `{"type":"message","role":"user","content":[]}`),
			rolloutLine("00:05", "response_item", `{"type":"reasoning"}`),
			rolloutLine("00:06", "response_item", `{"type":"function_call","name":"shell"}`),
			rolloutLine("00:07", "response_item", `{"type":"function_call_output"}`),
			rolloutLine("00:07", "event_msg", `{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":`+used+`,"window_minutes":300,"resets_at":1795000000},"secondary":null,"rate_limit_reached_type":null}}`),
		}
	}
	st := &CodexState{SessionID: "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000"}
	var got *core.Event
	for _, ev := range ParseCodex(r, path, rawLines(turn("12.0")...), st, time.UTC) {
		if !ev.RequestAt.IsZero() {
			got = &ev
		}
	}
	if got == nil || !got.RequestAt.Equal(time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)) || got.LimitID != "codex" {
		t.Fatalf("answer %+v", got)
	}
	// The tool's output went back to the model: that is the next request.
	if !st.Trigger.Equal(time.Date(2026, 11, 20, 10, 0, 7, 0, time.UTC)) || !st.InFlight.IsZero() {
		t.Fatalf("state %+v", st)
	}
	// Still at the limit (paid from credits): not on the plan.
	st = &CodexState{SessionID: "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000"}
	for _, ev := range ParseCodex(r, path, rawLines(turn("100.0")...), st, time.UTC) {
		if !ev.RequestAt.IsZero() {
			t.Fatalf("an answer with the limit used up counted: %+v", ev)
		}
	}
}

func TestCodexLimitNamesItsLimit(t *testing.T) {
	r := loadRules(t)
	path := "/c/sessions/2026/11/20/rollout-2026-11-20T10-00-00-0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000.jsonl"
	st := &CodexState{SessionID: "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000"}
	evs := ParseCodex(r, path, rawLines(
		rolloutLine("00:00", "event_msg", `{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":100.0,"window_minutes":300,"resets_at":1795000000},"secondary":null}}`),
		rolloutLine("00:01", "event_msg", `{"type":"error","message":"You've hit your usage limit.","codex_error_info":"usage_limit_exceeded"}`),
	), st, time.UTC)
	if len(evs) != 1 || evs[0].Kind != core.KindUsageLimit || evs[0].LimitID != "codex" {
		t.Fatalf("events %+v", evs)
	}
}

// A snapshot from an earlier turn may be another model's limit (a /model
// switch in between), and one with room left isn't the limit hit: the stall
// then names no limit, rather than the wrong one.
func TestCodexLimitIgnoresAnOlderOrRoomySnapshot(t *testing.T) {
	r := loadRules(t)
	path := "/c/sessions/2026/11/20/rollout-2026-11-20T10-00-00-0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000.jsonl"
	limitErr := rolloutLine("00:09", "event_msg", `{"type":"error","message":"You've hit your usage limit.","codex_error_info":"usage_limit_exceeded"}`)
	for name, lines := range map[string][]string{
		"snapshot from the turn before": {
			rolloutLine("00:00", "event_msg", `{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":100.0,"window_minutes":300,"resets_at":1795000000},"secondary":null}}`),
			rolloutLine("00:05", "event_msg", `{"type":"task_started"}`),
			limitErr,
		},
		"snapshot with room left": {
			rolloutLine("00:00", "event_msg", `{"type":"task_started"}`),
			rolloutLine("00:01", "event_msg", `{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":40.0,"window_minutes":300,"resets_at":1795000000},"secondary":null}}`),
			limitErr,
		},
	} {
		st := &CodexState{SessionID: "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000"}
		evs := ParseCodex(r, path, rawLines(lines...), st, time.UTC)
		var lim *core.Event
		for i := range evs {
			if evs[i].Kind == core.KindUsageLimit {
				lim = &evs[i]
			}
		}
		if lim == nil || lim.LimitID != "" {
			t.Errorf("%s: %+v", name, lim)
		}
	}
}

// A turn that ends without an error message (finished, or interrupted)
// leaves no request waiting for an answer.
func TestCodexTurnEndClearsTheRequest(t *testing.T) {
	r := loadRules(t)
	path := "/c/sessions/2026/11/20/rollout-2026-11-20T10-00-00-0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000.jsonl"
	for _, end := range []string{`{"type":"turn_aborted"}`, `{"type":"task_complete","last_agent_message":"done"}`} {
		st := &CodexState{SessionID: "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000"}
		ParseCodex(r, path, rawLines(
			rolloutLine("00:00", "event_msg", `{"type":"task_started"}`),
			rolloutLine("00:02", "response_item", `{"type":"reasoning"}`),
			rolloutLine("00:03", "event_msg", end),
		), st, time.UTC)
		if !st.Trigger.IsZero() || !st.InFlight.IsZero() {
			t.Errorf("%s: state %+v", end, st)
		}
	}
}

// The usual order in one turn: the turn starts, the 429's snapshot shows the
// limit full, the error. The snapshot names the limit and its reset. One
// from the turn before gives neither.
func TestCodexLimitFromThisTurnsSnapshot(t *testing.T) {
	r := loadRules(t)
	path := "/c/sessions/2026/11/20/rollout-2026-11-20T10-00-00-0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000.jsonl"
	full := `{"type":"token_count","rate_limits":{"limit_id":"codex","primary":{"used_percent":100.0,"window_minutes":300,"resets_at":1795186800},"secondary":null}}`
	limitErr := `{"type":"error","message":"You've hit your usage limit.","codex_error_info":"usage_limit_exceeded"}`
	limitOf := func(lines ...string) *core.Event {
		st := &CodexState{SessionID: "0199aaaa-bbbb-7ccc-8ddd-eeeeffff0000"}
		for _, ev := range ParseCodex(r, path, rawLines(lines...), st, time.UTC) {
			if ev.Kind == core.KindUsageLimit {
				return &ev
			}
		}
		return nil
	}
	ev := limitOf(rolloutLine("00:00", "event_msg", `{"type":"task_started"}`), rolloutLine("00:01", "event_msg", full), rolloutLine("00:02", "event_msg", limitErr))
	if ev == nil || ev.LimitID != "codex" || !ev.ResetAt.Equal(time.Unix(1795186800, 0)) {
		t.Fatalf("this turn's snapshot: %+v", ev)
	}
	ev = limitOf(rolloutLine("00:00", "event_msg", full), rolloutLine("00:01", "event_msg", `{"type":"task_started"}`), rolloutLine("00:02", "event_msg", limitErr))
	if ev == nil || ev.LimitID != "" || ev.ResetAt.Equal(time.Unix(1795186800, 0)) {
		t.Fatalf("the turn before's snapshot was used: %+v", ev)
	}
}
