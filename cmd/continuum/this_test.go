package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/daemon"
	"github.com/continuum-dev/continuum/internal/messages"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/scope"
	"github.com/continuum-dev/continuum/internal/store"
)

// runningUnder makes `this` see a chain of parent processes (the command's
// parent first) and an environment.
func runningUnder(t *testing.T, env map[string]string, chain ...string) {
	t.Helper()
	tree := map[int]proc.Node{}
	for i, name := range chain {
		tree[100+i] = proc.Node{PPID: 101 + i, Name: name}
	}
	tree[100+len(chain)] = proc.Node{PPID: 1, Name: "launchd"}
	ot, ol, oc, oe, op := thisTree, thisList, thisCwd, thisEnv, thisPPID
	t.Cleanup(func() { thisTree, thisList, thisCwd, thisEnv, thisPPID = ot, ol, oc, oe, op })
	thisTree = func() map[int]proc.Node { return tree }
	thisList = func() ([]proc.Process, error) { return nil, nil }
	thisCwd = func(int) string { return "" }
	thisEnv = func() map[string]string { return env }
	thisPPID = func() int { return 100 }
}

func TestThisFromTheAgentsOwnVariable(t *testing.T) {
	sandboxHome(t)
	runningUnder(t, map[string]string{"CLAUDE_CODE_SESSION_ID": "c1aude00-0000-4000-8000-000000000001"}, "zsh", "claude", "zsh")
	h, err := thisSession()
	if err != nil || h.Harness != "claude" || h.ID != "c1aude00-0000-4000-8000-000000000001" {
		t.Fatalf("%+v %v", h, err)
	}
}

// Codex doesn't say which session; its hooks recorded its process. A Claude
// variable inherited from an outer session must not be taken for it.
func TestThisFromTheAgentsProcess(t *testing.T) {
	sandboxHome(t)
	p := core.GetPaths()
	p.Ensure()
	store.SaveJSON(p.Sessions, map[string]*store.Session{
		"codex:0dex": {Harness: "codex", SessionID: "c0dex000-0000-7000-8000-000000000001", PID: 101, Cwd: "/w", LastSeen: time.Now()},
	})
	runningUnder(t, map[string]string{"CLAUDE_CODE_SESSION_ID": "outer"}, "zsh", "codex", "bash", "claude")
	h, err := thisSession()
	if err != nil || h.Harness != "codex" || h.ID != "c0dex000-0000-7000-8000-000000000001" {
		t.Fatalf("%+v %v", h, err)
	}
	// exclude this
	capture(t, func() {
		if err := cmdChoose("excluded", []string{"this"}); err != nil {
			t.Fatal(err)
		}
	})
	sc, _ := scope.Load(p)
	if ok, _ := sc.Covers(&core.Job{Harness: "codex", SessionID: h.ID}); ok {
		t.Fatal("exclude this didn't exclude it")
	}
	if h.selector() != "codex:c0dex000-0000-7000-8000-000000000001" {
		t.Fatalf("selector %s", h.selector())
	}
}

// An agent under an interpreter, known only by Continuum's wrapper.
func TestThisFromTheWrapper(t *testing.T) {
	sandboxHome(t)
	p := core.GetPaths()
	os.MkdirAll(filepath.Join(p.Wrappers, "w1"), 0o700)
	os.WriteFile(filepath.Join(p.Wrappers, "w1", "meta.json"), []byte(`{"harness":"gemini","cwd":"/w"}`), 0o600)
	runningUnder(t, map[string]string{"CONTINUUM_WRAPPER_ID": "w1"}, "bash", "node", "continuum", "zsh")
	h, err := thisSession()
	if err != nil || h.Harness != "gemini" || h.Wrapper != "w1" || h.ID != "" {
		t.Fatalf("%+v %v", h, err)
	}
	if e := h.entry(); e.Wrapper != "w1" {
		t.Fatalf("entry %+v", e)
	}
	// A wrapper id inherited by a different agent isn't its own.
	runningUnder(t, map[string]string{"CONTINUUM_WRAPPER_ID": "w1"}, "zsh", "claude", "node", "continuum")
	if h, err := thisSession(); err == nil && h.Wrapper != "" {
		t.Fatalf("claude took gemini's wrapper: %+v", h)
	}
}

func TestThisOutsideAnAgent(t *testing.T) {
	sandboxHome(t)
	runningUnder(t, map[string]string{"CLAUDE_CODE_SESSION_ID": "leftover"}, "zsh", "Terminal")
	if _, err := thisSession(); err == nil || !strings.Contains(err.Error(), "isn't run by an agent") {
		t.Fatalf("err %v", err)
	}
	if _, err := resolveEntry("this", nil); err == nil {
		t.Fatal("resolved this outside an agent")
	}
}

