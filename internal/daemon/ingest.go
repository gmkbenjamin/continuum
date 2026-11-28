package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/mux"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/resettime"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/sources"
	"github.com/continuum-dev/continuum/internal/store"
)

func (d *Daemon) ingestSpool() {
	entries, err := os.ReadDir(d.Paths.Spool)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, n := range names {
		path := filepath.Join(d.Paths.Spool, n)
		data, err := os.ReadFile(path)
		os.Remove(path)
		if err != nil {
			continue
		}
		var ev core.Event
		if json.Unmarshal(data, &ev) != nil {
			d.debugf("spool: unreadable event file %s skipped", n)
			continue
		}
		d.Handle(ev)
	}
}

func (d *Daemon) scanSources() {
	now := d.Now()
	since := now.Add(-d.Config.CatchUp())
	baseline := d.baseline
	d.baseline = false
	// On the very first scan history is not acted on, with one exception:
	// a session that is open right now and sitting on a limit is adopted,
	// so every open session is covered from the moment Continuum starts.
	var stalled []core.Event

	if d.Config.HarnessEnabled("claude") {
		root := filepath.Join(d.Config.ClaudeDir(), "projects")
		for path, lines := range d.Offsets.Claude.Scan(root, since, 3) {
			st := d.Offsets.ClaudeState[path]
			if st == nil {
				st = &sources.ClaudeState{}
			}
			evs := sources.ParseClaudeState(d.Rules, path, lines, st, d.Local)
			if st.Keep() {
				d.Offsets.ClaudeState[path] = st
			} else {
				delete(d.Offsets.ClaudeState, path)
			}
			d.debugf("transcript %s: %d new lines, %d events%s", filepath.Base(path), len(lines), len(evs), map[bool]string{true: " (first scan: history not acted on)"}[baseline])
			if baseline {
				if ev := lastStall(evs, now.Add(-lookBack)); ev != nil {
					stalled = append(stalled, *ev)
				}
				continue
			}
			for _, ev := range evs {
				d.Handle(ev)
			}
		}
		for p := range d.Offsets.ClaudeState {
			if _, ok := d.Offsets.Claude.Offsets[p]; !ok {
				delete(d.Offsets.ClaudeState, p)
			}
		}
	}
	if d.Config.HarnessEnabled("codex") {
		root := filepath.Join(d.Config.CodexDir(), "sessions")
		for path, lines := range d.Offsets.Codex.Scan(root, since, 5) {
			st := d.Offsets.State[path]
			if st == nil {
				st = &sources.CodexState{}
				d.Offsets.State[path] = st
			}
			evs := sources.ParseCodex(d.Rules, path, lines, st, d.Local)
			d.debugf("rollout %s: %d new lines, %d events%s", filepath.Base(path), len(lines), len(evs), map[bool]string{true: " (first scan: history not acted on)"}[baseline])
			if baseline {
				if ev := lastStall(evs, now.Add(-lookBack)); ev != nil {
					stalled = append(stalled, *ev)
				}
				continue
			}
			for _, ev := range evs {
				d.Handle(ev)
			}
		}
		for p := range d.Offsets.State {
			if _, ok := d.Offsets.Codex.Offsets[p]; !ok {
				delete(d.Offsets.State, p)
			}
		}
	}
	if d.Config.HarnessEnabled("antigravity") {
		root := filepath.Join(d.Config.AntigravityDataDir(), "brain")
		for path, lines := range d.Offsets.Antigravity.Scan(root, since, 4) {
			if filepath.Base(path) != "transcript.jsonl" {
				continue
			}
			st := d.Offsets.AntigravityState[path]
			if st == nil {
				st = &sources.AntigravityState{}
			}
			evs := sources.ParseAntigravityState(d.Rules, path, lines, st, d.Local)
			if st.Keep() {
				d.Offsets.AntigravityState[path] = st
			} else {
				delete(d.Offsets.AntigravityState, path)
			}
			d.debugf("antigravity transcript %s: %d new lines, %d events%s", filepath.Base(path), len(lines), len(evs), map[bool]string{true: " (first scan: history not acted on)"}[baseline])
			if baseline {
				if ev := lastStall(evs, now.Add(-lookBack)); ev != nil {
					stalled = append(stalled, *ev)
				}
				continue
			}
			for _, ev := range evs {
				d.Handle(ev)
			}
		}
		for p := range d.Offsets.AntigravityState {
			if _, ok := d.Offsets.Antigravity.Offsets[p]; !ok {
				delete(d.Offsets.AntigravityState, p)
			}
		}
	}
	for _, ev := range stalled {
		d.adoptIfOpen(ev)
	}
	if !d.lookedBack {
		d.lookedBack = true
		d.adoptOlderStalls(since)
	}
	d.dirty = true
}

