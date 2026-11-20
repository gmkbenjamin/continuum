package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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

// ourHookText is how Continuum's hook command for Claude Code reads in a
// settings file on this OS.
func ourHookText(bin string) string {
	if runtime.GOOS == "windows" {
		return "hook claude"
	}
	return `\"` + bin + `\" hook claude 2>/dev/null || true`
}

func TestStopFailureEvent(t *testing.T) {
	r := loadRules(t)
	now := time.Date(2026, 11, 11, 8, 0, 0, 0, time.UTC)
	in := Input{
		HookEventName: "StopFailure", SessionID: "sess-1", Cwd: "/work", TranscriptPath: "/nope.jsonl",
		Error:                []byte(`"rate_limit"`),
		LastAssistantMessage: "You've hit your session limit · resets 10am (UTC)",
	}
	env := map[string]string{"TMUX": "/tmp/tmux-501/default,123,0", "TMUX_PANE": "%7", "CLAUDE_PID": "4242", "CLAUDE_CODE_ENTRYPOINT": "cli"}
	ev, ok := BuildEvent(r, "claude", in, env, now)
	if !ok {
		t.Fatal("event dropped")
	}
	if ev.Kind != core.KindUsageLimit || ev.PID != 4242 || ev.Entrypoint != "cli" {
		t.Fatalf("bad event %+v", ev)
	}
	if ev.Location == nil || ev.Location.Kind != "tmux" || ev.Location.Target != "%7" || ev.Location.Socket != "/tmp/tmux-501/default" {
		t.Fatalf("bad location %+v", ev.Location)
	}
	if !ev.ResetAt.Equal(time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("reset %v", ev.ResetAt)
	}
}

func TestStopFailureCodesWithoutText(t *testing.T) {
	r := loadRules(t)
	for code, want := range map[string]string{"overloaded": core.KindTransient, "server_error": core.KindTransient, "rate_limit": core.KindUsageLimit} {
		ev, ok := BuildEvent(r, "claude", Input{HookEventName: "StopFailure", SessionID: "s", Error: []byte(`"` + code + `"`)}, nil, time.Now())
		if !ok || ev.Kind != want {
			t.Errorf("%s: got %v %q", code, ok, ev.Kind)
		}
	}
	for _, code := range []string{"authentication_failed", "invalid_request", "max_output_tokens"} {
		if _, ok := BuildEvent(r, "claude", Input{HookEventName: "StopFailure", SessionID: "s", Error: []byte(`"` + code + `"`)}, nil, time.Now()); ok {
			t.Errorf("%s should not produce an event", code)
		}
	}
	ev, _ := BuildEvent(r, "claude", Input{HookEventName: "StopFailure", SessionID: "s", Error: []byte(`"billing_error"`), LastAssistantMessage: "You're out of usage credits · add funds to continue"}, nil, time.Now())
	if ev.Kind != core.KindNeverResume {
		t.Errorf("out of credits: %q", ev.Kind)
	}
}

func TestLocationPreference(t *testing.T) {
	r := loadRules(t)
	// tmux running inside Rex: tmux is the inner multiplexer.
	env := map[string]string{"REX_BLOCK": "block:1", "REX_SERVER": "unix:///x.sock", "TMUX": "/s,1,0", "TMUX_PANE": "%1"}
	ev, _ := BuildEvent(r, "codex", Input{HookEventName: "SessionStart", SessionID: "s"}, env, time.Now())
	if ev.Location.Kind != "tmux" {
		t.Fatalf("got %+v", ev.Location)
	}
	// Continuum's wrapper wins over everything.
	env["CONTINUUM_WRAPPER_ID"] = "w1"
	ev, _ = BuildEvent(r, "codex", Input{HookEventName: "SessionStart", SessionID: "s"}, env, time.Now())
	if ev.Location.Kind != "wrapper" || ev.Location.Target != "w1" {
		t.Fatalf("got %+v", ev.Location)
	}
	// Zellij pane ids get their terminal_ prefix.
	ev, _ = BuildEvent(r, "codex", Input{HookEventName: "SessionStart", SessionID: "s"}, map[string]string{"ZELLIJ_PANE_ID": "3", "ZELLIJ_SESSION_NAME": "dev"}, time.Now())
	if ev.Location.Kind != "zellij" || ev.Location.Target != "terminal_3" || ev.Location.Socket != "dev" {
		t.Fatalf("got %+v", ev.Location)
	}
	// screen needs STY.
	ev, _ = BuildEvent(r, "codex", Input{HookEventName: "SessionStart", SessionID: "s"}, map[string]string{"WINDOW": "0"}, time.Now())
	if ev.Location != nil {
		t.Fatalf("WINDOW alone is not screen: %+v", ev.Location)
	}
}

