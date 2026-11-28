// Package wrapper implements `continuum run -- <agent>`: it runs the agent in
// a pseudo-terminal Continuum owns, passes all input and output through
// unchanged, watches the output for limit messages and types the resume prompt
// when the daemon asks. Works in any terminal emulator, focused or not.
package wrapper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/continuum-dev/continuum/internal/ansi"
	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/messages"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/resettime"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/scope"
	"golang.org/x/term"
)

// ptyProcess is the per-OS pseudo-terminal child.
type ptyProcess interface {
	io.Reader
	io.Writer
	Resize(cols, rows int) error
	Wait() (int, error)
	Close() error
	// OnStop is called when the agent suspends itself (Ctrl+Z).
	OnStop(func())
	// Signal relays a signal to the agent.
	Signal(os.Signal)
}

type Options struct {
	Harness string // empty: guess from the command name
	Argv    []string
	Stdin   *os.File
	Stdout  *os.File
	// Choose adds this session to the chosen sessions (scope "chosen") for
	// as long as it runs.
	Choose bool
	// Message is the resume message for this session only (also
	// CONTINUUM_MESSAGE).
	Message string
}

// DetectHarness guesses the harness from the command being run.
func DetectHarness(r *rules.Rules, argv []string) string {
	if len(argv) == 0 {
		return "generic"
	}
	command := strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe")
	for _, name := range r.HarnessNames() {
		if name == "generic" {
			continue
		}
		h := r.Harness(name)
		if h.IsHarnessProcess(argv[0]) || strings.EqualFold(command, h.Binary) {
			return name
		}
		for _, alias := range h.CommandAliases {
			if !strings.EqualFold(command, alias.Command) {
				continue
			}
			if path, err := proc.LookPath(argv[0]); err == nil {
				if resolved, err := filepath.EvalSymlinks(path); err == nil && alias.Matches(resolved) {
					return name
				}
			}
		}
	}
	return "generic"
}

