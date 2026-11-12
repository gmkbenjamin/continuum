//go:build darwin || linux

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/live"
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
