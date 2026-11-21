//go:build darwin || linux

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/store"
)

func TestMain(m *testing.M) {
	if os.Getenv("CONTINUUM_AGENT_HELPER") == "1" {
		time.Sleep(10 * time.Second) // stands in for an agent in a tmux pane
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// A session started before Continuum's hooks has no recorded location; the
// agent process's environment still says which pane it runs in.
func TestPaneFoundFromTheAgentsEnvironment(t *testing.T) {
	// The stand-in runs under the agent's name, as Continuum checks that a
	// recorded pid is still the agent before trusting it.
	self, _ := os.ReadFile(os.Args[0])
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, self, 0o755); err != nil {
		t.Fatal(err)
	}
	agent := exec.Command(bin)
	agent.Env = append(os.Environ(), "CONTINUUM_AGENT_HELPER=1", "TMUX=/tmp/tmux-1/default,1,0", "TMUX_PANE=%9")
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	defer agent.Process.Kill()
	time.Sleep(200 * time.Millisecond)

	h := setup(t)
	ev := limitEvent(h, "old", 30*time.Minute)
	ev.Location = nil
	ev.PID = 0
	h.d.Handle(ev)
	h.live = &live.State{Checked: true, Open: true, Source: "command", Session: &live.Session{SessionID: "old", PID: agent.Process.Pid, Status: "idle"}}
	h.advance(37 * time.Minute) // past the reset and Claude's 5-minute grace
	if len(h.delivered) != 1 {
		t.Fatalf("delivered %d", len(h.delivered))
	}
	if loc := h.delivered[0].Location; loc == nil || loc.Kind != "tmux" || loc.Target != "%9" {
		t.Fatalf("location %v", loc)
	}
}

func TestResumeFollowsReopenedCodexWrapper(t *testing.T) {
	h := setup(t)
	h.d.LiveFn = h.d.liveState
	id := core.NewID()
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, self, 0o755); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(h.d.Paths.Wrappers, "current")
	if err := os.MkdirAll(current, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
		t.Fatal(err)
	}
	agent := exec.Command(bin, "resume", id)
	agent.Env = append(os.Environ(), "CONTINUUM_AGENT_HELPER=1", "CONTINUUM_WRAPPER_ID=current")
	if err := agent.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = agent.Process.Kill()
		_ = agent.Wait()
	})
	old := &core.Location{Kind: "wrapper", Target: "closed"}
	key := store.SessionKey("codex", id)
	h.d.Sessions[key] = &store.Session{Harness: "codex", SessionID: id, Location: old, LastSeen: h.now}
	j := &core.Job{Key: key, Harness: "codex", SessionID: id, State: core.StatePending,
		Kind: core.KindUsageLimit, Location: old, WaitingOpen: true, LimitAt: h.now, DueAt: h.now}
	h.d.Jobs[key] = j
	h.d.runDue()
	if len(h.delivered) != 1 || j.State != core.StateDelivered || j.WaitingOpen ||
		j.Location.String() != "wrapper:current" || j.PID != agent.Process.Pid {
		t.Fatalf("expected delivery through the current wrapper, got %+v", j)
	}
}
