package hooks

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
	for _, want := range []string{`"model": "opus"`, `git log && git status`, `echo <hi>`, `rex __internal session-start`, bin + ` hook claude`, `"StopFailure"`} {
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
	if _, err := Install(tg, `C:\Program Files\continuum.exe`, false); err != nil {
		t.Fatal(err)
	}
	s := mustRead(t, path)
	if !strings.Contains(s, `"\"C:\\Program Files\\continuum.exe\" hook codex"`) {
		t.Fatalf("path with spaces not quoted:\n%s", s)
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
