package messages

import (
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
