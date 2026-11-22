//go:build realagents

// Run with: go test -tags realagents ./internal/deliver -run Real -v
// Starts the real Claude Code and Codex TUIs in a hidden tmux server, checks
// that Continuum recognises their panes, and types into their composers
// without pressing Enter (nothing is sent to a model).
package deliver

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/mux"
	"github.com/continuum-dev/continuum/internal/rules"
)

func TestRealAgentPanes(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	r, err := rules.Load("")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	sock := "continuum-real-" + core.NewID()
	tm := func(args ...string) string {
		out, err := exec.Command("tmux", append([]string{"-L", sock}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %v %s", args, err, out)
		}
		return string(out)
	}
	defer exec.Command("tmux", "-L", sock, "kill-server").Run()

	agents := map[string]string{"claude": "claude", "codex": "codex"}
	started := map[string]string{}
	first := true
	for harness, bin := range agents {
		if _, err := exec.LookPath(bin); err != nil {
			t.Logf("%s not installed, skipped", bin)
			continue
		}
		cmd := "env -u CLAUDECODE -u CLAUDE_CODE_ENTRYPOINT " + bin
		var pane string
		if first {
			pane = strings.TrimSpace(tm("new-session", "-d", "-P", "-F", "#{pane_id}", "-x", "160", "-y", "45", "-c", dir, cmd))
			first = false
		} else {
			pane = strings.TrimSpace(tm("new-window", "-d", "-P", "-F", "#{pane_id}", "-c", dir, cmd))
		}
		started[harness] = pane
	}
	if len(started) == 0 {
		t.Skip("no agents installed")
	}
	sockPath := strings.TrimSpace(tm("display-message", "-p", "#{socket_path}"))
	time.Sleep(6 * time.Second)

	for harness, pane := range started {
		loc := &core.Location{Kind: "tmux", Target: pane, Socket: sockPath}
		h := r.Harness(harness)
		name, _ := mux.ProcessName(r, loc)
		screen, _ := mux.Capture(r, loc)
		if strings.Contains(strings.ToLower(screen), "trust") {
			// First run in a new folder: accept the trust prompt so the composer shows.
			tm("send-keys", "-t", pane, "Enter")
			time.Sleep(3 * time.Second)
		}
		if err := VerifyPane(r, h, nil, loc); err != nil && strings.Contains(err.Error(), "menu or prompt") {
			// e.g. Codex's "Update available" menu, where Enter means "Update now".
			t.Logf("%s: correctly refused to type into an open menu: %v", harness, err)
			tm("send-keys", "-t", pane, "Escape")
			time.Sleep(3 * time.Second)
		}
		if err := VerifyPane(r, h, nil, loc); err != nil {
			after, _ := mux.Capture(r, loc)
			t.Errorf("%s (process %q): %v\n%s", harness, name, err, tail(after))
			continue
		}
		t.Logf("%s: pane %s recognised (process %q)", harness, pane, name)
		// Type without Enter.
		probe := "continuum-probe-" + harness
		if _, err := mux.Runner(5*time.Second, "", "tmux", "-S", sockPath, "send-keys", "-t", pane, "-l", "--", probe); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1500 * time.Millisecond)
		after, _ := mux.Capture(r, loc)
		if !strings.Contains(after, probe) {
			t.Errorf("%s: typed text did not reach the composer\n%s", harness, tail(after))
		} else {
			t.Logf("%s: typed text reached the composer while the session was detached", harness)
		}
		// Clear it again.
		tm("send-keys", "-t", pane, "C-u")
	}
	_ = os.Getenv
}

func tail(s string) string {
	l := rules.TailLines(s, 12)
	return strings.Join(l, "\n")
}
