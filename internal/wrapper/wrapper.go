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
}

// DetectHarness guesses the harness from the command being run.
func DetectHarness(r *rules.Rules, argv []string) string {
	if len(argv) == 0 {
		return "generic"
	}
	for _, name := range r.HarnessNames() {
		if r.Harness(name).IsHarnessProcess(argv[0]) {
			return name
		}
	}
	return "generic"
}

// Run starts the agent and blocks until it exits, returning its exit code.
func Run(o Options) (int, error) {
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
	meta, _ := json.Marshal(map[string]any{"harness": harness, "argv": o.Argv, "cwd": cwd, "started": time.Now()})
	_ = os.WriteFile(filepath.Join(dir, "meta.json"), meta, 0o600)
	if o.Choose || os.Getenv("CONTINUUM_CHOOSE") != "" {
		if sc, err := scope.Load(p); err == nil {
			sc.Chosen, _ = scope.Add(sc.Chosen, scope.Entry{Harness: harness, Wrapper: id, Label: filepath.Base(o.Argv[0]) + " in " + cwd})
			_ = sc.Save(p)
		}
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
		lastOutput time.Time
		changed    bool
		writeMu    sync.Mutex
	)
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
				mu.Unlock()
				if dialog := r.BlockingDialog(screen); dialog != "" {
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
				// Only output produced after the resume can contain a new limit;
				// the old message may still be on screen and must not count again.
				mu.Lock()
				ring = ansi.NewRing(16 * 1024)
				changed = false
				mu.Unlock()
			}
			// Detection once the screen has settled.
			mu.Lock()
			settled := changed && time.Since(lastOutput) > 1500*time.Millisecond
			var text string
			if settled {
				changed = false
				text = ansi.Strip(ring.String())
			}
			mu.Unlock()
			if !settled {
				continue
			}
			m := h.Classify(text, r.Defaults.TailLines)
			if m == nil {
				continue
			}
			// A screen redraw repeats the same message; report it again only
			// after a while (a genuinely new limit names a new reset time).
			sig := m.Kind + "|" + normalise(m.Text)
			now := time.Now()
			if last, ok := reported[sig]; ok && now.Sub(last) < 15*time.Minute {
				continue
			}
			reported[sig] = now
			ev := core.Event{
				Harness: harness, Kind: m.Kind, Message: m.Text, At: now, Source: "wrapper",
				Location: &core.Location{Kind: "wrapper", Target: id}, Cwd: cwd,
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
	return code, werr
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
