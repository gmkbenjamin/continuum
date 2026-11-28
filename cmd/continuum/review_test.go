package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/hooks"
	"github.com/continuum-dev/continuum/internal/messages"
	"github.com/continuum-dev/continuum/internal/rules"
)

func testShellRCPath(home string) string {
	if runtime.GOOS == "windows" {
		p := filepath.Join(home, "Documents", "PowerShell", "Microsoft.PowerShell_profile.ps1")
		_ = os.MkdirAll(filepath.Dir(p), 0o700)
		return p
	}
	return filepath.Join(home, ".zshrc")
}

// Install then uninstall gives a shell startup file back byte for byte, even
// one without a final newline.
func TestShellHookRoundTripIsExact(t *testing.T) {
	for _, orig := range []string{"export A=1", "export A=1\n\n", "a\n\n\n"} {
		home := sandboxHome(t)
		rc := testShellRCPath(home)
		os.WriteFile(rc, []byte(orig), 0o600)
		installShell("/x/continuum", false)
		if data, _ := os.ReadFile(rc); !strings.Contains(string(data), blockStart) {
			t.Fatalf("%q: not installed", orig)
		}
		uninstallShell(false)
		if data, _ := os.ReadFile(rc); string(data) != orig {
			t.Errorf("%q came back as %q", orig, data)
		}
	}
}

// A start marker without its end marker: Continuum can't tell where its block
// ends, so it leaves the file alone rather than delete the user's lines.
func TestDamagedShellBlockIsLeftAlone(t *testing.T) {
	home := sandboxHome(t)
	rc := testShellRCPath(home)
	content := "a\n" + blockStart + "\nalias ll='ls -l'\n# more of mine\n"
	os.WriteFile(rc, []byte(content), 0o600)
	out := strings.Join(installShell("/x/continuum", false), "\n")
	if data, _ := os.ReadFile(rc); string(data) != content || !strings.Contains(out, "damaged") {
		t.Fatalf("install changed it: %q\n%s", data, out)
	}
	out = strings.Join(uninstallShell(false), "\n")
	if data, _ := os.ReadFile(rc); string(data) != content || !strings.Contains(out, "damaged") {
		t.Fatalf("uninstall changed it: %q\n%s", data, out)
	}
}

// --purge deletes Continuum's own files only: a CONTINUUM_HOME pointed at a
// folder with other things in it keeps them.
func TestPurgeDeletesOnlyContinuumsFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "shared")
	os.MkdirAll(filepath.Join(dir, "spool"), 0o700)
	os.WriteFile(filepath.Join(dir, "config.json"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(dir, "jobs.json"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(dir, "my-notes.txt"), []byte("mine"), 0o600)
	if err := purgeHome(dir); err == nil {
		t.Fatal("no note about the files left in place")
	}
	for _, gone := range []string{"spool", "config.json", "jobs.json"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); err == nil {
			t.Errorf("%s not deleted", gone)
		}
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "my-notes.txt")); string(data) != "mine" {
		t.Fatal("deleted a file that isn't Continuum's")
	}
	// Only Continuum's files: the folder goes too.
	os.Remove(filepath.Join(dir, "my-notes.txt"))
	os.WriteFile(filepath.Join(dir, "history.jsonl"), nil, 0o600)
	if err := purgeHome(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("folder left behind")
	}
	if err := purgeHome(os.Getenv("HOME")); err == nil {
		t.Fatal("agreed to purge the home folder")
	}
}

