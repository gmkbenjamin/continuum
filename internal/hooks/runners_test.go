package hooks

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/proc"
)

// A turn that ends normally is activity, even when its reply talks about
// limits or API errors; such a reply is reported as a reply, never a stall.
func TestStopIsNeverAStall(t *testing.T) {
	r := loadRules(t)
	env := map[string]string{"TMUX_PANE": "%3", "TMUX": "/tmp/tmux-1/default,1,0"}
	for _, msg := range []string{
		"Done. The client now retries with backoff when the server answers API Error: 503.",
		"I added handling so the CLI prints \"usage limit reached\" when the quota API returns 429.",
		"Fixed the parser for messages like \"You've hit your session limit · resets 10am\".",
	} {
		ev, ok := BuildEvent(r, "claude", Input{HookEventName: "Stop", SessionID: "s1", LastAssistantMessage: msg}, env, time.Now())
		if !ok || ev.Kind != core.KindReply || ev.Message != msg {
			t.Errorf("%q: ok=%v kind=%s", msg[:30], ok, ev.Kind)
		}
	}
	ev, ok := BuildEvent(r, "claude", Input{HookEventName: "Stop", SessionID: "s1", LastAssistantMessage: "All tests pass."}, env, time.Now())
	if !ok || ev.Kind != core.KindActivity {
		t.Errorf("plain reply: ok=%v kind=%s", ok, ev.Kind)
	}
}

// The wrapper reads the last 40 lines of the screen, so a limit mentioned
// anywhere in a long reply must be recognised as part of the reply, not only
// in its last few lines.
func TestLongReplyMentioningALimitIsAReply(t *testing.T) {
	r := loadRules(t)
	lines := []string{"Summary of the change:", "The CLI now prints \"usage limit reached\" when the quota API returns 429."}
	for i := 0; i < 20; i++ {
		lines = append(lines, "- updated step "+strings.Repeat("x", i%7))
	}
	msg := strings.Join(lines, "\n")
	ev, ok := BuildEvent(r, "claude", Input{HookEventName: "Stop", SessionID: "s1", LastAssistantMessage: msg}, nil, time.Now())
	if !ok || ev.Kind != core.KindReply || !strings.Contains(ev.Message, "usage limit reached") {
		t.Fatalf("ok=%v kind=%s", ok, ev.Kind)
	}
}

// claude -p and SDK scripts inherit the pane of the session that started
// them; they must never schedule anything there.
func TestPrintModeHooksAreIgnored(t *testing.T) {
	r := loadRules(t)
	env := map[string]string{"CONTINUUM_WRAPPER_ID": "parentwrap", "CLAUDE_CODE_ENTRYPOINT": "sdk-cli"}
	in := Input{HookEventName: "StopFailure", SessionID: "child", Error: []byte(`"server_error"`), LastAssistantMessage: "API Error: 500 Internal server error"}
	if ev, ok := BuildEvent(r, "claude", in, env, time.Now()); ok {
		t.Fatalf("print-mode run reported: %+v", ev)
	}
	env["CLAUDE_CODE_ENTRYPOINT"] = "cli"
	if ev, ok := BuildEvent(r, "claude", in, env, time.Now()); !ok || ev.Kind != core.KindTransient {
		t.Fatalf("interactive run: ok=%v %+v", ok, ev)
	}
}

// Grok Build runs Claude Code's hooks with its own variables set: those runs
// are Grok's, and its limit wording is understood.
func TestGrokRunsClaudeHooks(t *testing.T) {
	r := loadRules(t)
	if got := r.HookHarness(map[string]string{"GROK_SESSION_ID": "019a", "GROK_HOOK_EVENT": "stop_failure"}); got != "grok" {
		t.Fatalf("hook harness %q", got)
	}
	if got := r.HookHarness(map[string]string{"TMUX_PANE": "%1"}); got != "" {
		t.Fatalf("plain env taken for %q", got)
	}
	cases := map[string]string{
		"You've hit the rate limit for your plan. Upgrade your account or try again later.":                                 core.KindUsageLimit,
		"You've reached your free Grok Build usage limit for now. Get SuperGrok for much higher limits, or try again later": core.KindUsageLimit,
		"Grok is temporarily overloaded. Please try again in a moment. (HTTP 503)":                                          core.KindTransient,
		"Something went wrong on the server (HTTP 500)":                                                                     core.KindTransient,
	}
	for msg, want := range cases {
		ev, ok := BuildEvent(r, "grok", Input{HookEventName: "StopFailure", SessionID: "019a", Error: []byte(`"rate_limit"`), LastAssistantMessage: msg}, nil, time.Now())
		if !ok || ev.Kind != want {
			t.Errorf("%q: ok=%v kind=%s want %s", msg, ok, ev.Kind, want)
		}
	}
	ev, ok := BuildEvent(r, "grok", Input{HookEventName: "StopFailure", SessionID: "019a", Error: []byte(`"authentication_failed"`), LastAssistantMessage: "Your team sign-in was rejected. Run `grok login` to sign in again."}, nil, time.Now())
	if !ok || ev.Kind != core.KindNeverResume {
		t.Errorf("sign-in error: ok=%v kind=%s", ok, ev.Kind)
	}
}

