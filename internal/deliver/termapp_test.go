package deliver

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
)

func fakeGhostty(t *testing.T, fgPID int) *[][]string {
	t.Helper()
	var calls [][]string
	oldRun, oldProcs, oldTTY := termRun, termProcs, termTTY
	t.Cleanup(func() { termRun, termProcs, termTTY = oldRun, oldProcs, oldTTY })
	termProcs = func() ([]proc.Process, error) {
		return []proc.Process{{PID: 1, Line: "/Applications/Ghostty.app/Contents/MacOS/ghostty"}}, nil
	}
	termTTY = func(int) string { return "/dev/ttys042" }
	termRun = func(argv []string) (string, error) {
		calls = append(calls, argv)
		if strings.Contains(strings.Join(argv, " "), "repeat with t in terminals") {
			if argv[len(argv)-1] != "/dev/ttys042" {
				t.Errorf("searched for tty %q", argv[len(argv)-1])
			}
			return "TERM-7\t" + strconv.Itoa(fgPID) + "\n", nil
		}
		return "", nil
	}
	return &calls
}

func ghosttyOpts(t *testing.T, st *live.State) Options {
	r, err := rules.Load("")
	if err != nil {
		t.Fatal(err)
	}
	return Options{Rules: r, Prompt: "Carry on", Strategies: []string{"terminal-app"}, Live: st}
}

func TestGhosttyTypesWhenTheAgentSaysItIsIdle(t *testing.T) {
	me := os.Getpid()
	calls := fakeGhostty(t, me)
	st := &live.State{Checked: true, Open: true, Source: "command", Session: &live.Session{SessionID: "s1", PID: me, Status: "idle"}}
	res, err := Deliver(ghosttyOpts(t, st), &core.Job{Harness: "claude", SessionID: "s1"})
	if err != nil || res.Via != "ghostty" {
		t.Fatalf("%+v %v", res, err)
	}
	if len(*calls) != 3 {
		t.Fatalf("calls %v", *calls)
	}
	text, enter := strings.Join((*calls)[1], " "), strings.Join((*calls)[2], " ")
	if !strings.Contains(text, "input text") || !strings.HasSuffix(text, "TERM-7 Carry on") {
		t.Fatalf("text call: %s", text)
	}
	if !strings.Contains(enter, `send key "enter"`) || !strings.HasSuffix(enter, "TERM-7") {
		t.Fatalf("enter call: %s", enter)
	}
}

func TestGhosttyRefusals(t *testing.T) {
	me := os.Getpid()
	cases := map[string]struct {
		fg int
		st *live.State
	}{
		"menu open":       {me, &live.State{Checked: true, Open: true, Source: "command", Session: &live.Session{PID: me, Status: "waiting", WaitingFor: "dialog open"}}},
		"another program": {me + 1, &live.State{Checked: true, Open: true, Source: "command", Session: &live.Session{PID: me, Status: "idle"}}},
		"no agent signal": {me, &live.State{Checked: true, Open: true, Source: "processes", Session: &live.Session{PID: me}}},
	}
	for name, c := range cases {
		calls := fakeGhostty(t, c.fg)
		_, err := Deliver(ghosttyOpts(t, c.st), &core.Job{Harness: "codex", SessionID: "s1"})
		if err == nil {
			t.Errorf("%s: typed", name)
		}
		for _, call := range *calls {
			if strings.Contains(strings.Join(call, " "), "input text") {
				t.Errorf("%s: sent text", name)
			}
		}
	}
}
