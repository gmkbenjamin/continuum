package messages

import (
	"fmt"
	"sync"
	"testing"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/scope"
)

func TestMostSpecificWins(t *testing.T) {
	r, _ := rules.Load("")
	cfg := core.Config{}
	j := &core.Job{Harness: "claude", SessionID: "5e551011-1111", Cwd: "/w/shop/api"}
	m := &Messages{Agents: map[string]Pair{}}

	if c := m.Resolve(j, false, cfg, r); c.Source != "built-in" || c.Text == "" {
		t.Fatalf("built-in: %+v", c)
	}
	if c := m.Resolve(j, true, cfg, r); c.Text != BuiltInOutage {
		t.Fatalf("built-in outage: %+v", c)
	}
	m.Default = Pair{Text: "Keep going in {folder} ({agent})."}
	if c := m.Resolve(j, false, cfg, r); c.Source != "default" || c.Text != "Keep going in api (Claude Code)." {
		t.Fatalf("default: %+v", c)
	}
	cfg.ResumePrompts = map[string]string{"claude": "from config"}
	if c := m.Resolve(j, false, cfg, r); c.Text != "from config" {
		t.Fatalf("config.json agent prompt: %+v", c)
	}
	m.Agents["claude"] = Pair{Text: "agent msg"}
	if c := m.Resolve(j, false, cfg, r); c.Source != "agent claude" {
		t.Fatalf("agent: %+v", c)
	}
	m.Set(scope.Entry{Dir: "/w"}, Pair{Text: "outer folder"})
	m.Set(scope.Entry{Dir: "/w/shop"}, Pair{Text: "inner folder"})
	if c := m.Resolve(j, false, cfg, r); c.Text != "inner folder" {
		t.Fatalf("deepest folder: %+v", c)
	}
	m.Set(scope.Entry{Session: "5e551011"}, Pair{Text: "this session"})
	if c := m.Resolve(j, false, cfg, r); c.Text != "this session" || c.Source != "session" {
		t.Fatalf("session: %+v", c)
	}
	// No outage message at the session level: falls through to the agent's
	// config outage message or the built-in one.
	if c := m.Resolve(j, true, cfg, r); c.Text != BuiltInOutage {
		t.Fatalf("outage: %+v", c)
	}
	m.Set(scope.Entry{Session: "5e551011"}, Pair{Outage: "retry please"})
	if c := m.Resolve(j, true, cfg, r); c.Text != "retry please" {
		t.Fatalf("session outage: %+v", c)
	}
	if c := m.Resolve(j, false, cfg, r); c.Text != "this session" {
		t.Fatal("setting the outage message replaced the main one")
	}
	if !m.Clear(scope.Entry{Session: "5e55"}, false) {
		t.Fatal("clear by prefix")
	}
	if c := m.Resolve(j, false, cfg, r); c.Text != "inner folder" {
		t.Fatalf("after clear: %+v", c)
	}
	// A wrapper's message applies to the session in it, until it ends.
	m.Set(scope.Entry{Wrapper: "w1"}, Pair{Text: "wrapped"})
	jw := &core.Job{Harness: "claude", Cwd: "/x", Location: &core.Location{Kind: "wrapper", Target: "w1"}}
	if c := m.Resolve(jw, false, cfg, r); c.Text != "wrapped" {
		t.Fatalf("wrapper: %+v", c)
	}
	m.PruneWrappers(func(string) bool { return false })
	if c := m.Resolve(jw, false, cfg, r); c.Text == "wrapped" {
		t.Fatal("ended wrapper's message kept")
	}
}

func TestClean(t *testing.T) {
	got, err := Clean("  line one\nline two\r\n\tthree  ")
	if err != nil || got != "line one line two three" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := Clean(" \n "); err == nil {
		t.Fatal("empty accepted")
	}
}

// `continuum message reset --session 1` must not clear another session's
// message because its id starts with 1.
func TestClearNeedsMoreThanACharacter(t *testing.T) {
	m := &Messages{Agents: map[string]Pair{}}
	m.Set(scope.Entry{Session: "1a2b3c4d"}, Pair{Text: "this one"})
	if m.Clear(scope.Entry{Session: "1"}, false) || len(m.Rules) != 1 {
		t.Fatalf("cleared by one character: %+v", m.Rules)
	}
}

func TestClearPrefixRespectsHarnessAndConcurrentUpdate(t *testing.T) {
	m := &Messages{Agents: map[string]Pair{}}
	m.Set(scope.Entry{Harness: "claude", Session: "abcd1111"}, Pair{Text: "c"})
	m.Set(scope.Entry{Harness: "codex", Session: "abcd2222"}, Pair{Text: "x"})
	if !m.Clear(scope.Entry{Harness: "claude", Session: "abcd"}, false) {
		t.Fatal("expected clear to match claude:abcd")
	}
	if len(m.Rules) != 1 || m.Rules[0].Harness != "codex" {
		t.Fatalf("remaining rules: %+v", m.Rules)
	}

	t.Setenv("CONTINUUM_HOME", t.TempDir())
	p := core.GetPaths()
	const workers = 10
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := Update(p, func(cur *Messages) (bool, error) {
				cur.Set(scope.Entry{Harness: "claude", Session: fmt.Sprintf("s-%02d", i)}, Pair{Text: "msg"})
				return true, nil
			}); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Update failed: %v", err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Rules) != workers {
		t.Fatalf("lost concurrent message updates: got %d of %d", len(got.Rules), workers)
	}
}

func TestSetAndExactClearRespectHarness(t *testing.T) {
	m := &Messages{Agents: map[string]Pair{}}
	m.Set(scope.Entry{Harness: "claude", Session: "same-id"}, Pair{Text: "claude msg"})
	m.Set(scope.Entry{Harness: "codex", Session: "same-id"}, Pair{Text: "codex msg"})
	if len(m.Rules) != 2 {
		t.Fatalf("expected 2 separate per-harness rules, got %+v", m.Rules)
	}
	// Exact Clear specifying claude must leave codex intact.
	if !m.Clear(scope.Entry{Harness: "claude", Session: "same-id"}, false) {
		t.Fatal("expected exact clear for claude:same-id")
	}
	if len(m.Rules) != 1 || m.Rules[0].Harness != "codex" || m.Rules[0].Text != "codex msg" {
		t.Fatalf("remaining rules after exact clear: %+v", m.Rules)
	}
	// Empty harness acts as wildcard (e.g. when clearing a gone session without harness).
	if !m.Clear(scope.Entry{Session: "same-id"}, false) || len(m.Rules) != 0 {
		t.Fatalf("expected wildcard clear to remove remaining rule, got %+v", m.Rules)
	}
}