// lastStall returns the transcript's final event if it is a stall (nothing
// happened in the session after it) recent enough to catch up on.
func lastStall(evs []core.Event, since time.Time) *core.Event {
	if len(evs) == 0 {
		return nil
	}
	ev := evs[len(evs)-1]
	switch ev.Kind {
	case core.KindUsageLimit, core.KindTransient:
		if ev.At.After(since) {
			return &ev
		}
	}
	return nil
}

// lookBack is how far the first scan after a start looks for open sessions
// still stopped on a limit: past a weekly limit.
const lookBack = 8 * 24 * time.Hour

// adoptOlderStalls runs once per start. A transcript last written before the
// catch-up window (since) isn't scanned, so a session that hit a limit while
// Continuum was stopped for longer than that would never be resumed. Each
// such transcript that ends on a stall is adopted if its session is open.
func (d *Daemon) adoptOlderStalls(since time.Time) {
	from := d.Now().Add(-lookBack)
	older := func(path string) bool {
		info, err := os.Stat(path)
		return err == nil && info.ModTime().Before(since)
	}
	if d.Config.HarnessEnabled("claude") {
		root := filepath.Join(d.Config.ClaudeDir(), "projects")
		for path, lines := range sources.NewTailer().Scan(root, from, 3) {
			if !older(path) {
				continue
			}
			if ev := lastStall(sources.ParseClaude(d.Rules, path, lines, d.Local), from); ev != nil {
				d.adoptIfOpen(*ev)
			}
		}
	}
	if d.Config.HarnessEnabled("codex") {
		root := filepath.Join(d.Config.CodexDir(), "sessions")
		for path, lines := range sources.NewTailer().Scan(root, from, 5) {
			if !older(path) {
				continue
			}
			if ev := lastStall(sources.ParseCodex(d.Rules, path, lines, &sources.CodexState{}, d.Local), from); ev != nil {
				d.adoptIfOpen(*ev)
			}
		}
	}
	if d.Config.HarnessEnabled("antigravity") {
		root := filepath.Join(d.Config.AntigravityDataDir(), "brain")
		for path, lines := range sources.NewTailer().Scan(root, from, 4) {
			if filepath.Base(path) != "transcript.jsonl" || !older(path) {
				continue
			}
			if ev := lastStall(sources.ParseAntigravity(d.Rules, path, lines, d.Local), from); ev != nil {
				d.adoptIfOpen(*ev)
			}
		}
	}
}

// stillOnLimit: the session of an old stall is open right now and hasn't moved
// on to another conversation.
func (d *Daemon) stillOnLimit(ev core.Event, sess *store.Session) bool {
	if sess != nil && sess.Ended {
		return false
	}
	if ev.SessionID != "" && d.LiveFn != nil {
		probe := &core.Job{Harness: ev.Harness, SessionID: ev.SessionID, Cwd: ev.Cwd, PID: ev.PID, Entrypoint: ev.Entrypoint}
		if st := d.LiveFn(probe); st != nil && st.Checked {
			return st.Open && !st.MovedOn
		}
	}
	return ev.Location != nil && ev.Location.Kind == "wrapper" && deliver.WrapperAlive(d.Paths, ev.Location.Target)
}

// adoptIfOpen schedules a stall found on the first scan, but only if its
// session is open right now: Continuum never revives closed history.
func (d *Daemon) adoptIfOpen(ev core.Event) {
	if d.LiveFn == nil {
		return
	}
	if sess := d.Sessions[store.SessionKey(ev.Harness, ev.SessionID)]; sess != nil && !sess.Ended {
		if ev.Cwd == "" {
			ev.Cwd = sess.Cwd
		}
		if ev.PID == 0 {
			ev.PID = sess.PID
		}
		if ev.Location == nil && sess.Location != nil {
			ev.Location = sess.Location
		}
	}
	probe := &core.Job{Harness: ev.Harness, SessionID: ev.SessionID, Cwd: ev.Cwd, PID: ev.PID, Entrypoint: ev.Entrypoint}
	st := d.LiveFn(probe)
	open := st != nil && st.Open && !st.MovedOn
	if !open && ev.Location != nil && ev.Location.Kind == "wrapper" && deliver.WrapperAlive(d.Paths, ev.Location.Target) {
		open = true
	}
	if !open {
		return
	}
	d.logf("%s:%s is open and stalled since %s: adopting it", ev.Harness, ev.SessionID, ev.At.Local().Format("15:04"))
	ev.Source += "+adopted"
	d.Handle(ev)
}

