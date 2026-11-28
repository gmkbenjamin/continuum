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

// A non-screen-detection agent (like Antigravity CLI) must not re-report the
// same limit on a later redraw, but must report a new limit after Enter.
func TestWrapperDedupAcrossRedrawsAndResetOnEnter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CONTINUUM_HOME", home)
	in, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer inW.Close()
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer devnull.Close()

	// 1. Print limit #1, settle (2.2s), redraw with different spinner (still same limit), settle (3.0s).
	// 2. Receive Enter once the redraw has had time to settle, then print same limit line again and settle.
	script := `printf '⣾\n⚠ Individual quota reached. Resets in 46m8s.\nError ID: e-1\n'; sleep 2.2; ` +
		`printf '⣯\n⚠ Individual quota reached. Resets in 46m8s.\nError ID: e-1\n'; sleep 3.5; ` +
		`printf '⚠ Individual quota reached. Resets in 46m8s.\nError ID: e-1\n'; sleep 2.2`
	go func() {
		// Wait for the first usage_limit event to be spooled, then let the
		// redraw at 2.2s also settle (1.5s + tick margin) before sending Enter.
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			files, _ := filepath.Glob(filepath.Join(home, "spool", "*.json"))
			for _, f := range files {
				if data, err := os.ReadFile(f); err == nil {
					var ev core.Event
					if json.Unmarshal(data, &ev) == nil && ev.Kind == core.KindUsageLimit {
						time.Sleep(3200 * time.Millisecond)
						_, _ = inW.Write([]byte("retry\r"))
						return
					}
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		_, _ = inW.Write([]byte("retry\r"))
	}()
	if _, err := Run(Options{Harness: "antigravity", Argv: []string{"/bin/sh", "-c", script}, Stdin: in, Stdout: devnull}); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(home, "spool", "*.json"))
	limits := 0
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var ev core.Event
		if json.Unmarshal(data, &ev) == nil && ev.Kind == core.KindUsageLimit {
			limits++
		}
	}
	if limits != 2 {
		t.Fatalf("got %d usage_limit events, want 2 (1 initial, 0 for redraw, 1 after Enter)", limits)
	}
}

// fakeLineEditorScript is a tiny python3 raw-mode TUI that supports cursor
// movement (\x1b[D/\x1b[C), Ctrl+A/E/U/K, Backspace, Up-arrow recall, and
// Enter submission, recording submitted lines and final unsubmitted draft.
const fakeLineEditorScript = `
import os, sys, tty, termios, select

out_path = sys.argv[1]
recalled = sys.argv[2] if len(sys.argv) > 2 else ""
fd = sys.stdin.fileno()
old = termios.tcgetattr(fd)
tty.setraw(fd)
buf = []
cur = 0
submitted = []
in_paste = False

sys.stdout.write("\x1b[?2004h")

def render():
    sys.stdout.write("\x1b[H\x1b[J> " + "".join(buf).replace("\n", "\r\n  "))
    sys.stdout.flush()

render()
try:
    while True:
        r, _, _ = select.select([fd], [], [], 0.2)
        if not r:
            continue
        ch = os.read(fd, 4096)
        if not ch:
            break
        i = 0
        while i < len(ch):
            b = ch[i]
            if ch[i:i+6] == b"\x1b[200~":
                in_paste = True
                i += 6; continue
            if ch[i:i+6] == b"\x1b[201~":
                in_paste = False
                render()
                i += 6; continue
            if in_paste:
                buf.insert(cur, chr(b)); cur += 1; i += 1; continue
            if b == 0x1b:
                if ch[i:i+3] == b"\x1b[D":
                    if cur > 0: cur -= 1
                    i += 3; continue
                if ch[i:i+3] == b"\x1b[C":
                    if cur < len(buf): cur += 1
                    i += 3; continue
                if ch[i:i+3] == b"\x1b[A":
                    buf = list(recalled)
                    cur = len(buf)
                    render()
                    i += 3; continue
                i += 1; continue
            if b == 10:
                buf.insert(cur, "\n"); cur += 1; render()
                i += 1; continue
            if b == 13 and cur > 0 and buf[cur-1] == chr(92):
                buf.insert(cur, "\n"); cur += 1; render()
                i += 1; continue
            if b == 13:
                line = "".join(buf)
                submitted.append(line)
                buf = []
                cur = 0
                render()
                i += 1; continue
            if b == 1: # Ctrl+A
                cur = 0; i += 1; continue
            if b == 5: # Ctrl+E
                cur = len(buf); i += 1; continue
            if b == 21: # Ctrl+U
                buf = buf[cur:]; cur = 0; render(); i += 1; continue
            if b == 11: # Ctrl+K
                buf = buf[:cur]; render(); i += 1; continue
            if b in (127, 8):
                if cur > 0:
                    del buf[cur-1]; cur -= 1; render()
                i += 1; continue
            if b == 4: # Ctrl+D exit sentinel
                raise EOFError()
            if b >= 32:
                buf.insert(cur, chr(b)); cur += 1; render()
            i += 1
except EOFError:
    pass
finally:
    termios.tcsetattr(fd, termios.TCSADRAIN, old)
    with open(out_path, "w") as f:
        for s in submitted:
            f.write("SUBMIT:" + s + "\n")
        f.write("DRAFT:" + "".join(buf) + "\n")
`