// WHERE names the multiplexer or terminal app, and says when a session runs
// through Continuum's wrapper.
func TestWhereNamesTheHost(t *testing.T) {
	r, _ := rules.Load("")
	rex := map[string]string{"REX_BLOCK": "block:1", "TERM_PROGRAM": "rex"}
	if got := hostFromEnv(r, rex); got != "Rex" {
		t.Fatalf("rex env: %q", got)
	}
	for env, want := range map[string]string{"Apple_Terminal": "Terminal", "iTerm.app": "iTerm2", "ghostty": "Ghostty", "vscode": "VS Code"} {
		if got := hostFromEnv(r, map[string]string{"TERM_PROGRAM": env}); got != want {
			t.Errorf("%s: %q", env, got)
		}
	}
	if got := hostFromEnv(r, map[string]string{"TERM_PROGRAM": "vscode", "CURSOR_TRACE_ID": "x"}); got != "Cursor" {
		t.Errorf("cursor: %q", got)
	}
	wrapped := knownSession{Harness: "claude", Open: true, Location: &core.Location{Kind: "wrapper", Target: "w1"}}
	if got := sessionWhere(wrapped, r); got != "terminal (via Continuum)" {
		t.Errorf("unknown host: %q", got)
	}
	wrapped.Host = "Rex"
	if got := sessionWhere(wrapped, r); got != "Rex (via Continuum)" {
		t.Errorf("rex: %q", got)
	}
	if got := sessionWhere(knownSession{Harness: "claude", Open: true, Host: "iTerm2"}, r); got != "iTerm2" {
		t.Errorf("plain terminal: %q", got)
	}
}

// Installed from a shell with CLAUDE_CONFIG_DIR set: the daemon started at
// login doesn't have that variable, yet must use the same folder (and never
// put hooks into a ~/.claude/settings.json the user doesn't use).
func TestInstallNotesTheAgentFolderForTheDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install would query the real Task Scheduler")
	}
	home := sandboxHome(t)
	work := filepath.Join(home, ".claude-work")
	os.MkdirAll(work, 0o700)
	t.Setenv("CLAUDE_CONFIG_DIR", work)
	capture(t, func() {
		if err := doInstall(installOptions{Standalone: true}); err != nil {
			t.Fatal(err)
		}
	})
	t.Setenv("CLAUDE_CONFIG_DIR", "") // the daemon's environment
	cfg, _ := core.LoadConfig(core.GetPaths().Config)
	if got := cfg.ClaudeDir(); got != work {
		t.Fatalf("the daemon would use %s, not %s", got, work)
	}
	if data, _ := os.ReadFile(hooks.Targets(cfg)[0].Path); !strings.Contains(string(data), " hook claude") {
		t.Fatalf("no hooks in %s", hooks.Targets(cfg)[0].Path)
	}
}

// The daemon runs the agents' own commands (`claude agents`, resume
// commands); they must use the folder config.json names, not the default.
func TestDaemonPassesTheAgentFolderToTheAgents(t *testing.T) {
	home := sandboxHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	exportAgentHomes(core.Config{ClaudeHome: filepath.Join(home, ".claude-work"), CodexHome: filepath.Join(home, ".codex")})
	if got := os.Getenv("CLAUDE_CONFIG_DIR"); got != filepath.Join(home, ".claude-work") {
		t.Errorf("CLAUDE_CONFIG_DIR=%q", got)
	}
	if got := os.Getenv("CODEX_HOME"); got != "" {
		t.Errorf("the default folder was set explicitly: CODEX_HOME=%q", got)
	}
}

// A hand-edited messages.json with a typo: resetting one message must not
// save an empty file over all the others.
func TestMessageResetKeepsAnUnreadableFile(t *testing.T) {
	sandboxHome(t)
	p := core.GetPaths()
	p.Ensure()
	broken := []byte("{\"default\": {\"text\": \"Keep going\"}, \"agents\": {\"codex\": {\"text\": \"Carry on\"}},}\n")
	os.WriteFile(messages.Path(p), broken, 0o600)
	err := resetMessage([]string{"--agent", "claude"})
	if data, _ := os.ReadFile(messages.Path(p)); err == nil || string(data) != string(broken) {
		t.Fatalf("err=%v, file now %q", err, data)
	}
}

// Options after the message are not part of it.
func TestMessageSetOptionsAfterTheText(t *testing.T) {
	sandboxHome(t)
	p := core.GetPaths()
	if err := setMessage([]string{"Carry on.", "--agent", "codex"}); err == nil {
		t.Fatal("accepted an option after the message")
	}
	if m, _ := messages.Load(p); m.Default.Text != "" || len(m.Agents) != 0 {
		t.Fatalf("messages changed: %+v", m)
	}
	if err := setMessage([]string{"--agent", "codex", "Use -v --verbose output."}); err != nil {
		t.Fatal(err)
	}
	if m, _ := messages.Load(p); m.Agents["codex"].Text != "Use -v --verbose output." {
		t.Fatalf("messages: %+v", m)
	}
}