// Run starts the agent and blocks until it exits, returning its exit code.
func Run(o Options) (int, error) {
	if len(o.Argv) == 0 {
		return 1, fmt.Errorf("empty command")
	}
	p := core.GetPaths()
	if err := p.Ensure(); err != nil {
		return 1, err
	}
	r, rerr := rules.Cached(p.Rules)
	if r == nil {
		return 1, rerr
	}
	harness := o.Harness
	if harness == "" {
		harness = DetectHarness(r, o.Argv)
	}
	h := r.Harness(harness)

	id := core.NewID()
	dir := deliver.WrapperDir(p, id)
	inbox := filepath.Join(dir, "inbox")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		return 1, err
	}
	defer os.RemoveAll(dir)
	_ = os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	cwd, _ := os.Getwd()
	started := time.Now()
	meta, _ := json.Marshal(map[string]any{"harness": harness, "argv": o.Argv, "cwd": cwd, "started": started})
	_ = os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o600)
	_ = core.SpoolEvent(p, core.Event{
		Harness:  harness,
		Kind:     core.KindWrapperStart,
		At:       started,
		Source:   "wrapper",
		Location: &core.Location{Kind: "wrapper", Target: id},
		Cwd:      cwd,
		PID:      os.Getpid(),
	})
	msg := o.Message
	if msg == "" {
		msg = os.Getenv("CONTINUUM_MESSAGE")
	}
	if msg != "" {
		if text, err := messages.Clean(msg); err == nil {
			_ = messages.Update(p, func(m *messages.Messages) (bool, error) {
				m.Set(scope.Entry{Harness: harness, Wrapper: id, Label: filepath.Base(o.Argv[0]) + " in " + cwd}, messages.Pair{Text: text})
				return true, nil
			})
		}
	}
	if o.Choose || os.Getenv("CONTINUUM_CHOOSE") != "" {
		_, _ = scope.Update(p, func(sc scope.Scope) (scope.Scope, bool, error) {
			var changed bool
			sc.Chosen, changed = scope.Add(sc.Chosen, scope.Entry{Harness: harness, Wrapper: id, Label: filepath.Base(o.Argv[0]) + " in " + cwd})
			return sc, changed, nil
		})
	}

	env := append(os.Environ(), "CONTINUUM_WRAPPER_ID="+id)
	cols, rows := 120, 40
	interactive := term.IsTerminal(int(o.Stdin.Fd()))
	if interactive {
		if c, rr, err := term.GetSize(int(o.Stdout.Fd())); err == nil {
			cols, rows = c, rr
		}
	}
	child, err := start(o.Argv, env, cols, rows)
	if err != nil {
		return 127, fmt.Errorf("start %s: %w", strings.Join(o.Argv, " "), err)
	}
	defer child.Close()
	defer forwardSignals(child)()

	var restore func()
	if interactive {
		fd := int(o.Stdin.Fd())
		if st, err := term.MakeRaw(fd); err == nil {
			restore = func() { _ = term.Restore(fd, st) }
			defer func() { restore() }()
		}
		stopResize := watchResize(o.Stdout, child)
		defer stopResize()
		child.OnStop(func() {
			// The agent suspended itself (Ctrl+Z): let its last output reach
			// the screen, give the terminal back, and stop like a job.
			time.Sleep(80 * time.Millisecond)
			if restore != nil {
				restore()
			}
			suspendSelf()
			// Continued (fg): take the terminal again, at its current size.
			if st, err := term.MakeRaw(fd); err == nil {
				restore = func() { _ = term.Restore(fd, st) }
			}
			if c, r, err := term.GetSize(int(o.Stdout.Fd())); err == nil {
				_ = child.Resize(c, r)
			}
		})
	}

	var (
		mu         sync.Mutex
		ring       = ansi.NewRing(16 * 1024)
		sinceInput = ansi.NewRing(16 * 1024) // output since the user last typed
		// What the agent shows: full-screen programs that redraw by moving
		// the cursor (opencode) leave a limit message in the stream as
		// fragments, pushed out of its tail by later redraws.
		screen = ansi.NewScreen(cols, rows)
		// screenOn: screen detection is wanted for this agent and the screen
		// model hasn't failed (a fault in it turns it off; the agent's
		// session must never suffer for it).
		screenOn = h.ScreenDetection
		// Limit messages on screen when the user last sent a prompt or
		// Continuum last resumed: they stay visible in the history and must
		// not count again.
		stale = map[string]int{} // line -> how many times it was shown
		// Limits found on the rendered screen, reported since the last
		// prompt or resume (the screen keeps showing them).
		reportedShown = map[string]time.Time{}
		lastOutput    time.Time
		changed       bool
		writeMu       sync.Mutex
	)
	// The screen model's operations, mu held.
	guard := func(f func()) {
		if !screenOn {
			return
		}
		defer func() {
			if recover() != nil {
				screenOn = false
			}
		}()
		f()
	}
	shownText := func() (text string) {
		guard(func() { text = screen.String() })
		return text
	}
	markStale := func() { // mu held
		reportedShown = map[string]time.Time{}
		stale = map[string]int{}
		if !screenOn {
			return
		}
		for _, l := range strings.Split(shownText(), "\n") {
			if h.Classify(l, 1) != nil {
				stale[normalise(l)]++
			}
		}
	}
	write := func(b []byte) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_, _ = child.Write(b)
	}

	// stdin -> child
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := o.Stdin.Read(buf)
			if n > 0 {
				write(buf[:n])
				// A menu shown before this was answered (or dismissed) by it.
				mu.Lock()
				sinceInput = ansi.NewRing(16 * 1024)
				if bytes.ContainsAny(buf[:n], "\r\n") {
					// A submitted prompt starts a new turn; older limit text
					// on screen must not be re-detected from the ring.
					ring = ansi.NewRing(16 * 1024)
					markStale()
				}
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	// child -> stdout, and into the ring for detection
	outDone := make(chan struct{})
	go func() {
		defer close(outDone)
		buf := make([]byte, 32*1024)
		for {
			n, err := child.Read(buf)
			if n > 0 {
				_, _ = o.Stdout.Write(buf[:n])
				mu.Lock()
				ring.Write(buf[:n])
				sinceInput.Write(buf[:n])
				guard(func() { screen.Write(buf[:n]) })
				lastOutput = time.Now()
				changed = true
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		enterDelay := time.Duration(r.Defaults.EnterDelayMS) * time.Millisecond
		reported := map[string]time.Time{} // signature -> last report
		stalled := false
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
			}
			// Deliveries from the daemon.
			for _, req := range readInbox(inbox) {
				mu.Lock()
				screen := ansi.Strip(ring.String())
				recent := ansi.Strip(sinceInput.String())
				shown := shownText()
				mu.Unlock()
				// The bottom of the screen, and anything shown since the user
				// last typed: a menu stays open until answered, even when
				// later redraws (a spinner, a timer) push it out of the tail.
				dialog := r.BlockingDialog(screen)
				if dialog == "" {
					dialog = r.BlockingDialogAnywhere(recent)
				}
				if dialog == "" && shown != "" {
					// A full-screen program draws its menus by moving the
					// cursor too: the rendered screen shows them whole.
					dialog = r.BlockingDialog(shown)
				}
				if dialog != "" {
					// Write the note before removing the request: the daemon
					// treats a vanished request without a note as delivered.
					note := fmt.Sprintf("a menu or prompt is open (%q); not typing into it", dialog)
					_ = os.WriteFile(filepath.Join(inbox, req.ID+".rejected"), []byte(note), 0o600)
					os.Remove(req.path)
					continue
				}
				write([]byte(req.Text))
				time.Sleep(enterDelay)
				write([]byte("\r"))
				os.Remove(req.path)
				stalled = false
				// Only output produced after the resume can contain a new limit;
				// the old message may still be on screen and must not count again.
				mu.Lock()
				ring = ansi.NewRing(16 * 1024)
				sinceInput = ansi.NewRing(16 * 1024)
				markStale()
				changed = false
				mu.Unlock()
			}
			if interactive && h.ScreenDetection {
				if c, rr, err := term.GetSize(int(o.Stdout.Fd())); err == nil {
					mu.Lock()
					guard(func() { screen.Resize(c, rr) })
					mu.Unlock()
				}
			}
			// Old copies of a stale message that scrolled away are forgotten
			// on every tick, not only when the screen settles, so one shown
			// again later is new; only between frames (no output for a
			// moment), never half way through a redraw.
			mu.Lock()
			if len(stale) > 0 && time.Since(lastOutput) > 200*time.Millisecond {
				forget(shownText(), stale)
			}
			mu.Unlock()
			// Detection once the screen has settled.
			mu.Lock()
			settled := changed && time.Since(lastOutput) > 1500*time.Millisecond
			var text, shown string
			var shownSeen map[string]time.Time
			if settled {
				changed = false
				text = ansi.Strip(ring.String())
				if screenOn {
					shown = freshLines(shownText(), stale)
					shownSeen = reportedShown
				}
			}
			mu.Unlock()
			if !settled {
				continue
			}
			m := h.Classify(text, min(r.Defaults.TailLines, 12))
			seen := reported
			fromScreen := false
			now := time.Now()
			dup := false
			if shownSeen != nil {
				if m != nil {
					if last, ok := reported[signature(m)]; ok && now.Sub(last) < 15*time.Minute {
						m, dup = nil, true // the screen knows whether it is new
					}
				}
				if m == nil {
					// Lines shown before the last prompt or resume are left
					// out, so a match here is new since then.
					m = h.Classify(shown, min(r.Defaults.TailLines, 12))
					seen, fromScreen = shownSeen, true
				}
			}
			if m == nil {
				if !dup && stalled && strings.TrimSpace(text) != "" {
					stalled = false
					_ = core.SpoolEvent(p, core.Event{
						Harness: harness, Kind: core.KindActivity, At: time.Now(), Source: "wrapper",
						Location: &core.Location{Kind: "wrapper", Target: id}, Cwd: cwd,
					})
				}
				continue
			}
			// A screen redraw repeats the same message; report it again only
			// after a while (a genuinely new limit names a new reset time).
			sig := signature(m)
			if last, ok := seen[sig]; ok && now.Sub(last) < 15*time.Minute {
				continue
			}
			seen[sig] = now
			if shownSeen != nil && !fromScreen {
				// The same message on the rendered screen is this report too.
				if ms := h.Classify(shown, min(r.Defaults.TailLines, 12)); ms != nil {
					shownSeen[signature(ms)] = now
				}
			}
			stalled = true
			ev := core.Event{
				Harness: harness, Kind: m.Kind, Message: m.Text, At: now, Source: "wrapper",
				Location: &core.Location{Kind: "wrapper", Target: id}, Cwd: cwd, Hit: m.Hit,
			}
			if h.NeverResume(m.Text) {
				ev.Kind = core.KindNeverResume
			}
			if ev.Kind == core.KindUsageLimit {
				if t, ok := resettime.Parse(m.Text, now, time.Local); ok {
					ev.ResetAt = t
				}
			}
			_ = core.SpoolEvent(p, ev)
		}
	}()

	code, werr := child.Wait()
	// Let the last output drain.
	select {
	case <-outDone:
	case <-time.After(500 * time.Millisecond):
	}
	close(stop)
	wg.Wait()
	_ = core.SpoolEvent(p, core.Event{
		Harness:  harness,
		Kind:     core.KindWrapperExit,
		At:       time.Now(),
		Source:   "wrapper",
		Location: &core.Location{Kind: "wrapper", Target: id},
		Cwd:      cwd,
		PID:      os.Getpid(),
	})
	return code, werr
}