const existing = `{
  "model": "opus",
  "permissions": {"allow": ["Bash(git log && git status)"]},
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "rex __internal session-start --agent claude || true", "timeout": 5}]}
    ]
  },
  "statusLine": {"type": "command", "command": "echo <hi>"}
}
`

func TestInstallUninstallKeepsEverythingElse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	os.WriteFile(path, []byte(existing), 0o644)
	tg := Target{Harness: "claude", Path: path, Events: []string{"SessionStart", "StopFailure"}, Timeout: 10}
	bin := "/Users/me/.continuum/bin/continuum"

	changed, err := Install(tg, bin, false)
	if err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	for _, want := range []string{`"model": "opus"`, `git log && git status`, `echo <hi>`, `rex __internal session-start`, ourHookText(bin), `"StopFailure"`} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if strings.Index(s, `"model"`) > strings.Index(s, `"statusLine"`) {
		t.Error("key order changed")
	}
	if ok, _ := Installed(tg); !ok {
		t.Error("Installed() = false after install")
	}
	backups, _ := filepath.Glob(path + ".continuum-backup-*")
	if len(backups) != 1 {
		t.Errorf("want 1 backup, got %v", backups)
	}

	// Idempotent.
	if changed, err := Install(tg, bin, false); err != nil || changed {
		t.Fatalf("second install changed=%v err=%v", changed, err)
	}
	if n := strings.Count(mustRead(t, path), "hook claude"); n != 2 {
		t.Fatalf("want 2 hook entries, got %d", n)
	}

	// Uninstall returns the file to its original content (modulo formatting).
	if changed, err := Uninstall(tg, false); err != nil || !changed {
		t.Fatalf("uninstall: %v %v", changed, err)
	}
	after := mustRead(t, path)
	if strings.Contains(after, "continuum") || !strings.Contains(after, "rex __internal") || strings.Contains(after, "StopFailure") {
		t.Fatalf("uninstall result:\n%s", after)
	}
}

func TestInstallCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex", "hooks.json")
	os.MkdirAll(filepath.Dir(path), 0o700)
	tg := Target{Harness: "codex", Path: path, Events: []string{"SessionStart", "Stop"}, Timeout: 10}
	bin := `/Users/me/My Tools/it's/continuum`
	if _, err := Install(tg, bin, false); err != nil {
		t.Fatal(err)
	}
	if st, _ := Status(tg, bin); st != StateCurrent {
		t.Fatalf("status %s:\n%s", st, mustRead(t, path))
	}
	// The command must run the binary with spaces and quotes intact.
	cmd := Command(bin, "codex")
	if !isOurs(cmd) {
		t.Fatalf("own command not recognised: %s", cmd)
	}
}

// If Continuum is deleted without uninstalling, the hook must stay silent and
// succeed, so the agent never shows a hook error.
func TestHookIsHarmlessWhenContinuumIsGone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix only")
	}
	cmd := Command(filepath.Join(t.TempDir(), "gone", "continuum"), "claude")
	out, err := exec.Command("/bin/sh", "-c", cmd).CombinedOutput()
	if err != nil || len(out) != 0 {
		t.Fatalf("err=%v output=%q", err, out)
	}
}

func TestOldHooksAreUpgradedAndStraysRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	// An older Continuum: bare command, another path, and an event this
	// version no longer uses. Plus the user's own hooks, which must stay.
	old := `{"hooks": {
  "SessionStart": [{"hooks": [{"type": "command", "command": "/old/place/continuum hook claude"}]}],
  "Notification": [{"hooks": [{"type": "command", "command": "/old/place/continuum hook claude"}, {"type": "command", "command": "say done"}]}],
  "Stop": [{"hooks": [{"type": "command", "command": "/Users/me/continuum-notes/bin/sync hook stop"}]}]
}}`
	os.WriteFile(path, []byte(old), 0o600)
	tg := Target{Harness: "claude", Path: path, Events: []string{"SessionStart", "Stop"}, Timeout: 10}
	bin := "/new/continuum"
	if st, _ := Status(tg, bin); st != StateOutdated {
		t.Fatalf("status before: %s", st)
	}
	if _, err := Install(tg, bin, false); err != nil {
		t.Fatal(err)
	}
	s := mustRead(t, path)
	if st, why := Status(tg, bin); st != StateCurrent {
		t.Fatalf("status after: %s (%s)\n%s", st, why, s)
	}
	for _, gone := range []string{"/old/place/continuum"} {
		if strings.Contains(s, gone) {
			t.Fatalf("old hook left:\n%s", s)
		}
	}
	for _, kept := range []string{"say done", "continuum-notes/bin/sync hook stop", `"Notification"`} {
		if !strings.Contains(s, kept) {
			t.Fatalf("user hook %q removed:\n%s", kept, s)
		}
	}
	// Uninstall leaves only the user's hooks.
	Uninstall(tg, false)
	s = mustRead(t, path)
	if strings.Contains(s, "/new/continuum") || !strings.Contains(s, "say done") || !strings.Contains(s, "continuum-notes") {
		t.Fatalf("after uninstall:\n%s", s)
	}
	if st, _ := Status(tg, bin); st != StateMissing {
		t.Fatalf("status after uninstall: %s", st)
	}
}

