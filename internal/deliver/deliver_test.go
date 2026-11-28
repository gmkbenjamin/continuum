//go:build !windows

package deliver

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/rules"
)

// fakeAgent writes a script that prints help containing helpText and records
// any other invocation in record.
func fakeAgent(t *testing.T, dir, name, helpText string) (bin, record string) {
	bin = filepath.Join(dir, name)
	record = filepath.Join(dir, name+".calls")
	script := `#!/bin/sh
for a in "$@"; do [ "$a" = "--help" ] && { printf '%s\n' "` + helpText + `"; exit 0; }; done
printf '%s\n' "$PWD" "$@" > "` + record + `"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, record
}

// closed: the session is known to be closed everywhere.
var closed = &live.State{Checked: true}

func waitFile(path string) string {
	for i := 0; i < 50; i++ {
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return string(data)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return ""
}

func TestNativeRoutes(t *testing.T) {
	t.Setenv("CONTINUUM_HOME", t.TempDir())
	r, err := rules.Load("")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	work := t.TempDir()
	claude, claudeCalls := fakeAgent(t, dir, "claude", "--bg --resume --print queue exec resume")
	codex, codexCalls := fakeAgent(t, dir, "codex", "queue exec --thread --message --resume")
	cfg := core.Config{Binaries: map[string]string{"claude": claude, "codex": codex}}
	o := Options{Rules: r, Config: cfg, Prompt: "Continue please", Strategies: []string{"wrapper", "multiplexer", "native"}, Live: closed}

	res, err := Deliver(o, &core.Job{Harness: "claude", SessionID: "abc-123", Cwd: work})
	if err != nil || res.Via != "claude-bg-resume" {
		t.Fatalf("claude: %+v %v", res, err)
	}
	got := waitFile(claudeCalls)
	if !strings.Contains(got, "--bg\n--resume\nabc-123\nContinue please") {
		t.Fatalf("claude called with:\n%s", got)
	}
	if real, _ := filepath.EvalSymlinks(work); !strings.HasPrefix(got, real) && !strings.HasPrefix(got, work) {
		t.Fatalf("claude not run in the session's directory:\n%s", got)
	}

	res, err = Deliver(o, &core.Job{Harness: "codex", SessionID: "thread-9", Cwd: work})
	if err != nil || res.Via != "codex-queue" {
		t.Fatalf("codex: %+v %v", res, err)
	}
	if got := waitFile(codexCalls); !strings.Contains(got, "queue\n--thread\nthread-9\n--message\nContinue please\n--resume") {
		t.Fatalf("codex called with:\n%s", got)
	}
}

func TestNativeFallsBackWhenAFlagDisappears(t *testing.T) {
	t.Setenv("CONTINUUM_HOME", t.TempDir())
	r, _ := rules.Load("")
	dir := t.TempDir()
	// A future Claude without --bg: Continuum must use the next command.
	claude, calls := fakeAgent(t, dir, "claude", "--resume --print")
	o := Options{Rules: r, Config: core.Config{Binaries: map[string]string{"claude": claude}}, Prompt: "go on", Strategies: []string{"native"}, Live: closed}
	res, err := Deliver(o, &core.Job{Harness: "claude", SessionID: "s1", Cwd: dir})
	if err != nil || res.Via != "claude-print-resume" {
		t.Fatalf("%+v %v", res, err)
	}
	if got := waitFile(calls); !strings.Contains(got, "--print\n--resume\ns1\ngo on") {
		t.Fatalf("called with:\n%s", got)
	}
}

func TestNoRouteIsReported(t *testing.T) {
	r, _ := rules.Load("")
	dir := t.TempDir()
	claude, _ := fakeAgent(t, dir, "claude", "nothing useful")
	o := Options{Rules: r, Config: core.Config{Binaries: map[string]string{"claude": claude}}, Prompt: "x", Strategies: []string{"wrapper", "multiplexer", "native"}, Live: closed}
	_, err := Deliver(o, &core.Job{Harness: "claude", SessionID: "s1"})
	if err == nil || !strings.Contains(err.Error(), "no delivery route") {
		t.Fatalf("got %v", err)
	}
}

func TestDryRun(t *testing.T) {
	r, _ := rules.Load("")
	dir := t.TempDir()
	codex, calls := fakeAgent(t, dir, "codex", "queue --thread --message --resume")
	o := Options{Rules: r, Config: core.Config{Binaries: map[string]string{"codex": codex}}, Prompt: "x", DryRun: true, Strategies: []string{"native"}, Live: closed}
	res, err := Deliver(o, &core.Job{Harness: "codex", SessionID: "t1"})
	if err != nil || !strings.Contains(res.Detail, "dry run") {
		t.Fatalf("%+v %v", res, err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(calls); err == nil {
		t.Fatal("dry run executed the agent")
	}
}

func TestCleanEnv(t *testing.T) {
	out := CleanEnv([]string{"PATH=/bin", "CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDE_CONFIG_DIR=/c", "TMUX_PANE=%1", "HOME=/h", "ANTHROPIC_API_KEY=k", "HERDR_PANE_ID=w1:p1"})
	joined := strings.Join(out, " ")
	for _, gone := range []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "TMUX_PANE", "HERDR_PANE_ID"} {
		if strings.Contains(joined, gone) {
			t.Errorf("%s kept", gone)
		}
	}
	for _, kept := range []string{"CLAUDE_CONFIG_DIR=/c", "HOME=/h", "ANTHROPIC_API_KEY=k", "PATH="} {
		if !strings.Contains(joined, kept) {
			t.Errorf("%s dropped", kept)
		}
	}
}

func TestNeverStartsACopyOfAnOpenSession(t *testing.T) {
	t.Setenv("CONTINUUM_HOME", t.TempDir())
	r, _ := rules.Load("")
	dir := t.TempDir()
	claude, calls := fakeAgent(t, dir, "claude", "--bg --resume --print attach")
	codex, codexCalls := fakeAgent(t, dir, "codex", "queue exec --thread --message --resume")
	cfg := core.Config{Binaries: map[string]string{"claude": claude, "codex": codex}}
	open := &live.State{Checked: true, Open: true, Session: &live.Session{SessionID: "s1", PID: 99999, Kind: "interactive"}}
	for _, harness := range []string{"claude", "codex"} {
		o := Options{Rules: r, Config: cfg, Prompt: "go on", Strategies: []string{"wrapper", "multiplexer", "native"}, Live: open}
		_, err := Deliver(o, &core.Job{Harness: harness, SessionID: "s1", Cwd: dir})
		if !errors.Is(err, ErrStillOpen) || !errors.Is(err, ErrNoRoute) {
			t.Fatalf("%s: got %v", harness, err)
		}
	}
	// Liveness unknown and no pid: also refused.
	o := Options{Rules: r, Config: cfg, Prompt: "go on", Strategies: []string{"native"}}
	if _, err := Deliver(o, &core.Job{Harness: "claude", SessionID: "s1"}); !errors.Is(err, ErrStillOpen) {
		t.Fatalf("unknown liveness: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	for _, f := range []string{calls, codexCalls} {
		if _, err := os.Stat(f); err == nil {
			t.Fatalf("an agent was started: %s", f)
		}
	}
	// Liveness unknown but the hook-recorded process is gone: allowed.
	res, err := Deliver(o, &core.Job{Harness: "claude", SessionID: "s1", PID: 999999, Cwd: dir})
	if err != nil || res.Via != "claude-bg-resume" {
		t.Fatalf("dead pid: %+v %v", res, err)
	}
	// allow_copies opts back in.
	yes := true
	o = Options{Rules: r, Config: core.Config{Binaries: cfg.Binaries, AllowCopies: &yes}, Prompt: "go on", Strategies: []string{"native"}, Live: open}
	if res, err := Deliver(o, &core.Job{Harness: "codex", SessionID: "s1", Cwd: dir}); err != nil || res.Via != "codex-queue" {
		t.Fatalf("allow_copies: %+v %v", res, err)
	}
}

// fakeAttach writes a claude stand-in whose `attach` shows a screen and
// records the line typed into it.
func fakeAttach(t *testing.T, dir, screen string) (bin, record string) {
	bin = filepath.Join(dir, "claude")
	record = filepath.Join(dir, "typed")
	script := `#!/bin/sh
for a in "$@"; do [ "$a" = "--help" ] && { echo "Commands: attach agents --bg --resume --print"; exit 0; }; done
if [ "$1" = "attach" ]; then
  printf '%s\n' "$2" > "` + record + `.id"
  printf '` + screen + `\n'
  IFS= read -r line
  printf '%s' "$line" > "` + record + `"
  sleep 30
fi
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, record
}

