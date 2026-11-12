package main

import (
	"bufio"
	"bytes"
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
	if out := runScript(t, "x\n"); !strings.Contains(out, "Please choose a number") {
		t.Fatalf("unknown choice:\n%s", out)
	}
	if out := runScript(t, "1\n"); !strings.Contains(out, "Continuum "+core.Version) {
		t.Fatalf("status:\n%s", out)
	}
}
