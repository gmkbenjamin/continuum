// Package wrapper implements `continuum run -- <agent>`: it runs the agent in
// a pseudo-terminal Continuum owns, passes all input and output through
// unchanged, watches the output for limit messages and types the resume prompt
// when the daemon asks. Works in any terminal emulator, focused or not.
package wrapper

import (
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

// Timing knobs for draft handling during wrapper delivery (overridden in tests).
var (
	draftClearDelay   = 80 * time.Millisecond
	draftSubmitDelay  = 350 * time.Millisecond
	draftRestoreDelay = 300 * time.Millisecond
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
		// fragments, pushed out of its tail by later redraws. Always kept
		// current so draft handling can also inspect the active prompt box
		// and bracketed-paste mode without enabling screen limit detection
		// for agents that don't want it.
		screen = ansi.NewScreen(cols, rows)
		// screenDetect: screen-based limit detection is wanted for this agent.
		screenDetect = h.ScreenDetection
		// screenAlive: false if the screen emulator ever recovered from a fault
		// (the agent's session must never suffer for it).
		screenAlive = true
		// Unsubmitted text the user typed or pasted into the input box.
		draft draftBuffer
		// Limit messages on screen when the user last sent a prompt or
		// Continuum last resumed: they stay visible in the history and must
		// not count again.
		stale = map[string]int{} // line -> how many times it was shown
		// Limits found on the rendered screen, reported since the last
		// prompt or resume (the screen keeps showing them).
		reportedShown = map[string]bool{}
		lastOutput    time.Time
		changed       bool
		turnGen       uint64
		writeMu       sync.Mutex
	)
	// The screen model's operations, mu held.
	guard := func(f func()) {
		if !screenAlive {
			return
		}
		defer func() {
			if recover() != nil {
				screenAlive = false
			}
		}()
		f()
	}
	shownText := func() (text string) {
		if !screenDetect {
			return ""
		}
		guard(func() { text = screen.String() })
		return text
	}
	screenPromptDraft := func() (text string, found, bp bool) {
		guard(func() {
			text, found = screen.PromptDraft()
			bp = screen.BracketedPaste()
		})
		return text, found, bp
	}
	markStale := func() { // mu held
		reportedShown = map[string]bool{}
		stale = map[string]int{}
		if !screenDetect || !screenAlive {
			return
		}
		for _, l := range strings.Split(shownText(), "\n") {
			if h.Classify(l, 1) != nil {
				stale[normalise(l)]++
			}
		}
	}
	// stdin -> child
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := o.Stdin.Read(buf)
			if n > 0 {
				writeMu.Lock()
				mu.Lock()
				submitted := draft.Feed(buf[:n])
				if draft.userInput {
					// User input may answer or dismiss a blocking menu.
					sinceInput = ansi.NewRing(16 * 1024)
					if r.BlockingDialog(shownText()) != "" {
						draft.uncertain = true
					}
				}
				if submitted {
					// A submitted prompt starts a new turn; older limit text
					// on screen must not be re-detected from the ring.
					ring = ansi.NewRing(16 * 1024)
					markStale()
					turnGen++
				}
				mu.Unlock()
				_, _ = child.Write(buf[:n])
				writeMu.Unlock()
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
		reported := map[string]bool{} // signature -> reported in current turn
		var seenGen uint64
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
				claimed := req.path + ".inflight"
				if err := os.Rename(req.path, claimed); err != nil {
					continue // the daemon withdrew the request
				}
				// Keep stdin writes and the draft model in the same order.
				// Keystrokes arriving during delivery reach the restored input.
				note, attempted := func() (note string, attempted bool) {
					writeMu.Lock()
					defer writeMu.Unlock()
					mu.Lock()
					tail := ansi.Strip(ring.String())
					recent := ansi.Strip(sinceInput.String())
					shown := shownText()
					saved := draft.Text()
					uncertain := draft.Uncertain() || len(draft.pending) != 0
					incomplete := draft.inPaste || draft.reply != 0
					screenDraft, foundPrompt, bracketedPaste := screenPromptDraft()
					cursor := draft.cursor
					mu.Unlock()
					if incomplete {
						return "a paste or terminal response is still in progress", false
					}

					dialog := r.BlockingDialog(tail)
					if dialog == "" {
						dialog = r.BlockingDialogAnywhere(recent)
					}
					if dialog == "" {
						dialog = r.BlockingDialog(shown)
					}
					if dialog != "" {
						return fmt.Sprintf("a menu or prompt is open (%q); not typing into it", dialog), false
					}
					if uncertain {
						// A wrapped or multiline screen capture cannot establish
						// the original line breaks or the editor's cursor offset.
						if !foundPrompt || strings.ContainsRune(screenDraft, '\n') {
							return "could not read the existing input safely; clear or submit it, then resume", false
						}
						saved = screenDraft
					}

					mode := core.NormalizeDraftHandling(req.DraftMode)
					if saved != "" && (!foundPrompt || compactDraft(saved) != compactDraft(screenDraft)) {
						return "could not verify the existing input against the prompt; submit it or resume manually", false
					}
					var restore []byte
					if saved != "" && mode == core.DraftRestore {
						restore = formatDraftForWrite(saved, bracketedPaste)
						if restore == nil {
							return "the agent has not enabled bracketed paste for restoring this draft; submit it or resume manually", false
						}
						// Keep a private recovery copy until the original text is
						// back in the editor. The agent can exit or open a menu
						// after accepting the resume prompt.
						backup, err := os.CreateTemp(p.Home, "saved-draft-*.txt")
						if err != nil {
							return err.Error(), false
						}
						_, err = backup.WriteString(saved)
						closeErr := backup.Close()
						if err != nil || closeErr != nil {
							_ = os.Remove(backup.Name())
							return "could not save a recovery copy of the draft", false
						}
						defer func() {
							if note == "" {
								_ = os.Remove(backup.Name())
							} else {
								note += "; draft saved to " + backup.Name()
							}
						}()
						if _, err := child.Write(clearDraftInput(saved, cursor, uncertain)); err != nil {
							return err.Error(), true
						}
						time.Sleep(draftClearDelay)
						mu.Lock()
						text, found, _ := screenPromptDraft()
						mu.Unlock()
						if !found || text != "" {
							return "could not confirm that the existing input was cleared", true
						}
					}
					if saved != "" && mode == core.DraftSubmit {
						mu.Lock()
						sinceInput = ansi.NewRing(16 * 1024)
						mu.Unlock()
						if _, err := child.Write([]byte("\r")); err != nil {
							return err.Error(), false
						}
						time.Sleep(max(draftSubmitDelay, enterDelay))
						mu.Lock()
						dialog = r.BlockingDialogAnywhere(ansi.Strip(sinceInput.String()))
						text, found, _ := screenPromptDraft()
						draft.Reset()
						mu.Unlock()
						if dialog != "" {
							return fmt.Sprintf("the draft was submitted and a menu or prompt opened (%q)", dialog), true
						}
						if !found || text != "" {
							return "could not confirm that the existing input was submitted", true
						}
					}
					// Record the turn boundary before Enter so a fast agent's
					// next limit output is not cleared along with the old text.
					mu.Lock()
					markStale()
					ring = ansi.NewRing(16 * 1024)
					sinceInput = ansi.NewRing(16 * 1024)
					draft.Reset()
					turnGen++
					changed = false
					mu.Unlock()
					if _, err := child.Write([]byte(req.Text)); err != nil {
						return err.Error(), true
					}
					time.Sleep(enterDelay)
					if _, err := child.Write([]byte("\r")); err != nil {
						return err.Error(), true
					}
					if restore != nil {
						time.Sleep(draftRestoreDelay)
						mu.Lock()
						dialog = r.BlockingDialog(ansi.Strip(ring.String()))
						text, found, bp := screenPromptDraft()
						mu.Unlock()
						if dialog != "" {
							return fmt.Sprintf("the resume prompt was sent and a menu or prompt opened (%q)", dialog), true
						}
						if !found || text != "" || (strings.ContainsAny(saved, "\n\t") && !bp) {
							return "the resume prompt was sent but the editor is not ready for the draft", true
						}
						if _, err := child.Write(restore); err != nil {
							return err.Error(), true
						}
						time.Sleep(draftClearDelay)
						mu.Lock()
						text, found, _ = screenPromptDraft()
						draft.SetText(saved)
						if !found || compactDraft(saved) != compactDraft(text) {
							draft.uncertain = true
						}
						restored := !draft.uncertain
						mu.Unlock()
						if !restored {
							return "the resume prompt was sent but the draft restore could not be confirmed", true
						}
					}
					return "", true
				}()
				if note != "" {
					// The daemon checks the note before acknowledging delivery.
					suffix := ".rejected"
					if attempted {
						suffix = ".unconfirmed"
					}
					_ = os.WriteFile(filepath.Join(inbox, req.ID+suffix), []byte(note), 0o600)
				}
				os.Remove(claimed)
				if attempted {
					stalled = false
				}
			}
			if interactive {
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
			if turnGen != seenGen {
				reported = map[string]bool{}
				stalled = false
				seenGen = turnGen
			}
			settled := changed && time.Since(lastOutput) > 1500*time.Millisecond
			var text, shown string
			var shownSeen map[string]bool
			if settled {
				changed = false
				text = ansi.Strip(ring.String())
				if screenDetect && screenAlive {
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
				if m != nil && reported[signature(m)] {
					m, dup = nil, true // already reported in this turn; the screen knows whether it is new
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
					reported = map[string]bool{}
					if screenDetect {
						mu.Lock()
						if turnGen == seenGen {
							reportedShown = map[string]bool{}
						}
						mu.Unlock()
					}
					_ = core.SpoolEvent(p, core.Event{
						Harness: harness, Kind: core.KindActivity, At: time.Now(), Source: "wrapper",
						Location: &core.Location{Kind: "wrapper", Target: id}, Cwd: cwd,
					})
				}
				continue
			}
			// A screen redraw repeats the same message; never report the same
			// signature again until a new turn starts (prompt/resume) or the
			// session visibly moves past the stall.
			sig := signature(m)
			if seen[sig] {
				continue
			}
			seen[sig] = true
			if shownSeen != nil && !fromScreen {
				// The same message on the rendered screen is this report too.
				if ms := h.Classify(shown, min(r.Defaults.TailLines, 12)); ms != nil {
					shownSeen[signature(ms)] = true
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

// clearDraftInput moves to the end of the draft and erases it. For multiline
// editors, horizontal movement crosses line breaks without recalling history.
func clearDraftInput(saved string, cursor int, uncertain bool) []byte {
	if uncertain {
		return []byte("\x05\x15\x01\x0b")
	}
	size := len([]rune(saved))
	return []byte(strings.Repeat("\x1b[C", size-cursor) + strings.Repeat("\x7f", size))
}

// Terminal rendering may expand tabs or wrap the draft at the window width.
func compactDraft(text string) string { return strings.Join(strings.Fields(text), "") }

func signature(m *rules.Match) string {
	body := m.Text
	if m.Line != "" {
		if idx := strings.Index(m.Text, m.Line); idx >= 0 {
			body = m.Text[idx:]
		} else {
			body = m.Line
		}
	}
	return m.Kind + "|" + normalise(body)
}

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
		if json.Unmarshal(data, &req) != nil || req.Text == "" || req.ID+".json" != n {
			os.Remove(path)
			continue
		}
		out = append(out, inboxRequest{Request: req, path: path})
	}
	return out
}