func TestAgentGuideAndSkill(t *testing.T) {
	g := guideText("/x/continuum")
	for _, want := range []string{"continuum this", "exclude this", "message set --session this", "/x/continuum"} {
		if !strings.Contains(g, want) {
			t.Errorf("guide lacks %q", want)
		}
	}
	home := sandboxHome(t)
	p := core.GetPaths()
	r := rulesForTest(t)
	cfg, _ := core.LoadConfig(p.Config)
	// A skill of the user's with the same name is left alone.
	mine := filepath.Join(home, ".codex", "skills", "continuum")
	os.MkdirAll(mine, 0o700)
	os.WriteFile(filepath.Join(mine, "SKILL.md"), []byte("---\nname: continuum\n---\nmine"), 0o600)
	installSkills(cfg, r, "/x/continuum", false)
	claude := filepath.Join(home, ".claude", "skills", "continuum")
	if !ourSkill(claude) {
		t.Fatal("no skill for Claude Code")
	}
	if data, _ := os.ReadFile(filepath.Join(mine, "SKILL.md")); string(data) != "---\nname: continuum\n---\nmine" {
		t.Fatal("overwrote the user's skill")
	}
	uninstallSkills(cfg, false)
	if _, err := os.Stat(claude); !os.IsNotExist(err) {
		t.Fatal("skill left behind")
	}
	if _, err := os.Stat(filepath.Join(mine, "SKILL.md")); err != nil {
		t.Fatal("removed the user's skill")
	}
}