func TestBackgroundSessionIsTypedIntoInPlace(t *testing.T) {
	attachQuiet, attachAfterEnter = 300*time.Millisecond, 500*time.Millisecond
	t.Setenv("CONTINUUM_HOME", t.TempDir())
	r, _ := rules.Load("")
	dir := t.TempDir()
	claude, record := fakeAttach(t, dir, "You have hit your limit\\n> \\n? for shortcuts")
	bg := &live.State{Checked: true, Open: true, Background: true, Session: &live.Session{SessionID: "s1", ShortID: "7b3e9a10", Kind: "background"}}
	o := Options{Rules: r, Config: core.Config{Binaries: map[string]string{"claude": claude}}, Prompt: "Carry on", Strategies: []string{"wrapper", "multiplexer", "native"}, Live: bg}
	start := time.Now()
	res, err := Deliver(o, &core.Job{Harness: "claude", SessionID: "s1", Cwd: dir})
	if err != nil || res.Via != "claude-attach" {
		t.Fatalf("%+v %v", res, err)
	}
	if got := waitFile(record); got != "Carry on" {
		t.Fatalf("typed %q", got)
	}
	if id := waitFile(record + ".id"); strings.TrimSpace(id) != "7b3e9a10" {
		t.Fatalf("attached to %q", id)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("the attach client was not closed")
	}
}

