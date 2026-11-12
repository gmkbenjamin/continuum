package sources

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
)

func claudeLines(t *testing.T, raw ...string) (string, []Line) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s1.jsonl")
	os.WriteFile(path, []byte(strings.Join(raw, "\n")+"\n"), 0o600)
	return path, ReadFrom(path, 0)
}

const limitLine = `{"type":"assistant","isApiErrorMessage":true,"error":"rate_limit","sessionId":"s1","cwd":"/w","entrypoint":"%s","timestamp":"2026-11-10T22:03:00.000Z","message":{"role":"assistant","content":[{"type":"text","text":"You've hit your limit · resets 10am (Australia/Sydney)"}]}}`

// Running /usage or /status, or a `!` shell command, after a limit doesn't
// continue the session: it must not cancel the resume.
func TestLocalCommandsAfterALimitAreNotActivity(t *testing.T) {
	r := loadRules(t)
	path, lines := claudeLines(t,
		strings.ReplaceAll(limitLine, "%s", "cli"),
		`{"type":"user","isMeta":true,"sessionId":"s1","timestamp":"2026-11-10T22:03:05.000Z","message":{"role":"user","content":"<local-command-caveat>Caveat: ...</local-command-caveat>"}}`,
		`{"type":"user","sessionId":"s1","timestamp":"2026-11-10T22:03:06.000Z","message":{"role":"user","content":"<command-name>/usage</command-name>"}}`,
		`{"type":"user","sessionId":"s1","timestamp":"2026-11-10T22:03:07.000Z","message":{"role":"user","content":"<local-command-stdout>Usage: 100%</local-command-stdout>"}}`,
		`{"type":"user","sessionId":"s1","timestamp":"2026-11-10T22:03:08.000Z","message":{"role":"user","content":"<bash-input>ls</bash-input>"}}`,
	)
	evs := ParseClaude(r, path, lines, time.UTC)
	if len(evs) != 1 || evs[0].Kind != core.KindUsageLimit {
		t.Fatalf("want only the limit, got %+v", evs)
	}
	// A real prompt afterwards is activity.
	path, lines = claudeLines(t,
		strings.ReplaceAll(limitLine, "%s", "cli"),
		`{"type":"user","sessionId":"s1","timestamp":"2026-11-10T22:04:00.000Z","message":{"role":"user","content":"keep going"}}`,
	)
	evs = ParseClaude(r, path, lines, time.UTC)
	if len(evs) != 2 || evs[1].Kind != core.KindActivity {
		t.Fatalf("want the limit then activity, got %+v", evs)
	}
}

// `claude -p` and Agent SDK scripts are never resumed.
func TestPrintModeTranscriptsAreIgnored(t *testing.T) {
	r := loadRules(t)
	for _, ep := range []string{"sdk-cli", "sdk-ts", "sdk-py"} {
		path, lines := claudeLines(t, strings.ReplaceAll(limitLine, "%s", ep))
		if evs := ParseClaude(r, path, lines, time.UTC); len(evs) != 0 {
			t.Fatalf("%s: got %+v", ep, evs)
		}
	}
}

// `codex exec` runs and subagent threads are never resumed.
func TestCodexExecAndSubagentRolloutsAreIgnored(t *testing.T) {
	r := loadRules(t)
	errLine := `{"timestamp":"2026-11-04T09:25:51.350Z","type":"event_msg","payload":{"type":"task_complete","error":{"message":"You've hit your usage limit. Try again at 5:53 PM.","codex_error_info":"usage_limit_exceeded"}}}`
	for name, meta := range map[string]string{
		"exec":     `{"timestamp":"2026-11-04T09:25:00.000Z","type":"session_meta","payload":{"id":"0000000a-0000-7000-8000-000000000001","cwd":"/w","originator":"codex_exec","source":"exec"}}`,
		"subagent": `{"timestamp":"2026-11-04T09:25:00.000Z","type":"session_meta","payload":{"id":"0000000a-0000-7000-8000-000000000002","cwd":"/w","originator":"codex-tui","source":{"subagent":{"other":"guardian"}}}}`,
	} {
		path := filepath.Join(t.TempDir(), "rollout-x-"+name+".jsonl")
		os.WriteFile(path, []byte(meta+"\n"+errLine+"\n"), 0o600)
		st := &CodexState{}
		if evs := ParseCodex(r, path, ReadFrom(path, 0), st, time.UTC); len(evs) != 0 || !st.Headless {
			t.Fatalf("%s: got %+v (headless=%v)", name, evs, st.Headless)
		}
	}
}