func TestIsOursIsPrecise(t *testing.T) {
	ours := []string{
		"/x/continuum hook claude",
		`"C:\\Program Files\\continuum.exe" hook codex`,
		`/bin/sh -c '"/x/.continuum/bin/continuum" hook claude 2>/dev/null || true'`,
	}
	theirs := []string{
		"/Users/me/continuum-notes/bin/sync hook stop",
		"/Users/me/Documents/continuum/scripts/notify.sh",
		"rex __internal session-start --agent codex",
		"mycontinuum hook x",
	}
	for _, c := range ours {
		if !isOurs(c) {
			t.Errorf("not recognised: %s", c)
		}
	}
	for _, c := range theirs {
		if isOurs(c) {
			t.Errorf("someone else's hook taken for ours: %s", c)
		}
	}
}

func TestRefusesBrokenJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(path, []byte(`{"model": "opus",}`), 0o600)
	if _, err := Install(Target{Harness: "claude", Path: path, Events: []string{"Stop"}}, "/x/continuum", false); err == nil {
		t.Fatal("expected refusal")
	}
	if mustRead(t, path) != `{"model": "opus",}` {
		t.Fatal("file was modified")
	}
}

func mustRead(t *testing.T, p string) string {
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestUninstallRestoresExactBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hooks.json")
	// Compact, unusual formatting the pretty-printer would never produce.
	orig := "{\"hooks\": {\"SessionStart\": [{\"hooks\": [{\"type\": \"command\", \"command\": \"rex x\", \"timeout\": 5}]}]}}\n"
	os.WriteFile(path, []byte(orig), 0o644)
	tg := Target{Harness: "codex", Path: path, Events: []string{"SessionStart", "Stop"}, Timeout: 10}
	Install(tg, "/a/continuum", false)
	Install(tg, "/b/continuum", false) // an update: a second backup
	Uninstall(tg, false)
	if got := mustRead(t, path); got != orig {
		t.Fatalf("not restored byte for byte:\n%q\nwant\n%q", got, orig)
	}
}

func TestUninstallRemovesAFileItCreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	tg := Target{Harness: "codex", Path: path, Events: []string{"SessionStart", "Stop"}, Timeout: 10}
	Install(tg, "/a/continuum", false)
	Uninstall(tg, false)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file Continuum created is still there: %v", err)
	}
}

func TestUninstallRemovesCreatedFileAfterUpdateBackups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	tg := Target{Harness: "codex", Path: path, Events: []string{"SessionStart", "Stop"}, Timeout: 10}
	if _, err := Install(tg, "/a/continuum", false); err != nil {
		t.Fatal(err)
	}
	// Simulating `continuum update` creates a backup containing only Continuum's hooks.
	if _, err := Install(tg, "/b/continuum", false); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(tg, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file Continuum created was left behind after update+uninstall: %v", err)
	}

	// Pre-existing empty `{}\n` is still restored, not removed.
	emptyPath := filepath.Join(t.TempDir(), "existing-empty.json")
	os.WriteFile(emptyPath, []byte("{}\n"), 0o600)
	tgEmpty := Target{Harness: "codex", Path: emptyPath, Events: []string{"SessionStart", "Stop"}, Timeout: 10}
	if _, err := Install(tgEmpty, "/a/continuum", false); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(tgEmpty, false); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, emptyPath); got != "{}\n" {
		t.Fatalf("pre-existing empty object = %q, want %q", got, "{}\n")
	}
}