func TestBackgroundSessionWithAMenuIsLeftAlone(t *testing.T) {
	attachQuiet, attachAfterEnter = 300*time.Millisecond, 500*time.Millisecond
	t.Setenv("CONTINUUM_HOME", t.TempDir())
	r, _ := rules.Load("")
	dir := t.TempDir()
	claude, record := fakeAttach(t, dir, "Do you want to proceed?\\n> 1. Yes\\n  2. No\\n? for shortcuts")
	bg := &live.State{Checked: true, Open: true, Background: true, Session: &live.Session{SessionID: "s1", ShortID: "abc", Kind: "background"}}
	o := Options{Rules: r, Config: core.Config{Binaries: map[string]string{"claude": claude}}, Prompt: "Carry on", Strategies: []string{"native"}, Live: bg}
	_, err := Deliver(o, &core.Job{Harness: "claude", SessionID: "s1", Cwd: dir})
	if err == nil || !strings.Contains(err.Error(), "menu or prompt") {
		t.Fatalf("got %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if data, _ := os.ReadFile(record); len(data) > 0 {
		t.Fatalf("typed into a menu: %q", data)
	}
}

func TestDesktopSessions(t *testing.T) {
	t.Setenv("CONTINUUM_HOME", t.TempDir())
	r, _ := rules.Load("")
	dir := t.TempDir()
	claude, claudeCalls := fakeAgent(t, dir, "claude", "--bg --resume --print attach")
	codex, codexCalls := fakeAgent(t, dir, "codex", "queue exec --thread --message --resume")
	cfg := core.Config{Binaries: map[string]string{"claude": claude, "codex": codex}}
	held := func(app string) *live.State {
		return &live.State{Checked: true, Open: true, OnlyDesktop: true, DesktopApp: app, Session: &live.Session{SessionID: "s1", Kind: "desktop"}}
	}
	// Claude Desktop holds it: no CLI route may run (it would start a copy).
	o := Options{Rules: r, Config: cfg, Prompt: "go on", Strategies: []string{"wrapper", "multiplexer", "native"}, Live: held("Claude Desktop")}
	if _, err := Deliver(o, &core.Job{Harness: "claude", SessionID: "s1", Cwd: dir}); !errors.Is(err, ErrStillOpen) || !strings.Contains(err.Error(), "Claude Desktop") {
		t.Fatalf("claude desktop: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(claudeCalls); err == nil {
		t.Fatal("started claude for a session Claude Desktop holds")
	}
	// Codex Desktop: codex queue goes through the app's own server.
	o.Live = held("Codex Desktop")
	res, err := Deliver(o, &core.Job{Harness: "codex", SessionID: "t1", Cwd: dir})
	if err != nil || res.Via != "codex-queue" {
		t.Fatalf("codex desktop: %+v %v", res, err)
	}
	if got := waitFile(codexCalls); !strings.Contains(got, "queue\n--thread\nt1") {
		t.Fatalf("codex called with %q", got)
	}
}

func TestCodexQueueWarningDoesNotResumeAgain(t *testing.T) {
	r, _ := rules.Load("")
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	calls := filepath.Join(dir, "calls")
	script := `#!/bin/sh
case "$*" in *--help*) echo 'queue exec resume --thread --message --resume'; exit 0;; esac
echo "$1" >> "` + calls + `"
echo 'Warning: message was queued, but could not resume thread t1' >&2
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	o := Options{Rules: r, Config: core.Config{Binaries: map[string]string{"codex": bin}}, Prompt: "Continue.", Strategies: []string{"native"}, Live: closed}
	if _, err := Deliver(o, &core.Job{Harness: "codex", SessionID: "t1", Cwd: dir}); !errors.Is(err, ErrQueued) {
		t.Fatalf("expected a queued prompt requiring the session to reopen, got %v", err)
	}
	if got := waitFile(calls); got != "queue\n" {
		t.Fatalf("expected one queue and no fallback, got %q", got)
	}
}

// A wrapper that exited, for a session nothing else knows (no id, no pane):
// no route can ever reach it, which the daemon must not retry.
func TestGoneWrapperWithNothingElseIsPermanent(t *testing.T) {
	r, _ := rules.Load("")
	dir := t.TempDir()
	claude, _ := fakeAgent(t, dir, "claude", "nothing useful")
	o := Options{Rules: r, Paths: core.Paths{Wrappers: filepath.Join(dir, "wrappers")}, Config: core.Config{Binaries: map[string]string{"claude": claude}},
		Prompt: "x", Strategies: []string{"wrapper", "multiplexer", "native"}, Live: closed}
	gone := &core.Job{Harness: "claude", Location: &core.Location{Kind: "wrapper", Target: "dead0000"}}
	_, err := Deliver(o, gone)
	if !errors.Is(err, ErrNoRoute) || !errors.Is(err, ErrGone) {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "permanent") {
		t.Errorf("the marker shows in the message: %v", err)
	}
	// With a session id the agent's own command may still continue it.
	known := &core.Job{Harness: "claude", SessionID: "s1", Location: gone.Location}
	if _, err := Deliver(o, known); err == nil || errors.Is(err, ErrGone) {
		t.Fatalf("with a session id: %v", err)
	}
}

func TestTrackClosedSessionsOffBlocksStoppedNativeCommands(t *testing.T) {
	r, _ := rules.Load("")
	dir := t.TempDir()
	claude, calls := fakeAgent(t, dir, "claude", "--resume --print")
	off := false
	o := Options{
		Rules:      r,
		Config:     core.Config{Binaries: map[string]string{"claude": claude}, TrackClosedSessions: &off},
		Prompt:     "go on",
		Strategies: []string{"native"},
		Live:       closed,
	}
	_, err := Deliver(o, &core.Job{Harness: "claude", SessionID: "s1", Cwd: dir})
	if err == nil || !strings.Contains(err.Error(), "track_closed_sessions=false") {
		t.Fatalf("expected stopped native command to be blocked, got %v", err)
	}
	if _, err := os.Stat(calls); err == nil {
		t.Fatal("claude should not have been invoked for a closed session")
	}
}
