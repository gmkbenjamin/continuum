package main

import (
	"bufio"
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/messages"
	"github.com/continuum-dev/continuum/internal/scope"
)

func runScript(t *testing.T, input string) string {
	t.Helper()
	var out bytes.Buffer
	var err error
	captured := capture(t, func() {
		err = runInteractive(bufio.NewReader(strings.NewReader(input)), &out)
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.String() + captured
}

func TestInteractiveMenu(t *testing.T) {
	sandboxHome(t)
	p := core.GetPaths()

	// Scope: chosen only.
	out := runScript(t, "4\n2\n\nq\n")
	if sc, _ := scope.Load(p); sc.Mode != scope.ModeChosen {
		t.Fatalf("scope not changed:\n%s", out)
	}
	// Messages: default, agent (by number), folder.
	runScript(t, "3\n1\nKeep going in {folder}\n3\n2\nCodex, carry on\n4\n.\nThis folder\n\nq\n")
	m, _ := messages.Load(p)
	if m.Default.Text != "Keep going in {folder}" || m.Agents["codex"].Text != "Codex, carry on" || len(m.Rules) != 1 {
		t.Fatalf("messages: %+v", m)
	}
	// Reset all, confirmed.
	runScript(t, "3\n6\ny\n\nq\n")
	m, _ = messages.Load(p)
	if m.Default.Text != "" || len(m.Agents) != 0 || len(m.Rules) != 0 {
		t.Fatalf("not reset: %+v", m)
	}
	// End of input quits cleanly; unknown choices are explained.
	if out := runScript(t, "z\n"); !strings.Contains(out, "Please choose a number or letter") {
		t.Fatalf("unknown choice:\n%s", out)
	}
	if out := runScript(t, "1\n"); !strings.Contains(out, "Continuum "+core.Version) {
		t.Fatalf("status:\n%s", out)
	}
}

func TestInteractiveLetters(t *testing.T) {
	sandboxHome(t)
	p := core.GetPaths()

	// Scope by letters (either case): c = Scope, c = chosen; then e = every.
	out := runScript(t, "C\nc\n\nq\n")
	if sc, _ := scope.Load(p); sc.Mode != scope.ModeChosen {
		t.Fatalf("scope not changed by letter:\n%s", out)
	}
	if !strings.Contains(out, "[S]tatus") || !strings.Contains(out, "S[c]ope") || !strings.Contains(out, "Resume [c]hosen") {
		t.Fatalf("letters not shown:\n%s", out)
	}
	runScript(t, "c\ne\n\nq\n")
	if sc, _ := scope.Load(p); sc.Mode == scope.ModeChosen {
		t.Fatal("scope not set back to every session")
	}
	// Messages by letters: m = messages, d = default, a = agent.
	runScript(t, "m\nd\nCarry on\na\n2\nCodex, go\n\nq\n")
	m, _ := messages.Load(p)
	if m.Default.Text != "Carry on" || m.Agents["codex"].Text != "Codex, go" {
		t.Fatalf("messages by letter: %+v", m)
	}
}

func TestMenuLettersUnique(t *testing.T) {
	src, err := os.ReadFile("interactive.go")
	if err != nil {
		t.Fatal(err)
	}
	// Each menu literal is a "menu := ..." block; letters must be unique
	// within it and never "q" (quit / go back).
	for _, block := range strings.Split(string(src), "menu := ")[1:] {
		block = block[:strings.Index(block, "t.showMenu")]
		seen := map[string]string{}
		for _, m := range regexp.MustCompile(`\{"(\w+)", "([^"]*&[^"]*)"`).FindAllStringSubmatch(block, -1) {
			l := option{m[1], m[2]}.letter()
			if l == "q" || seen[l] != "" {
				t.Errorf("letter %q of %q clashes (%q)", l, m[2], seen[l])
			}
			seen[l] = m[2]
		}
	}
}