func runDraftScenario(t *testing.T, mode string, userInput []byte, recalled string, duringDelivery ...string) string {
	t.Helper()
	oldClear, oldSubmit, oldRestore := draftClearDelay, draftSubmitDelay, draftRestoreDelay
	draftClearDelay, draftSubmitDelay, draftRestoreDelay = 40*time.Millisecond, 120*time.Millisecond, 120*time.Millisecond
	t.Cleanup(func() {
		draftClearDelay, draftSubmitDelay, draftRestoreDelay = oldClear, oldSubmit, oldRestore
	})

	home := t.TempDir()
	t.Setenv("CONTINUUM_HOME", home)
	outFile := filepath.Join(t.TempDir(), "editor.out")
	in, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	defer inW.Close()
	devnull, _ := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	defer devnull.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = Run(Options{
			Harness: "antigravity",
			Argv:    []string{"python3", "-c", fakeLineEditorScript, outFile, recalled},
			Stdin:   in,
			Stdout:  devnull,
		})
	}()

	var inbox string
	for i := 0; i < 60 && inbox == ""; i++ {
		time.Sleep(50 * time.Millisecond)
		if m, _ := filepath.Glob(filepath.Join(home, "wrappers", "*", "inbox")); len(m) == 1 {
			inbox = m[0]
		}
	}
	if inbox == "" {
		t.Fatal("wrapper inbox not created")
	}
	// Let the editor initialize raw mode, then send user's unsubmitted draft.
	time.Sleep(250 * time.Millisecond)
	if _, err := inW.Write(userInput); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)

	reqData, _ := json.Marshal(map[string]string{
		"id":         "r1",
		"text":       "Continue.",
		"draft_mode": mode,
	})
	reqPath := filepath.Join(inbox, "r1.json")
	if err := os.WriteFile(reqPath, reqData, 0o600); err != nil {
		t.Fatal(err)
	}
	if len(duringDelivery) > 0 {
		for i := 0; i < 150; i++ {
			if _, err := os.Stat(reqPath + ".inflight"); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := inW.WriteString(duringDelivery[0]); err != nil {
			t.Fatal(err)
		}
	}
	// Wait until the wrapper finishes delivering the claimed request.
	for i := 0; i < 60; i++ {
		_, pending := os.Stat(reqPath)
		_, claimed := os.Stat(reqPath + ".inflight")
		if os.IsNotExist(pending) && os.IsNotExist(claimed) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, _ = inW.Write([]byte{0x04}) // Ctrl+D exits fake editor
	<-done

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read editor output: %v", err)
	}
	return string(data)
}

func TestWrapperDraftRestoreMode(t *testing.T) {
	// Cursor sits right in the middle of "banked": "when click on bank|ed reset"
	input := append([]byte("when click on baned reset"), []byte("\x1b[D\x1b[D\x1b[D\x1b[D\x1b[D\x1b[D\x1b[D\x1b[Dk")...)
	got := runDraftScenario(t, core.DraftRestore, input, "")
	want := "SUBMIT:Continue.\nDRAFT:when click on banked reset\n"
	if got != want {
		t.Fatalf("restore mode mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestWrapperDraftSubmitMode(t *testing.T) {
	input := append([]byte("when click on baned reset"), []byte("\x1b[D\x1b[D\x1b[D\x1b[D\x1b[D\x1b[D\x1b[D\x1b[Dk")...)
	got := runDraftScenario(t, core.DraftSubmit, input, "")
	want := "SUBMIT:when click on banked reset\nSUBMIT:Continue.\nDRAFT:\n"
	if got != want {
		t.Fatalf("submit mode mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestWrapperDraftUncertainFallsBackToScreen(t *testing.T) {
	// User pressed Up arrow to recall a draft from agent history.
	got := runDraftScenario(t, core.DraftRestore, []byte("\x1b[A"), "recalled draft prompt")
	want := "SUBMIT:Continue.\nDRAFT:recalled draft prompt\n"
	if got != want {
		t.Fatalf("screen fallback restore mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestWrapperMultilineDraftAndConcurrentTyping(t *testing.T) {
	got := runDraftScenario(t, core.DraftRestore, []byte("\x1b[200~first line\nsecond line\x1b[201~"), "", "!")
	if want := "SUBMIT:Continue.\nDRAFT:first line\nsecond line!\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestWrapperSubmitStopsWhenEnterKeepsDraftOpen(t *testing.T) {
	got := runDraftScenario(t, core.DraftSubmit, []byte("draft\\"), "")
	if want := "DRAFT:draft\\\n\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