func rulesForTest(t *testing.T) *rules.Rules {
	t.Helper()
	r, err := rules.Load("")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Found by hand: a skill that was written reported "no such file", the
// ~/.local/bin link promised by the plan wasn't made when the folder didn't
// exist yet, and detect printed one block per agent for the same verdict.
func TestHandFoundBugs(t *testing.T) {
	home := sandboxHome(t)
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	out := strings.Join(installSkills(cfg, rulesForTest(t), "/x/continuum", false), "\n")
	if strings.Contains(out, "no such file") || !strings.Contains(out, "can now check") {
		t.Errorf("skill install said:\n%s", out)
	}

	os.RemoveAll(filepath.Join(home, ".local"))
	bin := filepath.Join(home, "prog")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	capture(t, func() { linkIntoPath(bin, false) })
	if target, err := os.Readlink(filepath.Join(home, ".local", "bin", "continuum")); runtime.GOOS != "windows" && (err != nil || target != bin) {
		t.Errorf("link: %q %v", target, err)
	}

	got := capture(t, func() { cmdDetect([]string{"Claude usage limit reached · resets 3pm"}) })
	if strings.Count(got, "matched:") != 1 || !strings.Contains(got, "every agent") {
		t.Errorf("detect:\n%s", got)
	}
	got = capture(t, func() {
		cmdDetect([]string{"--harness", "antigravity", "Your AI credits balance is too low to continue."})
	})
	if !strings.Contains(got, "never_resume") || strings.Contains(got, "retry with backoff") {
		t.Errorf("detect billing:\n%s", got)
	}
	if s := (scope.Entry{Dir: filepath.Join(home, "work", "api")}).String(); s != "every session in "+filepath.FromSlash("~/work/api") {
		t.Errorf("folder label %q", s)
	}
}

func TestThisSessionEnvFillsCwdAndAntigravityWrapper(t *testing.T) {
	sandboxHome(t)
	p := core.GetPaths()
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	// SessionEnv populates Cwd from sessions.json so folder scope applies.
	store.SaveJSON(p.Sessions, map[string]*store.Session{
		"claude:s-env": {Harness: "claude", SessionID: "s-env", Cwd: "/work/app", LastSeen: time.Now()},
	})
	runningUnder(t, map[string]string{"CLAUDE_CODE_SESSION_ID": "s-env"}, "zsh", "claude", "bash")
	h, err := thisSession()
	if err != nil || h.Cwd != "/work/app" || h.ID != "s-env" {
		t.Fatalf("got %+v err=%v", h, err)
	}

	// Antigravity inside continuum's wrapper sets ANTIGRAVITY_CONVERSATION_ID
	// but is tracked by wrapper ID because it sends no hooks/transcripts.
	wdir := filepath.Join(p.Wrappers, "w-ag")
	os.MkdirAll(wdir, 0o700)
	os.WriteFile(filepath.Join(wdir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	meta, _ := json.Marshal(map[string]any{"harness": "antigravity", "cwd": "/work/ag", "started": time.Now()})
	os.WriteFile(filepath.Join(wdir, "meta.json"), meta, 0o600)
	runningUnder(t, map[string]string{
		"CONTINUUM_WRAPPER_ID":        "w-ag",
		"ANTIGRAVITY_CONVERSATION_ID": "conv-999",
	}, "bash", "antigravity", "zsh")
	h, err = thisSession()
	if err != nil || h.Harness != "antigravity" || h.Wrapper != "w-ag" || h.ID != "" || h.Cwd != "/work/ag" {
		t.Fatalf("wrapped antigravity got %+v err=%v", h, err)
	}
	if h.selector() != "antigravity@wrapper:w-ag" || h.entry().Wrapper != "w-ag" {
		t.Fatalf("selector=%q entry=%+v", h.selector(), h.entry())
	}
}

func TestThisInterpretedAgentAndTmuxSocketMismatch(t *testing.T) {
	sandboxHome(t)
	p := core.GetPaths()
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	// Interpreted agent outside wrapper: `node` running gemini.
	store.SaveJSON(p.Sessions, map[string]*store.Session{
		"gemini:g1": {Harness: "gemini", SessionID: "g1", PID: 101, Cwd: "/work/g", LastSeen: time.Now()},
	})
	runningUnder(t, map[string]string{}, "zsh", "node", "bash")
	thisList = func() ([]proc.Process, error) {
		return []proc.Process{{PID: 101, Line: "node /usr/local/lib/node_modules/@google/gemini-cli/dist/index.js"}}, nil
	}
	h, err := thisSession()
	if err != nil || h.Harness != "gemini" || h.ID != "g1" {
		t.Fatalf("interpreted gemini got %+v err=%v", h, err)
	}

	// Same tmux pane target %1 on a different socket must not match.
	store.SaveJSON(p.Sessions, map[string]*store.Session{
		"codex:c1": {Harness: "codex", SessionID: "c1", Location: &core.Location{Kind: "tmux", Target: "%1", Socket: "/tmp/sock-a"}, LastSeen: time.Now()},
	})
	runningUnder(t, map[string]string{"TMUX": "/tmp/sock-b,12,0", "TMUX_PANE": "%1"}, "zsh", "codex", "bash")
	if h, err := thisSession(); err == nil {
		t.Fatalf("matched session from different tmux socket: %+v", h)
	}
}

func TestResolveEntryWindowsPathAndDuplicateNameAndPurgeAndInstalledBin(t *testing.T) {
	sandboxHome(t)
	p := core.GetPaths()
	p.Ensure()

	// Windows drive-letter path must not be misparsed as harness:session.
	if _, err := resolveEntry(`C:\work\app`, nil); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("Windows drive path err = %v", err)
	}

	// Duplicate exact session names must report ErrAmbiguous.
	_, err := resolveEntry("bugfix", []knownSession{
		{Harness: "claude", ID: "11111111", Name: "bugfix"},
		{Harness: "codex", ID: "22222222", Name: "bugfix"},
	})
	if !errors.Is(err, scope.ErrAmbiguous) {
		t.Fatalf("duplicate exact name err = %v, want ErrAmbiguous", err)
	}

	// installedBin falls back to install.json when bin-path is missing.
	rec, _ := json.Marshal(daemon.Install{Bin: "/opt/continuum/bin/continuum"})
	os.WriteFile(daemon.InstallPath(p), rec, 0o600)
	if got := installedBin(); got != "/opt/continuum/bin/continuum" {
		t.Fatalf("installedBin() = %q", got)
	}

	// purgeHome cleans native-*.log and *.lock files.
	os.WriteFile(filepath.Join(p.Home, "native-claude.log"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(p.Home, "scope.json.lock"), []byte(""), 0o600)
	os.WriteFile(filepath.Join(p.Home, "messages.json.lock"), []byte(""), 0o600)
	if err := purgeHome(p.Home); err != nil {
		t.Fatalf("purgeHome: %v", err)
	}
	if _, err := os.Stat(p.Home); !os.IsNotExist(err) {
		t.Fatalf("home not removed cleanly: %v", err)
	}
}

func TestUnchooseResetAndControlWithThisAndWrapper(t *testing.T) {
	sandboxHome(t)
	p := core.GetPaths()
	p.Ensure()

	// Outside an agent session, `unchoose this` and `message reset --session this` must error.
	runningUnder(t, map[string]string{}, "zsh", "bash")
	if err := cmdUnchoose("chosen", []string{"this"}); !errors.Is(err, errNotInSession) {
		t.Fatalf("unchoose this outside session err = %v, want errNotInSession", err)
	}
	if err := resetMessage([]string{"--session", "this"}); !errors.Is(err, errNotInSession) {
		t.Fatalf("message reset --session this outside session err = %v, want errNotInSession", err)
	}

	// resetOne routes Windows paths to --folder instead of --session.
	if err := resetOne(`C:\work\app`); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("resetOne(C:\\work\\app) err = %v, want directory error", err)
	}

	// Inside a wrapped session where both SessionID and Wrapper are known,
	// `unchoose this`, `message reset --session this`, and `cancel this` must
	// also affect wrapper-keyed entries/jobs from `continuum run`.
	os.MkdirAll(filepath.Join(p.Wrappers, "w1"), 0o700)
	os.WriteFile(filepath.Join(p.Wrappers, "w1", "meta.json"), []byte(`{"harness":"claude","cwd":"/work/app"}`), 0o600)
	store.SaveJSON(p.Sessions, map[string]*store.Session{
		"claude:c1234567": {Harness: "claude", SessionID: "c1234567", PID: 101, Cwd: "/work/app", LastSeen: time.Now()},
	})
	runningUnder(t, map[string]string{"CONTINUUM_WRAPPER_ID": "w1"}, "zsh", "claude", "bash")
	_ = (scope.Scope{Mode: scope.ModeChosen, Chosen: []scope.Entry{{Harness: "claude", Wrapper: "w1"}}}).Save(p)
	if err := cmdUnchoose("chosen", []string{"this"}); err != nil {
		t.Fatalf("unchoose this: %v", err)
	}
	sc, _ := scope.Load(p)
	if len(sc.Chosen) != 0 {
		t.Fatalf("wrapper chosen entry still present after unchoose this: %+v", sc.Chosen)
	}

	_ = messages.Update(p, func(m *messages.Messages) (bool, error) {
		m.Set(scope.Entry{Harness: "claude", Wrapper: "w1"}, messages.Pair{Text: "wrap msg"})
		return true, nil
	})
	if err := resetMessage([]string{"--session", "this"}); err != nil {
		t.Fatalf("resetMessage --session this: %v", err)
	}
	ms, _ := messages.Load(p)
	if len(ms.Rules) != 0 {
		t.Fatalf("wrapper message rule still present after reset: %+v", ms.Rules)
	}

	// cmdControl: `wrapper:w1`, `claude:1a2b`, and `this` against wrapper-keyed job.
	store.SaveJSON(p.Jobs, map[string]*core.Job{
		"generic@wrapper:w1":  {Key: "generic@wrapper:w1", Harness: "generic", State: core.StatePending, Location: &core.Location{Kind: "wrapper", Target: "w1"}},
		"claude:1a2b3c4d":     {Key: "claude:1a2b3c4d", Harness: "claude", SessionID: "1a2b3c4d", State: core.StatePending},
		"codex:1a2b9999":      {Key: "codex:1a2b9999", Harness: "codex", SessionID: "1a2b9999", State: core.StatePending},
		"antigravity@wrapper": {Key: "antigravity@wrapper:w2", Harness: "antigravity", State: core.StatePending, Location: &core.Location{Kind: "wrapper", Target: "w2"}},
	})
	if err := cmdControl("cancel", []string{"this"}); err != nil {
		t.Fatalf("cancel this: %v", err)
	}
	if err := cmdControl("cancel", []string{"claude:1a2b"}); err != nil {
		t.Fatalf("cancel claude:1a2b: %v", err)
	}
	if err := cmdControl("cancel", []string{"wrapper:w2"}); err != nil {
		t.Fatalf("cancel wrapper:w2: %v", err)
	}
	jobs, _ := store.LoadJobs(p.Jobs)
	if jobs["generic@wrapper:w1"].State != core.StateCancelled {
		t.Fatalf("generic@wrapper:w1 state = %s, want cancelled", jobs["generic@wrapper:w1"].State)
	}
	if jobs["claude:1a2b3c4d"].State != core.StateCancelled {
		t.Fatalf("claude:1a2b3c4d state = %s, want cancelled", jobs["claude:1a2b3c4d"].State)
	}
	if jobs["codex:1a2b9999"].State != core.StatePending {
		t.Fatalf("codex:1a2b9999 state = %s, want pending (harness isolation)", jobs["codex:1a2b9999"].State)
	}
	if jobs["antigravity@wrapper"].State != core.StateCancelled {
		t.Fatalf("antigravity@wrapper:w2 state = %s, want cancelled", jobs["antigravity@wrapper"].State)
	}
}
