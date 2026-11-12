package deliver

import (
	"strings"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/mux"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
)

// fakeProc is one process in a fake process table.
type fakeProc struct {
	pid, ppid int
	name      string
	pane      string // TMUX_PANE, "" for none
	state     string
	started   time.Time
}

// fakePane makes tmux pane %3 show screen and run the processes given,
// and records what is typed into it.
func fakePane(t *testing.T, screen, foreground string, procs ...fakeProc) *[]string {
	t.Helper()
	var sent []string
	oldRunner, oldProcs := mux.Runner, Procs
	t.Cleanup(func() { mux.Runner, Procs = oldRunner, oldProcs })
	mux.Runner = func(_ time.Duration, _ string, argv ...string) (string, error) {
		j := strings.Join(argv, " ")
		switch {
		case strings.Contains(j, "capture-pane"):
			return screen, nil
		case strings.Contains(j, "pane_current_command"):
			return foreground + "\n", nil
		case strings.Contains(j, "send-keys"):
			sent = append(sent, j)
		}
		return "", nil
	}
	byPID := map[int]fakeProc{}
	for _, p := range procs {
		byPID[p.pid] = p
	}
	Procs.Supported = func() bool { return true }
	Procs.List = func() ([]proc.Process, error) {
		var out []proc.Process
		for _, p := range procs {
			out = append(out, proc.Process{PID: p.pid, Args: []string{p.name}, Line: p.name})
		}
		return out, nil
	}
	Procs.Environ = func(pid int) map[string]string {
		if p, ok := byPID[pid]; ok && p.pane != "" {
			return map[string]string{"TMUX_PANE": p.pane, "TMUX": "/tmp/tmux-501/default,1,0"}
		}
		return nil
	}
	Procs.Alive = func(pid int) bool { _, ok := byPID[pid]; return ok }
	Procs.Info = func(pid int) (int, string, bool) {
		p, ok := byPID[pid]
		return p.ppid, p.name, ok
	}
	Procs.State = func(pid int) string { return byPID[pid].state }
	Procs.StartTime = func(pid int) (time.Time, bool) {
		p, ok := byPID[pid]
		return p.started, ok && !p.started.IsZero()
	}
	return &sent
}

const claudeIdle = "╭──────────╮\n│ >        │\n╰──────────╯\n  ? for shortcuts\n"

func paneJob(pid int) *core.Job {
	return &core.Job{Harness: "claude", SessionID: "s1", PID: pid, LimitAt: time.Now().Add(-2 * time.Hour),
		Location: &core.Location{Kind: "tmux", Target: "%3"}}
}

func paneDeliver(t *testing.T, j *core.Job, st *live.State) (Result, error) {
	r, _ := rules.Load("")
	o := Options{Rules: r, Prompt: "Continue.", Strategies: []string{"multiplexer"}, Live: st}
	return Deliver(o, j)
}

func TestNeverTypesIntoAShellLeftInThePane(t *testing.T) {
	// Claude exited; the pane shows zsh with a "❯" prompt and the old frame.
	sent := fakePane(t, claudeIdle+"~/work main\n❯ \n", "zsh",
		fakeProc{pid: 50, name: "zsh", pane: "%3", state: "Ss+"})
	if _, err := paneDeliver(t, paneJob(100), nil); err == nil || len(*sent) > 0 {
		t.Fatalf("typed into a shell: err=%v sent=%q", err, *sent)
	}
}

func TestNeverTypesIntoAnEditor(t *testing.T) {
	sent := fakePane(t, "The counter resets every hour.\n~\n\"notes.md\" 2L\n", "nvim",
		fakeProc{pid: 50, name: "zsh", pane: "%3", state: "Ss"},
		fakeProc{pid: 60, ppid: 50, name: "nvim", pane: "%3", state: "S+"})
	if _, err := paneDeliver(t, paneJob(0), nil); err == nil || len(*sent) > 0 {
		t.Fatalf("typed into vim: err=%v sent=%q", err, *sent)
	}
}

