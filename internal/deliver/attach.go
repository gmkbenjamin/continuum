package deliver

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/continuum-dev/continuum/internal/ansi"
	"github.com/continuum-dev/continuum/internal/rules"
)

// Timings for typing through an attached terminal; tests shorten them.
var (
	attachReadyTimeout = 30 * time.Second
	attachQuiet        = 1500 * time.Millisecond
	attachAfterEnter   = 3 * time.Second
)

// typeViaPTY runs argv (a command that opens the session interactively, such
// as `claude attach <id>`) in a terminal Continuum owns, waits for the agent's
// input box, refuses if a menu or prompt is open, types the prompt and Enter,
// then closes that terminal. The session itself keeps running.
func typeViaPTY(r *rules.Rules, h *rules.Harness, argv []string, dir string, env []string, prompt string) error {
	c, err := startPTY(argv, dir, env)
	if err != nil {
		return err
	}
	defer c.stop()

	var (
		mu     sync.Mutex
		raw    bytes.Buffer
		last   = time.Now()
		exited bool
	)
	ring := ansi.NewRing(32 * 1024)
	go func() {
		buf := make([]byte, 8192)
		for {
			n, err := c.rw.Read(buf)
			if n > 0 {
				chunk := buf[:n]
				mu.Lock()
				ring.Write(chunk)
				raw.Write(chunk)
				last = time.Now()
				mu.Unlock()
				answerQueries(c, chunk)
			}
			if err != nil {
				mu.Lock()
				exited = true
				mu.Unlock()
				return
			}
		}
	}()
	screen := func() string {
		mu.Lock()
		defer mu.Unlock()
		return ansi.Strip(ring.String())
	}

	deadline := time.Now().Add(attachReadyTimeout)
	ready := false
	for time.Now().Before(deadline) {
		time.Sleep(150 * time.Millisecond)
		mu.Lock()
		quiet, gone := time.Since(last) >= attachQuiet, exited
		mu.Unlock()
		tail := strings.Join(rules.TailLines(screen(), 60), "\n")
		if gone {
			return fmt.Errorf("%s exited: %s", strings.Join(argv[:min(2, len(argv))], " "), lastLine(tail))
		}
		if quiet && (h.HasUIMarker(tail) || r.BlockingDialog(tail) != "") {
			ready = true
			break
		}
	}
	if !ready {
		return fmt.Errorf("%s did not show %s's input box", strings.Join(argv[:min(2, len(argv))], " "), h.DisplayName)
	}
	if d := r.BlockingDialog(screen()); d != "" {
		return fmt.Errorf("the session has a menu or prompt open (%q); not typing into it", strings.TrimSpace(d))
	}
	if prompt == "" { // probe only: reached the input box, type nothing
		return nil
	}
	if _, err := c.rw.Write([]byte(prompt)); err != nil {
		return err
	}
	time.Sleep(time.Duration(max(r.Defaults.EnterDelayMS, 100)) * time.Millisecond)
	if _, err := c.rw.Write([]byte("\r")); err != nil {
		return err
	}
	time.Sleep(attachAfterEnter)
	return nil
}

// answerQueries replies to the terminal queries a TUI may wait on, since no
// real terminal is attached: cursor position and device attributes.
func answerQueries(c *ptyChild, chunk []byte) {
	if bytes.Contains(chunk, []byte("\x1b[6n")) {
		_, _ = c.rw.Write([]byte("\x1b[40;1R"))
	}
	if bytes.Contains(chunk, []byte("\x1b[c")) || bytes.Contains(chunk, []byte("\x1b[0c")) {
		_, _ = c.rw.Write([]byte("\x1b[?62;22c"))
	}
}

func lastLine(s string) string {
	lines := rules.TailLines(s, 3)
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return "(no output)"
}
