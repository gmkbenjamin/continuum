package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
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
	ot, oe, op := thisTree, thisEnv, thisPPID
	t.Cleanup(func() { thisTree, thisEnv, thisPPID = ot, oe, op })
	thisTree = func() map[int]proc.Node { return tree }
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
	if target, err := os.Readlink(filepath.Join(home, ".local", "bin", "continuum")); err != nil || target != bin {
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
	if s := (scope.Entry{Dir: filepath.Join(home, "work", "api")}).String(); s != "every session in ~/work/api" {
		t.Errorf("folder label %q", s)
	}
}