func TestInstallCleansPartialLegacyEventGroup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	orig := `{"hooks": {
  "Notification": [{"hooks": [{"type": "command", "command": "/old/continuum hook claude"}, {"type": "command", "command": "say hi"}]}]
}}`
	os.WriteFile(path, []byte(orig), 0o600)
	tg := Target{Harness: "claude", Path: path, Events: []string{"Stop"}, Timeout: 10}
	changed, err := Install(tg, "/new/continuum", false)
	if err != nil || !changed {
		t.Fatalf("Install changed=%v err=%v", changed, err)
	}
	got := mustRead(t, path)
	if strings.Contains(got, "/old/continuum") || !strings.Contains(got, "say hi") {
		t.Fatalf("unexpected Notification group after upgrade:\n%s", got)
	}
}

// Grok Build, Muse Code and the Cursor CLI: a turn that ended normally was
// answered on the plan; Claude Code's hook leaves that to its transcript.
func TestStopHookAnswers(t *testing.T) {
	r, err := rules.Load("")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	for harness, want := range map[string]bool{"grok": true, "muse": true, "cursor": true, "claude": false, "codex": false} {
		in := Input{HookEventName: "Stop", SessionID: "s1", Cwd: "/w", LastAssistantMessage: "All done."}
		if harness == "cursor" {
			in = Input{HookEventName: "afterAgentResponse", ConversationID: "s1", Text: "All done."}
		}
		ev, ok := BuildEvent(r, harness, in, nil, now)
		if !ok || ev.Kind != core.KindActivity {
			t.Fatalf("%s: %+v", harness, ev)
		}
		if ev.Answered != want {
			t.Errorf("%s: answered %v, want %v", harness, ev.Answered, want)
		}
		if !ev.RequestAt.IsZero() {
			t.Errorf("%s: the hook can't know when the request went out: %v", harness, ev.RequestAt)
		}
	}
}

func TestPromptSubmitStartsATurn(t *testing.T) {
	r := loadRules(t)
	now := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	for harness, in := range map[string]Input{
		"grok":   {HookEventName: "UserPromptSubmit", SessionID: "s1", Cwd: "/w"},
		"muse":   {HookEventName: "UserPromptSubmit", SessionID: "s1", Cwd: "/w"},
		"cursor": {HookEventName: "beforeSubmitPrompt", ConversationID: "s1", Model: "claude-fable-5-2"},
	} {
		ev, ok := BuildEvent(r, harness, in, nil, now)
		if !ok || ev.Kind != core.KindActivity || !ev.TurnStart {
			t.Fatalf("%s: %+v", harness, ev)
		}
		if harness == "cursor" && ev.Model != "claude-fable-5-2" {
			t.Errorf("cursor: model %q", ev.Model)
		}
	}
	ev, _ := BuildEvent(r, "cursor", Input{HookEventName: "afterAgentResponse", ConversationID: "s1", Text: "Done.", Model: "auto"}, nil, now)
	if !ev.Answered || ev.Model != "auto" {
		t.Errorf("cursor answer: %+v", ev)
	}
}

func TestInstallNamesHooks(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		t    Target
		want bool
	}{
		{Target{Harness: "codex", Path: filepath.Join(dir, "codex.json"), Events: []string{"SessionStart", "Stop"}, Timeout: 10, StatusMessage: HookStatusMessage}, true},
		{Target{Harness: "cursor", Path: filepath.Join(dir, "cursor.json"), Events: []string{"stop"}, Flat: true}, false},
	} {
		if _, err := Install(tc.t, "/x/continuum", false); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(tc.t.Path)
		if got := strings.Contains(string(data), `"statusMessage": "`+HookStatusMessage+`"`); got != tc.want {
			t.Errorf("%s: status message %v, want %v:\n%s", tc.t.Harness, got, tc.want, data)
		}
		if st, _ := Status(tc.t, "/x/continuum"); st != StateCurrent {
			t.Errorf("%s: status %s after install", tc.t.Harness, st)
		}
	}
	// A hook from before Continuum named its hooks is outdated, so the
	// daemon rewrites it.
	old := Target{Harness: "codex", Path: filepath.Join(dir, "old.json"), Events: []string{"SessionStart", "Stop"}, Timeout: 10}
	if _, err := Install(old, "/x/continuum", false); err != nil {
		t.Fatal(err)
	}
	old.StatusMessage = HookStatusMessage
	if st, _ := Status(old, "/x/continuum"); st != StateOutdated {
		t.Errorf("unnamed hooks: status %s, want outdated", st)
	}
	if changed, err := Install(old, "/x/continuum", false); err != nil || !changed {
		t.Errorf("reinstall: changed %v, %v", changed, err)
	}
	if st, _ := Status(old, "/x/continuum"); st != StateCurrent {
		t.Errorf("after reinstall: %s", st)
	}
}