func TestNeverTypesIntoADifferentSessionInTheSamePane(t *testing.T) {
	// s1 ran as pid 100; the user quit it and started a new claude (pid 200).
	sent := fakePane(t, claudeIdle, "claude",
		fakeProc{pid: 200, name: "claude", pane: "%3", state: "S+", started: time.Now().Add(-time.Minute)})
	_, err := paneDeliver(t, paneJob(100), nil)
	if err == nil || len(*sent) > 0 || !strings.Contains(err.Error(), "different") {
		t.Fatalf("typed into another session: err=%v sent=%q", err, *sent)
	}
	// With no pid recorded, a process started after the stall is not it.
	_, err = paneDeliver(t, paneJob(0), nil)
	if err == nil || len(*sent) > 0 {
		t.Fatalf("typed into a session started after the stall: err=%v sent=%q", err, *sent)
	}
}

func TestNeverTypesWhenTheAgentIsSuspendedOrInTheBackground(t *testing.T) {
	for _, state := range []string{"T", "T+", "S"} {
		sent := fakePane(t, claudeIdle, "zsh",
			fakeProc{pid: 100, name: "claude", pane: "%3", state: state})
		if _, err := paneDeliver(t, paneJob(100), nil); err == nil || len(*sent) > 0 {
			t.Fatalf("state %q: typed while the agent wasn't in front: err=%v sent=%q", state, err, *sent)
		}
	}
}

func TestTypesIntoTheSessionsOwnPane(t *testing.T) {
	sent := fakePane(t, claudeIdle, "claude",
		fakeProc{pid: 100, name: "claude", pane: "%3", state: "S+"},
		fakeProc{pid: 101, ppid: 100, name: "claude", pane: "%3", state: "S+"}) // a helper
	if res, err := paneDeliver(t, paneJob(100), nil); err != nil || res.Via != "tmux" || len(*sent) == 0 {
		t.Fatalf("own pane: res=%+v err=%v sent=%q", res, err, *sent)
	}
	// No pid recorded, but the agent in the pane was running before the stall.
	sent = fakePane(t, claudeIdle, "claude",
		fakeProc{pid: 100, name: "claude", pane: "%3", state: "S+", started: time.Now().Add(-5 * time.Hour)})
	j := paneJob(0)
	if res, err := paneDeliver(t, j, nil); err != nil || res.Via != "tmux" || j.PID != 100 {
		t.Fatalf("own pane, no pid: res=%+v err=%v pid=%d", res, err, j.PID)
	}
}

func TestClosedSessionsOldPaneIsLeftAlone(t *testing.T) {
	sent := fakePane(t, claudeIdle, "claude",
		fakeProc{pid: 100, name: "claude", pane: "%3", state: "S+"})
	st := &live.State{Checked: true, Authoritative: true}
	if _, err := paneDeliver(t, paneJob(0), st); err == nil || len(*sent) > 0 {
		t.Fatalf("typed into the pane of a session the agent says is closed: err=%v", err)
	}
}

func TestWithoutProcessInfoTheScreenMustShowTheAgent(t *testing.T) {
	cases := []struct {
		fg, screen string
		ok         bool
	}{
		{"zsh", claudeIdle, false},
		{"claude", "anything", true},
		{"node", "~/work\n❯ \n", false},    // a prompt character alone proves nothing
		{"node", claudeIdle, true},         // "? for shortcuts" is Claude's
		{"node", "resets 10am\n> ", false}, // an old limit message is not the agent
	}
	for _, c := range cases {
		sent := fakePane(t, c.screen, c.fg)
		Procs.Supported = func() bool { return false }
		_, err := paneDeliver(t, paneJob(0), nil)
		if (err == nil) != c.ok || (len(*sent) > 0) != c.ok {
			t.Errorf("fg=%s screen=%q: err=%v sent=%d, want ok=%v", c.fg, c.screen, err, len(*sent), c.ok)
		}
	}
}