// An agent started by another agent's tool (a `claude -p` from a Bash tool)
// is nested; an agent in a terminal, a desktop app or a helper of its own
// is not.
func TestNestedAgents(t *testing.T) {
	r := loadRules(t)
	tree := func(chain ...string) map[int]proc.Node {
		m := map[int]proc.Node{}
		for i, name := range chain { // chain[0] is the hook's parent, pid 100
			m[100+i] = proc.Node{PPID: 101 + i, Name: name}
		}
		m[100+len(chain)] = proc.Node{PPID: 1, Name: "launchd"}
		return m
	}
	cases := []struct {
		chain  []string
		nested bool
		agent  string
	}{
		{[]string{"sh", "claude", "zsh", "tmux"}, false, "claude"},
		{[]string{"sh", "claude", "zsh", "claude", "zsh"}, true, "claude"},
		{[]string{"sh", "codex", "bash", "claude", "login"}, true, "codex"},
		{[]string{"sh", "claude", "Claude"}, false, "claude"},                                            // Claude Desktop runs it
		{[]string{"sh", "grok-1.0.77-macos-aarch64", "grok-1.0.77-macos-aarch64", "zsh"}, false, "grok"}, // a helper of Grok's own
		{[]string{"sh", "Cursor Helper (Plugin)", "Cursor"}, false, ""},
	}
	for _, c := range cases {
		tr := tree(c.chain...)
		if got := nested(r, tr, 100); got != c.nested {
			t.Errorf("%v: nested=%v want %v", c.chain, got, c.nested)
		}
		if got := nearestAgent(r, tr, 100); got != c.agent {
			t.Errorf("%v: agent %q want %q", c.chain, got, c.agent)
		}
	}
}

// Dotfile managers keep settings files as links into a repository: the link
// must stay a link, and the repository's file gets the hooks.
func TestSymlinkedSettingsStayLinked(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "dotfiles", "settings.json")
	os.MkdirAll(filepath.Dir(real), 0o700)
	os.WriteFile(real, []byte("{\n    \"model\": \"opus\"\n}\n"), 0o644)
	link := filepath.Join(dir, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(link), 0o700)
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	tg := Target{Harness: "claude", Path: link, Events: []string{"SessionStart", "Stop"}, Timeout: 10}
	if _, err := Install(tg, "/x/.continuum/bin/continuum", false); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("install replaced the link with a file")
	}
	if data, _ := os.ReadFile(real); !strings.Contains(string(data), "hook claude") {
		t.Fatal("the linked file didn't get the hooks")
	}
	if fi, _ := os.Stat(real); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o644 {
		t.Fatalf("permissions changed to %v", fi.Mode().Perm())
	}
	if _, err := Uninstall(tg, false); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("uninstall replaced the link with a file")
	}
}

// One backup per version of a file, however often hooks are re-added.
func TestBackupsAreNotRepeated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	for i := 0; i < 3; i++ {
		if err := SaveBackup(path, []byte(`{"a":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(Backups(path)); n != 1 {
		t.Fatalf("%d backups", n)
	}
}

// On Windows a parent id can be stale and reused, so the tree can loop; the
// walk must still end (a hook has to be quick).
func TestNestedEndsOnALoopInTheTree(t *testing.T) {
	r := loadRules(t)
	tree := map[int]proc.Node{
		100: {PPID: 200, Name: "sh.exe"},
		200: {PPID: 300, Name: "claude.exe"},
		300: {PPID: 200, Name: "claude.exe"},
	}
	done := make(chan bool, 1)
	go func() { done <- nested(r, tree, 100) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("nested() never returned on a loop in the process tree")
	}
}