type paneLimit struct {
	line    string
	count   int
	kind    string
	seenAt  time.Time
	resetAt time.Time
}

// scanPanes finds limit messages in tmux panes running agents that have no
// transcript source (any harness whose driver does not skip pane scanning).
func (d *Daemon) scanPanes() {
	if _, ok := d.Rules.Multiplexers["tmux"]; !ok {
		return
	}
	if _, err := proc.LookPath("tmux"); err != nil {
		return
	}
	panes, err := mux.List(d.Rules, "tmux", d.Config.ScanTmuxSocket)
	if err != nil {
		return
	}
	d.debugf("pane scan: %d tmux panes", len(panes))
	for _, p := range panes {
		h := d.paneHarness(p)
		if h == nil || DriverFor(h.Name).IgnoreScreenEvents(d.Config) {
			continue
		}
		d.debugf("pane %s runs %s", p.Location.String(), h.DisplayName)
		text, err := mux.Capture(d.Rules, p.Location)
		if err != nil {
			continue
		}
		key := p.Location.String()
		// Only the bottom of the pane counts: that is where a stalled agent's
		// last message sits, just above its input box.
		m := h.Classify(text, 12)
		if m == nil {
			delete(d.paneSeen, key)
			continue
		}
		// A resume can echo a prompt or redraw the footer while the earlier
		// limit remains visible. Count copies of the matched line so only a
		// new line, or a new copy of the same line, reports another stall.
		at := d.Now()
		limit := paneLimit{line: m.Line, kind: m.Kind, seenAt: at}
		for line := range strings.SplitSeq(text, "\n") {
			if strings.TrimSpace(line) == m.Line {
				limit.count++
			}
		}
		previous := d.paneSeen[key]
		sameLine := limit.line == previous.line && limit.count <= previous.count
		if sameLine {
			// Compare relative reset text against the same reference time.
			limit.seenAt = previous.seenAt
		}
		if h.NeverResume(m.Text) {
			limit.kind = core.KindNeverResume
		}
		if limit.kind == core.KindUsageLimit {
			limit.resetAt, _ = resettime.Parse(m.Text, limit.seenAt, d.Local)
		}
		if sameLine && limit.kind == previous.kind &&
			(limit.resetAt.IsZero() || limit.resetAt.Equal(previous.resetAt)) {
			previous.count = limit.count
			d.paneSeen[key] = previous
			continue
		}
		d.paneSeen[key] = limit
		d.Handle(core.Event{
			Harness: h.Name, Kind: limit.kind, Message: m.Text, At: at, ResetAt: limit.resetAt,
			Source: "pane-scan", Location: p.Location, Hit: m.Hit,
		})
	}
}

// paneHarness identifies the agent in a pane: by its process name, or, for an
// agent running under an interpreter (tmux then says "node" or "python"), by
// the command line of the pane's foreground process.
func (d *Daemon) paneHarness(p mux.Pane) *rules.Harness {
	names := d.Rules.HarnessNames()
	pick := func(match func(*rules.Harness) bool) *rules.Harness {
		for _, name := range names {
			if DriverFor(name).SkipPaneScan() || name == "generic" || !d.Config.HarnessEnabled(name) {
				continue
			}
			if h := d.Rules.Harness(name); match(h) {
				return h
			}
		}
		if g := d.Rules.Harness("generic"); g != nil && match(g) {
			return g
		}
		return nil
	}
	if h := pick(func(h *rules.Harness) bool { return h.IsHarnessProcess(p.Command) }); h != nil {
		return h
	}
	if !d.Rules.IsInterpreter(p.Command) || p.Location.Kind != "tmux" {
		return nil
	}
	argv := []string{"tmux"}
	if p.Location.Socket != "" {
		argv = append(argv, "-S", p.Location.Socket)
	}
	tty, err := mux.Runner(5*time.Second, "", append(argv, "display-message", "-p", "-t", p.Location.Target, "#{pane_tty}")...)
	if err != nil {
		return nil
	}
	line := proc.ForegroundCommand(strings.TrimSpace(tty))
	if line == "" {
		return nil
	}
	return pick(func(h *rules.Harness) bool { return h.MatchesCommandLine(line) })
}
