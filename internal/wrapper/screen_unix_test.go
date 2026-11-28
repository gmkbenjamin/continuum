//go:build !windows

package wrapper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
)

// The wrapper starts agents through its own program's __pty-helper; in
// tests that program is the test binary.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "__pty-helper" && os.Args[2] == "--" {
		os.Exit(Helper(os.Args[3:]))
	}
	os.Exit(m.Run())
}

// runFake runs a shell script under the wrapper and returns the events it
// reported. send, if set, is written to the agent's input after delay.
func runFake(t *testing.T, script, send string, delay time.Duration) []core.Event {
	home := t.TempDir()
	t.Setenv("CONTINUUM_HOME", home)
	in, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer devnull.Close()
	go func() {
		if send != "" {
			time.Sleep(delay)
			inW.Write([]byte(send))
		}
	}()
	defer inW.Close()
	if _, err := Run(Options{Harness: "opencode", Argv: []string{"/bin/sh", "-c", script}, Stdin: in, Stdout: devnull}); err != nil {
		t.Fatal(err)
	}
	var evs []core.Event
	files, _ := filepath.Glob(filepath.Join(home, "spool", "*.json"))
	for _, f := range files {
		var e core.Event
		data, _ := os.ReadFile(f)
		if json.Unmarshal(data, &e) == nil {
			evs = append(evs, e)
		}
	}
	return evs
}

const drawLimit = `printf '\033[?1049h\033[2J\033[5;6HError: You'\''ve reached your usage limit. Your limit resets at 4:00 PM.\033[40;1H'; `

// opencode draws by moving the cursor and redraws its title many times
// after an error: the message is only whole on the rendered screen.
func TestLimitDrawnByCursorIsDetected(t *testing.T) {
	script := drawLimit + `i=0; while [ $i -lt 30 ]; do printf '\033[1;3HNew session %d\n' $i; i=$((i+1)); done; sleep 3`
	evs := runFake(t, script, "", 0)
	found := false
	for _, e := range evs {
		if e.Kind == core.KindUsageLimit && e.ResetAt.Hour() == 16 {
			found = true
		}
	}
	if !found {
		t.Fatalf("limit not detected: %+v", evs)
	}
}

// A limit message still on screen when the user sends a new prompt is
// history, not a new stall.
func TestLimitOnScreenBeforeAPromptIsStale(t *testing.T) {
	script := drawLimit + `i=0; while [ $i -lt 20 ]; do printf '\033[1;3HNew session %d' $i; sleep 0.1; i=$((i+1)); done; sleep 3`
	evs := runFake(t, script, "carry on\r", 500*time.Millisecond)
	for _, e := range evs {
		if e.Kind == core.KindUsageLimit {
			t.Fatalf("old message reported again: %+v", e)
		}
	}
}

func TestFreshLines(t *testing.T) {
	stale := map[string]int{"Error: limit": 1}
	if got := freshLines("a\nError: limit\nb", stale); got != "a\nb" {
		t.Fatalf("old copy kept: %q", got)
	}
	// Hit again after a resume: the second copy is new.
	if got := freshLines("a\nError: limit\nresume\nError:  limit\nb", stale); got != "a\nresume\nError:  limit\nb" {
		t.Fatalf("new copy dropped: %q", got)
	}
	// The old copy scrolled away: forgotten, so a later copy counts.
	freshLines("x\ny", stale)
	if stale["Error: limit"] != 0 {
		t.Fatalf("stale count kept: %v", stale)
	}
	if got := freshLines("Error: limit", stale); got != "Error: limit" {
		t.Fatalf("new copy after scroll dropped: %q", got)
	}
}

// The same limit shown again after a new prompt is a new stall, even with
// the old message still on screen above it.
func TestSameLimitAgainAfterAPromptIsNew(t *testing.T) {
	script := drawLimit + `sleep 4; printf '\033[7;6HError: You'\''ve reached your usage limit. Your limit resets at 4:00 PM.\033[40;1H'; sleep 3`
	evs := runFake(t, script, "carry on\r", 3*time.Second)
	n := 0
	for _, e := range evs {
		if e.Kind == core.KindUsageLimit {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d limit reports, want 2 (the first, and the same one again after the prompt): %+v", n, evs)
	}
}

// Agents without screen detection keep the stream-only behaviour.
func TestScreenDetectionIsPerAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CONTINUUM_HOME", home)
	in, inW, _ := os.Pipe()
	defer inW.Close()
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer devnull.Close()
	script := drawLimit + `i=0; while [ $i -lt 30 ]; do printf '\033[1;3HNew session %d\n' $i; i=$((i+1)); done; sleep 3`
	if _, err := Run(Options{Harness: "aider", Argv: []string{"/bin/sh", "-c", script}, Stdin: in, Stdout: devnull}); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(home, "spool", "*.json"))
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var ev core.Event
		if json.Unmarshal(data, &ev) == nil && ev.Kind != core.KindWrapperStart && ev.Kind != core.KindWrapperExit {
			t.Fatalf("aider reported from the rendered screen: %+v", ev)
		}
	}
}

// A menu a full-screen program draws by moving the cursor is open on the
// rendered screen: nothing is typed into it.
func TestMenuDrawnByCursorBlocksTyping(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CONTINUUM_HOME", home)
	in, inW, _ := os.Pipe()
	defer inW.Close()
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer devnull.Close()
	// The question and its options drawn cell by cell, bottom up, then a
	// title redrawn many times (the stream's tail holds only the title).
	script := `printf '\033[?1049h\033[2J\033[12;3HSkip\033[11;3HRun it\033[10;10Hwant to run this command\033[10;3HDo you'; ` +
		`i=0; while [ $i -lt 40 ]; do printf '\033[1;3HNew session %d\n' $i; i=$((i+1)); done; sleep 4`
	done := make(chan struct{})
	go func() {
		defer close(done)
		Run(Options{Harness: "opencode", Argv: []string{"/bin/sh", "-c", script}, Stdin: in, Stdout: devnull})
	}()
	var inbox string
	for i := 0; i < 50 && inbox == ""; i++ {
		time.Sleep(50 * time.Millisecond)
		m, _ := filepath.Glob(filepath.Join(home, "wrappers", "*", "inbox"))
		if len(m) == 1 {
			inbox = m[0]
		}
	}
	if inbox == "" {
		t.Fatal("no wrapper inbox")
	}
	time.Sleep(1500 * time.Millisecond)
	os.WriteFile(filepath.Join(inbox, "r1.json"), []byte(`{"id":"r1","text":"continue"}`), 0o600)
	for i := 0; i < 40; i++ {
		if _, err := os.Stat(filepath.Join(inbox, "r1.rejected")); err == nil {
			<-done
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	<-done
	t.Fatal("typed into an open menu")
}