func signature(m *rules.Match) string { return m.Kind + "|" + normalise(m.Text) }

// freshLines is screen without its stale lines: of each line in stale, the
// first (oldest) copies, as many as were shown then. The same message shown
// once more since is new (a limit hit again after a resume). Counts drop as
// old copies scroll away, so a later copy isn't mistaken for an old one.
func freshLines(screen string, stale map[string]int) string {
	if len(stale) == 0 {
		return screen
	}
	forget(screen, stale)
	lines := strings.Split(screen, "\n")
	skip := map[string]int{}
	out := lines[:0]
	for _, l := range lines {
		k := normalise(l)
		if skip[k] < stale[k] {
			skip[k]++
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// forget lowers each stale count to the copies screen still shows.
func forget(screen string, stale map[string]int) {
	shown := map[string]int{}
	for _, l := range strings.Split(screen, "\n") {
		if k := normalise(l); stale[k] > 0 {
			shown[k]++
		}
	}
	for k, n := range stale {
		if shown[k] < n {
			stale[k] = shown[k]
		}
	}
}

func normalise(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

type inboxRequest struct {
	deliver.Request
	path string
}

// readInbox returns pending requests; the caller removes each file once the
// request is handled.
func readInbox(dir string) []inboxRequest {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var out []inboxRequest
	for _, n := range names {
		path := filepath.Join(dir, n)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var req deliver.Request
		if json.Unmarshal(data, &req) != nil || req.Text == "" {
			os.Remove(path)
			continue
		}
		out = append(out, inboxRequest{Request: req, path: path})
	}
	return out
}
