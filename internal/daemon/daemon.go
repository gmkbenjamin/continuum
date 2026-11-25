// Package daemon is Continuum's scheduler: it collects events, keeps one job
// per stalled session, waits for the reset, checks the session is still
// stalled, and delivers the resume prompt.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/awake"
	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/logx"
	"github.com/continuum-dev/continuum/internal/messages"
	"github.com/continuum-dev/continuum/internal/mux"
	"github.com/continuum-dev/continuum/internal/notify"
	"github.com/continuum-dev/continuum/internal/opencode"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/resettime"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/scope"
	"github.com/continuum-dev/continuum/internal/sources"
	"github.com/continuum-dev/continuum/internal/store"
)

type offsetsFile struct {
	Claude *sources.Tailer                `json:"claude"`
	Codex  *sources.Tailer                `json:"codex"`
	State  map[string]*sources.CodexState `json:"codex_state"`
	// ClaudeState: transcripts whose last turn is wrapping up after a limit.
	ClaudeState map[string]*sources.ClaudeState `json:"claude_state,omitempty"`
}

// Daemon holds the scheduler state.
type Daemon struct {
	Paths    core.Paths
	Config   core.Config
	Scope    scope.Scope
	Rules    *rules.Rules
	Jobs     map[string]*core.Job
	Sessions map[string]*store.Session
	Offsets  offsetsFile
	Local    *time.Location

	Now       func() time.Time
	DeliverFn func(deliver.Options, *core.Job) (deliver.Result, error)
	NotifyFn  func(title, body string)
	Log       *logx.Logger
	// LiveFn reports whether a job's session is open right now (nil: unknown).
	LiveFn func(*core.Job) *live.State
	// Awake keeps the computer from idle-sleeping while work is pending.
	Awake *awake.Keeper
	// Compat, when set, enables the update watch (see compat.go).
	Compat *Compat

	baseline    bool
	lookedBack  bool
	timingSig   string
	compatDirty bool
	lastBeat    time.Time
	lastCompat  time.Time
	lastDesktop time.Time
	paneSeen    map[string]paneLimit
	replies     map[string]replySeen
	// answered: per account, the latest model answer on the plan (see
	// lifted.go), for limits reported after it is read.
	answered         map[string]core.Event
	lastScan         time.Time
	lastPaneScan     time.Time
	lastOpenCodeScan time.Time
	dirty            bool
}

// New loads state from disk.
func New(p core.Paths) (*Daemon, error) {
	if err := p.Ensure(); err != nil {
		return nil, err
	}
	d := &Daemon{
		Paths:     p,
		Now:       time.Now,
		DeliverFn: deliver.Deliver,
		NotifyFn:  notify.Send,
		Local:     time.Local,
		Log:       logx.New(logx.Sink{W: os.Stderr, Level: logx.LevelFromEnv(logx.Info)}),
		Sessions:  map[string]*store.Session{},
		paneSeen:  map[string]paneLimit{},
		Awake:     awake.New(),
	}
	d.LiveFn = d.liveState
	d.reload()
	var err error
	if d.Jobs, err = store.LoadJobs(p.Jobs); err != nil {
		d.Log.Warnf("jobs file unreadable, starting empty: %v", err)
		d.Jobs = map[string]*core.Job{}
	}
	d.reconcileLoadedJobs()
	if err := store.LoadJSON(p.Sessions, &d.Sessions); err != nil || d.Sessions == nil {
		d.Sessions = map[string]*store.Session{}
	}
	if _, statErr := os.Stat(p.Offsets); os.IsNotExist(statErr) {
		// First run: learn where every transcript ends without acting on
		// history, so installing Continuum never revives old sessions.
		d.baseline = true
	}
	_ = store.LoadJSON(p.Offsets, &d.Offsets)
	if d.Offsets.Claude == nil {
		d.Offsets.Claude = sources.NewTailer()
	}
	if d.Offsets.Codex == nil {
		d.Offsets.Codex = sources.NewTailer()
	}
	d.Offsets.Claude.InitialBytes, d.Offsets.Codex.InitialBytes = 512*1024, 512*1024
	if d.Offsets.Claude.Offsets == nil {
		d.Offsets.Claude.Offsets = map[string]int64{}
	}
	if d.Offsets.Codex.Offsets == nil {
		d.Offsets.Codex.Offsets = map[string]int64{}
	}
	if d.Offsets.State == nil {
		d.Offsets.State = map[string]*sources.CodexState{}
	}
	if d.Offsets.ClaudeState == nil {
		d.Offsets.ClaudeState = map[string]*sources.ClaudeState{}
	}
	return d, nil
}

// reconcileLoadedJobs repairs pending jobs loaded from jobs.json whose reset
// time could not be parsed by an older binary or that matched a user input
// prompt line.
func (d *Daemon) reconcileLoadedJobs() {
	for _, j := range d.Jobs {
		if j == nil || j.State != core.StatePending || j.Message == "" {
			continue
		}
		if len(j.Sources) == 1 && j.Sources[0] == "wrapper" && d.Rules != nil && d.Rules.Harness(j.Harness).Classify(j.Message, d.Rules.Defaults.TailLines) == nil {
			j.State = core.StateSkipped
			j.Result = "ignored user input prompt"
			j.UpdatedAt = d.Now()
			d.dirty = true
			d.logf("·  %s · skipped false-positive wrapper event from user input prompt", d.describe(j))
			continue
		}
		if j.Kind == core.KindUsageLimit && !j.ResetKnown {
			ref := j.LimitAt
			if ref.IsZero() {
				ref = d.Now()
			}
			if t, ok := resettime.Parse(j.Message, ref, d.Local); ok {
				j.ResetAt, j.ResetKnown = t, true
				d.reschedule(j)
				j.UpdatedAt = d.Now()
				d.dirty = true
				d.logf("⏸  %s · parsed reset %s from saved message · resumes %s", d.describe(j), t.In(d.Local).Format("15:04"), when(j.DueAt, d.Now()))
			}
		}
	}
}

func (d *Daemon) reload() {
	cfg, err := core.LoadConfig(d.Paths.Config)
	if err != nil {
		d.warnf("config %s ignored: %v", d.Paths.Config, err)
	}
	d.Config = cfg
	sc, err := scope.Load(d.Paths)
	if err != nil {
		d.logf("scope %s unreadable, resuming every session: %v", scope.Path(d.Paths), err)
	}
	d.Scope = sc
	r, err := rules.Cached(d.Paths.Rules)
	if err != nil {
		d.logf("%v", err)
	}
	if r != nil {
		d.Rules = r
	}
}

func (d *Daemon) logf(format string, args ...any) {
	d.Log.Infof(format, args...)
}

func (d *Daemon) history(action string, j *core.Job, extra map[string]any) {
	e := map[string]any{"action": action, "key": j.Key, "harness": j.Harness, "session": j.SessionID, "state": j.State}
	for k, v := range extra {
		e[k] = v
	}
	store.AppendHistory(d.Paths.History, e)
}

// Run loops until stop is closed.
func (d *Daemon) Run(stop <-chan struct{}) {
	d.logf("continuum %s daemon started (home %s)", core.Version, d.Paths.Home)
	for {
		d.Tick()
		wait := d.Config.Poll()
		if next := d.nextDue(); !next.IsZero() {
			if until := next.Sub(d.Now()); until < wait {
				wait = max(until, 200*time.Millisecond)
			}
		}
		select {
		case <-stop:
			d.save()
			if d.Awake != nil {
				d.Awake.Set(false, "")
			}
			d.logf("daemon stopped")
			return
		case <-time.After(wait):
		}
	}
}

func (d *Daemon) nextDue() time.Time {
	var next time.Time
	for _, j := range d.Jobs {
		if j.State == core.StatePending && (next.IsZero() || j.DueAt.Before(next)) {
			next = j.DueAt
		}
	}
	return next
}

// Tick runs one pass: ingest, scan, deliver, save.
func (d *Daemon) Tick() {
	d.reload()
	d.reconcileLoadedJobs()
	d.ingestSpool()
	now := d.Now()
	if d.Config.TranscriptsWatched() && now.Sub(d.lastScan) >= 2*d.Config.Poll() {
		d.lastScan = now
		d.scanSources()
	}
	if d.Config.OpenCodeServer != "" && d.Config.IsEnabled() && d.Config.HarnessEnabled("opencode") &&
		now.Sub(d.lastOpenCodeScan) >= 15*time.Second {
		d.lastOpenCodeScan = now
		if err := d.pollOpenCode(now); err != nil {
			d.debugf("OpenCode server scan: %v", err)
		}
	}
	if d.Config.PanesScanned() && now.Sub(d.lastPaneScan) >= 15*time.Second {
		d.lastPaneScan = now
		d.scanPanes()
	}
	d.applyScope()
	d.applyTiming()
	if d.Compat != nil {
		if now.Sub(d.lastDesktop) >= time.Minute {
			d.lastDesktop = now
			d.observeDesktop()
		}
		if now.Sub(d.lastCompat) >= 10*time.Minute {
			if d.lastCompat.IsZero() && d.Rules != nil {
				apps := make(map[string]*rules.TerminalApp, len(d.Rules.TerminalApps))
				for k, v := range d.Rules.TerminalApps {
					apps[k] = v
				}
				go d.probeTerminalApps(apps) // may wait for the user's answer
			}
			d.lastCompat = now
			d.checkCompat(false)
		}
	}
	if d.Log.Enabled(logx.Debug) && now.Sub(d.lastBeat) >= time.Minute {
		d.lastBeat = now
		pending, next := 0, time.Time{}
		for _, j := range d.Jobs {
			if j.State == core.StatePending {
				pending++
				if next.IsZero() || j.DueAt.Before(next) {
					next = j.DueAt
				}
			}
		}
		if pending > 0 {
			d.debugf("waiting: %d resume(s), next at %s", pending, next.Local().Format("15:04:05"))
		} else {
			d.debugf("waiting: nothing scheduled")
		}
	}
	d.runDue()
	d.prune()
	d.keepAwake()
	d.saveCompat()
	if d.dirty {
		d.save()
	}
}

func (d *Daemon) save() {
	if err := store.SaveJSON(d.Paths.Jobs, d.Jobs); err != nil {
		d.warnf("save jobs: %v", err)
	}
	if err := store.SaveJSON(d.Paths.Sessions, d.Sessions); err != nil {
		d.warnf("save sessions: %v", err)
	}
	if err := store.SaveJSON(d.Paths.Offsets, d.Offsets); err != nil {
		d.warnf("save offsets: %v", err)
	}
	d.dirty = false
}

// ---- event intake ----------------------------------------------------------

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
	probe := &core.Job{Harness: ev.Harness, SessionID: ev.SessionID, Cwd: ev.Cwd, PID: ev.PID, Entrypoint: ev.Entrypoint}
	st := d.LiveFn(probe)
	if st == nil || !st.Open || st.MovedOn {
		return
	}
	d.logf("%s:%s is open and stalled since %s: adopting it", ev.Harness, ev.SessionID, ev.At.Local().Format("15:04"))
	ev.Source += "+adopted"
	d.Handle(ev)
}

// Handle applies one event.
func (d *Daemon) Handle(ev core.Event) {
	if !d.Config.IsEnabled() {
		d.debugf("event ignored: Continuum is switched off (enabled: false)")
		return
	}
	if ev.Kind != "control" && d.Log.Enabled(logx.Debug) {
		d.debugf("event %s %s session=%s source=%s where=%s at=%s", ev.Harness, ev.Kind, short(ev.SessionID), ev.Source, ev.Location.String(), ev.At.Local().Format("15:04:05"))
	}
	if ev.Kind == "control" {
		d.control(ev.Message)
		return
	}
	if ev.Harness == "" {
		ev.Harness = "generic"
	}
	if !d.Config.HarnessEnabled(ev.Harness) {
		return
	}
	if ev.Harness == "opencode" && d.Config.OpenCodeServer != "" && screenSourced(ev.Source) {
		return // the configured server reports session outcomes by ID
	}
	if ev.At.IsZero() {
		ev.At = d.Now()
	}
	sess := d.touchSession(ev)

	switch ev.Kind {
	case core.KindActivity:
		if !ev.TurnStart {
			// A prompt submitted isn't the session moving on: it may be a
			// local command (/usage, /model) typed while it waits. The
			// turn's end, or its new limit, says what happened.
			d.continued(ev, nil)
		}
		if ev.Answered {
			ev.RequestAt = hookRequestAt(ev, sess)
		}
		if !ev.RequestAt.IsZero() {
			d.limitLifted(ev)
		}
	case core.KindLimitReset:
		d.limitLifted(ev)
	case core.KindSessionEnd:
		// The agent quit: its pane (or wrapper) now holds something else, so
		// a pending resume must not be typed there. It continues the closed
		// session through the agent's own command instead, if it has one.
		if j := d.jobFor(ev); j != nil && j.State == core.StatePending && (j.Location != nil || j.PID > 0) {
			d.debugf("%s: the session ended; forgetting its pane %s", j.Key, j.Location.String())
			j.Location, j.PID = nil, 0
			j.UpdatedAt = d.Now()
			d.dirty = true
		}
	case core.KindNativeGaveUp:
		if j := d.jobFor(ev); j != nil && j.State == core.StatePending {
			j.NativeGaveUp = true
			j.UpdatedAt = d.Now()
			d.reschedule(j)
			d.dirty = true
		}
	case core.KindReply:
		d.reply(ev, sess)
		// A turn that ended normally is activity too, for an earlier stall
		// its reply doesn't repeat.
		d.continued(ev, func(j *core.Job) bool { return !replyHas(ev.Message, j.Hit) && !replyHas(ev.Message, j.Message) })
	case core.KindUsageLimit, core.KindTransient, core.KindNeverResume:
		if d.partOfReply(ev, sess) {
			d.logf("%s · %s: the limit text on screen is part of the agent's reply, not an error; ignoring it", d.name(ev.Harness), ev.Location.String())
			return
		}
		d.limitEvent(ev, sess)
	}
}

// continued drops pending resumes of ev's session (or place) whose stall came
// before ev: the session did something since. only, when set, picks which.
func (d *Daemon) continued(ev core.Event, only func(*core.Job) bool) {
	for _, j := range d.Jobs {
		if j.State != core.StatePending || !ev.At.After(j.LimitAt.Add(time.Second)) {
			continue
		}
		if ev.SessionID != "" && j.SessionID != "" {
			// A pane can be reused, and Codex's shared backend used to
			// report its launcher's wrapper for unrelated conversations.
			if ev.SessionID != j.SessionID || ev.Harness != j.Harness {
				continue
			}
		} else if ev.Location == nil || j.Location == nil || ev.Location.String() != j.Location.String() {
			continue
		}
		if only != nil && !only(j) {
			continue
		}
		if screenSourced(ev.Source) && !onlyScreenSources(j.Sources) {
			// The screen changing isn't the session moving on: Claude Code
			// clears its limit notice by itself when the limit resets. The
			// transcript or hooks that reported this stall report activity.
			d.debugf("%s: the screen changed, but the stall came from %s; waiting for the agent itself to show activity", j.Key, strings.Join(j.Sources, ", "))
			continue
		}
		j.State = core.StateSkipped
		j.Result = "the session continued on its own or you continued it"
		j.UpdatedAt = d.Now()
		d.dirty = true
		d.history("skipped", j, map[string]any{"reason": j.Result})
		d.logf("%s: %s", j.Key, j.Result)
	}
}

// A reply that mentions limits, kept for a few minutes per place.
type replySeen struct {
	text string
	at   time.Time
}

const replyWindow = 3 * time.Minute

func screenSourced(source string) bool { return source == "wrapper" || source == "pane-scan" }

func replyHas(reply, hit string) bool {
	hit = strings.ToLower(strings.Join(strings.Fields(hit), " "))
	return hit != "" && strings.Contains(strings.ToLower(strings.Join(strings.Fields(reply), " ")), hit)
}

// reply records a normal turn end whose reply mentions limits, and drops a
// resume that was scheduled from that same text on screen.
func (d *Daemon) reply(ev core.Event, sess *store.Session) {
	loc := ev.Location
	if loc == nil && sess != nil {
		loc = sess.Location
	}
	if loc == nil || ev.Message == "" {
		return
	}
	if d.replies == nil {
		d.replies = map[string]replySeen{}
	}
	d.replies[loc.String()] = replySeen{text: ev.Message, at: ev.At}
	for _, j := range d.Jobs {
		if j.State != core.StatePending || j.Location == nil || j.Location.String() != loc.String() {
			continue
		}
		if !onlyScreenSources(j.Sources) || j.LimitAt.Sub(ev.At).Abs() > replyWindow || !replyHas(ev.Message, j.Hit) {
			continue
		}
		j.State = core.StateSkipped
		j.Result = "the limit text on screen was part of the agent's reply, not an error"
		j.UpdatedAt = d.Now()
		d.dirty = true
		d.history("skipped", j, map[string]any{"reason": j.Result})
		d.logf("%s: %s", j.Key, j.Result)
	}
}

func onlyScreenSources(list []string) bool {
	for _, s := range list {
		if !screenSourced(s) {
			return false
		}
	}
	return len(list) > 0
}

// partOfReply: limit text read off the screen that the agent's own reply,
// just finished, contains.
func (d *Daemon) partOfReply(ev core.Event, sess *store.Session) bool {
	if !screenSourced(ev.Source) || ev.Location == nil || ev.Hit == "" {
		return false
	}
	r, ok := d.replies[ev.Location.String()]
	return ok && ev.At.Sub(r.at).Abs() <= replyWindow && replyHas(r.text, ev.Hit)
}

func (d *Daemon) touchSession(ev core.Event) *store.Session {
	if ev.SessionID == "" {
		return nil
	}
	key := store.SessionKey(ev.Harness, ev.SessionID)
	s := d.Sessions[key]
	if s == nil {
		s = &store.Session{Harness: ev.Harness, SessionID: ev.SessionID}
		d.Sessions[key] = s
	}
	if ev.Cwd != "" {
		s.Cwd = ev.Cwd
	}
	if ev.TranscriptPath != "" {
		s.TranscriptPath = ev.TranscriptPath
	}
	if ev.Location != nil && ev.Location.Kind != "" {
		s.Location = ev.Location
	}
	if ev.PID > 0 {
		s.PID = ev.PID
	}
	if ev.Entrypoint != "" {
		s.Entrypoint = ev.Entrypoint
	}
	if ev.Model != "" {
		s.Model = ev.Model
	}
	if ev.TurnStart && ev.At.After(s.TurnAt) {
		s.TurnAt = ev.At
	}
	switch ev.Kind {
	case core.KindSessionStart:
		s.Ended = false
	case core.KindSessionEnd:
		s.Ended = true
	}
	if ev.At.After(s.LastSeen) {
		s.LastSeen = ev.At
	}
	d.dirty = true
	return s
}

func jobKey(ev core.Event) string {
	if ev.SessionID != "" {
		return ev.Harness + ":" + ev.SessionID
	}
	if ev.Location != nil {
		return ev.Harness + "@" + ev.Location.String()
	}
	return ""
}

func (d *Daemon) jobFor(ev core.Event) *core.Job {
	if k := jobKey(ev); k != "" {
		return d.Jobs[k]
	}
	return nil
}

func addSource(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

func (d *Daemon) limitEvent(ev core.Event, sess *store.Session) {
	now := d.Now()
	key := jobKey(ev)
	if ev.At.Before(now.Add(-d.Config.CatchUp())) {
		// Older than the catch-up window: Continuum wasn't running when it
		// happened (stopped, or the computer was off). It still counts if the
		// session is open and has sat on that limit since; later activity
		// (from the spool or the transcript) drops it again.
		if !d.stillOnLimit(ev, sess) {
			d.debugf("%s: ignoring a %s from %s at %s: older than the catch-up window, and the session is closed or has moved on", key, ev.Kind, ev.Source, ev.At.Local().Format("Jan 2 15:04"))
			return
		}
		if d.Jobs[key] == nil && !strings.HasSuffix(ev.Source, "+adopted") {
			d.logf("%s · %s: hit a limit at %s while Continuum wasn't running, and is still waiting on it: scheduling its resume", d.name(ev.Harness), key, ev.At.Local().Format("Jan 2 15:04"))
		}
	}
	if key == "" {
		d.logf("ignoring %s %s event with no session or location", ev.Harness, ev.Kind)
		return
	}
	if d.liftedNotice(ev) {
		d.debugf("%s: the limit notice on screen is the one already lifted early; ignoring it", key)
		return
	}
	if h := d.Rules.Harness(ev.Harness); h.IgnoredEntrypoint(ev.Entrypoint) || sess != nil && h.IgnoredEntrypoint(sess.Entrypoint) {
		d.debugf("%s: a print-mode or SDK run; not resuming it", key)
		return
	}
	if ev.Source == "wrapper" && ev.Message != "" && d.Rules != nil && d.Rules.Harness(ev.Harness).Classify(ev.Message, d.Rules.Defaults.TailLines) == nil {
		d.debugf("%s: ignoring wrapper event from user input prompt: %q", key, firstLine(ev.Message))
		return
	}
	if ev.Kind == core.KindUsageLimit && ev.ResetAt.IsZero() && ev.Message != "" {
		if t, ok := resettime.Parse(ev.Message, ev.At, d.Local); ok {
			ev.ResetAt = t
		}
	}
	// Where the session runs, as its hooks recorded it: a transcript or
	// rollout line doesn't say, and without it a wrapper's job could be
	// matched to another session in the same folder.
	if ev.Location == nil && sess != nil && sess.Location != nil && !sess.Ended {
		ev.Location = sess.Location
	}
	j := d.Jobs[key]
	if twin := d.findTwin(ev, key); twin != nil {
		if ev.SessionID != "" && twin.SessionID == "" {
			// Re-key the pane/wrapper job under the session it belongs to.
			delete(d.Jobs, twin.Key)
			twin.Key, twin.SessionID, twin.Harness = key, ev.SessionID, ev.Harness
			if old := d.Jobs[key]; old != nil {
				if old.State == core.StatePending || recentlyDelivered(old, ev.At) {
					twin.Attempts = old.Attempts
				}
				if recentlyDelivered(old, ev.At) {
					if staleReset(old, twin.ResetAt) {
						twin.ResetAt, twin.ResetKnown = time.Time{}, false
					}
					if staleReset(old, ev.ResetAt) {
						ev.ResetAt = time.Time{}
					}
				}
			}
			d.Jobs[key] = twin
		}
		d.merge(twin, ev)
		return
	}
	if j != nil {
		switch {
		case j.State == core.StatePending && ev.Source != opencode.LocationKind &&
			ev.Kind != core.KindNeverResume && !ev.At.After(j.LimitAt.Add(2*time.Minute)):
			// The same limit seen by another observer (hook + transcript).
			d.merge(j, ev)
			return
		case (j.State == core.StateDelivered) && !ev.At.After(j.DeliveredAt):
			d.debugf("%s: ignoring %s from %s at %s (already delivered at %s)", key, ev.Kind, ev.Source, ev.At.Format(time.RFC3339), j.DeliveredAt.Format(time.RFC3339))
			return // an old line about a limit already handled
		case j.State == core.StateCancelled && !ev.At.After(j.UpdatedAt):
			d.debugf("%s: ignoring %s from %s (cancelled at %s)", key, ev.Kind, ev.Source, j.UpdatedAt.Format(time.RFC3339))
			return
		case j.State == core.StateNeedsUser && !ev.At.After(j.LimitAt.Add(2*time.Minute)):
			d.debugf("%s: ignoring %s from %s (already marked needs_user)", key, ev.Kind, ev.Source)
			return
		case j.State == core.StateIgnored && !ev.At.After(j.LimitAt.Add(2*time.Minute)):
			d.merge(j, ev)
			return
		case !ev.At.After(j.LimitAt):
			d.debugf("%s: ignoring older %s from %s (%s <= %s)", key, ev.Kind, ev.Source, ev.At.Format(time.RFC3339), j.LimitAt.Format(time.RFC3339))
			return
		}
	}
	attempts := 0
	stale := false
	if j != nil && j.State == core.StatePending {
		attempts = j.Attempts
		if ev.ResetAt.IsZero() && j.ResetKnown && j.ResetAt.After(ev.At) {
			ev.ResetAt = j.ResetAt
		}
	} else if recentlyDelivered(j, ev.At) {
		attempts = j.Attempts // the resume hit a limit again: keep counting
		// The same reset time as the limit already resumed means either an old
		// message seen again (a redraw) or a reset that has not happened yet.
		// Either way, do not retry straight away.
		stale = staleReset(j, ev.ResetAt)
		if stale {
			d.debugf("%s: reset time %s from %s is not newer than prior delivered reset %s; using retry backoff", key, ev.ResetAt.Format(time.RFC3339), ev.Source, j.ResetAt.Format(time.RFC3339))
		}
	}
	nj := &core.Job{
		Key: key, Harness: ev.Harness, SessionID: ev.SessionID, Cwd: ev.Cwd,
		TranscriptPath: ev.TranscriptPath, TranscriptSize: ev.TranscriptSize,
		Location: ev.Location, PID: ev.PID, Entrypoint: ev.Entrypoint,
		Kind: ev.Kind, Message: ev.Message, ResetAt: ev.ResetAt, ResetKnown: !ev.ResetAt.IsZero(),
		LimitAt: ev.At, CreatedAt: now, UpdatedAt: now, Attempts: attempts,
		State: core.StatePending, Sources: []string{ev.Source}, Hit: ev.Hit, LimitID: ev.LimitID,
	}
	if nj.Model == "" {
		if s := d.sessionAt(ev.Harness, ev.Location, sess); s != nil {
			nj.Model = s.Model
		}
	}
	if j != nil && j.State == core.StateDelivered && !j.LiftedAt.IsZero() && ev.Kind == core.KindUsageLimit && ev.At.Sub(j.DeliveredAt) < liftCheck && sameLimitOf(j, nj) {
		// Resumed early because the limit looked lifted, and hit the same
		// limit again straight away: it wasn't. Wait for the reset this
		// time: the one it was lifted from, if this report names none.
		nj.LiftFailed = true
		if nj.ResetAt.IsZero() && j.LiftedFrom.After(ev.At) {
			nj.ResetAt, nj.ResetKnown = j.LiftedFrom, true
			stale = false
		}
		d.logf("%s: the limit wasn't lifted after all; waiting for its reset", key)
	} else if j != nil && j.LiftFailed && j.ResetKnown && j.ResetAt.After(ev.At) && ev.Kind == core.KindUsageLimit && sameLimitOf(j, nj) {
		// The same limit again before the reset a failed lift is waiting
		// for (the session was continued by hand meanwhile): answers still
		// don't lift it, and it waits for that reset.
		nj.LiftFailed = true
		if nj.ResetAt.IsZero() {
			nj.ResetAt, nj.ResetKnown = j.ResetAt, true
		}
	}
	if sess != nil {
		if nj.Location == nil {
			nj.Location = sess.Location
		}
		if nj.PID == 0 {
			nj.PID = sess.PID
		}
		if nj.Cwd == "" {
			nj.Cwd = sess.Cwd
		}
		if nj.Entrypoint == "" {
			nj.Entrypoint = sess.Entrypoint
		}
	}
	d.Jobs[key] = nj
	d.dirty = true

	if ev.Kind == core.KindNeverResume {
		nj.State = core.StateNeedsUser
		nj.Result = "this limit needs you (billing, credits, spend cap or sign-in); Continuum will not resume it"
		d.history("needs_user", nj, map[string]any{"message": ev.Message})
		d.notify(nj, "Needs you", firstLine(ev.Message))
		return
	}
	if d.stopAtMaxAttempts(nj) {
		return
	}
	if stale {
		nj.ResetAt, nj.ResetKnown = time.Time{}, false
	}
	if !nj.ResetKnown && ev.Kind == core.KindUsageLimit {
		if t, ok := d.resetHint(ev.Harness); ok && t.After(now) {
			nj.ResetAt, nj.ResetKnown = t, true
		}
	}
	if !nj.ResetKnown && !stale && ev.Kind == core.KindUsageLimit {
		if o := d.sameLimit(nj); o != nil {
			nj.ResetAt, nj.ResetKnown = o.ResetAt, true
			d.debugf("%s: no reset time of its own; using %s from %s, which hit the same limit", key, o.ResetAt.Local().Format("15:04"), o.Key)
		}
	}
	if nj.ResetKnown && ev.Kind == core.KindUsageLimit {
		d.shareReset(nj)
	}
	d.reschedule(nj)
	d.liftIfAnswered(nj)
	if nj.State == core.StatePending {
		if ok, why := d.Scope.Covers(nj); !ok {
			nj.State, nj.Result = core.StateIgnored, why
			d.history("ignored", nj, map[string]any{"reason": why})
			d.logf("·  %s · %s, not resuming: %s", d.describe(nj), strings.ReplaceAll(nj.Kind, "_", " "), why)
			return
		}
		histExtra := map[string]any{
			"due":         nj.DueAt.Format(time.RFC3339),
			"kind":        nj.Kind,
			"source":      ev.Source,
			"reset_known": nj.ResetKnown,
			"attempt":     nj.Attempts + 1,
		}
		if nj.Hit != "" {
			histExtra["hit"] = nj.Hit
		}
		if nj.ResetKnown {
			histExtra["reset_at"] = nj.ResetAt.Format(time.RFC3339)
		}
		d.history("scheduled", nj, histExtra)
		if nj.ResetKnown {
			d.logf("⏸  %s · %s · reset %s · resumes %s (from %s)", d.describe(nj), strings.ReplaceAll(nj.Kind, "_", " "), nj.ResetAt.In(d.Local).Format("15:04"), when(nj.DueAt, d.Now()), ev.Source)
		} else {
			d.logf("⏸  %s · %s · reset time unknown (backoff #%d) · resumes %s (from %s, hit %q)", d.describe(nj), strings.ReplaceAll(nj.Kind, "_", " "), nj.Attempts+1, when(nj.DueAt, d.Now()), ev.Source, nj.Hit)
		}
		d.debugf("%s: scheduled from %s (hit=%q reset_known=%v message=%q)", nj.Key, ev.Source, nj.Hit, nj.ResetKnown, firstLine(nj.Message))
		if nj.Kind == core.KindUsageLimit {
			d.notify(nj, "Paused until reset", "Will continue at "+nj.DueAt.In(d.Local).Format("Mon 3:04 PM"))
		}
	}
}

// findTwin finds a pending job for the same stall reported under another key:
// a hook or transcript knows the session id, while the wrapper or a pane scan
// knows only where the agent runs. Matching on location (or directory) and
// time keeps one stall to one resume.
func (d *Daemon) findTwin(ev core.Event, key string) *core.Job {
	for k, j := range d.Jobs {
		if k == key || j.State != core.StatePending {
			continue
		}
		sameHarness := j.Harness == ev.Harness
		// The wrapper and pane scanner may only guess "generic"; the hook knows better.
		if !sameHarness && j.Harness != "generic" && ev.Harness != "generic" {
			continue
		}
		if ev.SessionID != "" && j.SessionID != "" {
			continue // two different sessions
		}
		dt := ev.At.Sub(j.LimitAt)
		if dt < -3*time.Minute || dt > 3*time.Minute {
			continue
		}
		bothLoc := ev.Location != nil && j.Location != nil
		sameLoc := bothLoc && ev.Location.String() == j.Location.String()
		if bothLoc && !sameLoc {
			continue // two different panes
		}
		sameDir := ev.Cwd != "" && j.Cwd != "" && filepath.Clean(ev.Cwd) == filepath.Clean(j.Cwd)
		if sameLoc || (sameDir && sameHarness) {
			return j
		}
	}
	return nil
}

func (d *Daemon) merge(j *core.Job, ev core.Event) {
	j.Sources = addSource(j.Sources, ev.Source)
	if j.Harness == "generic" && ev.Harness != "generic" {
		j.Harness = ev.Harness
	}
	if ev.Location != nil && ev.Location.Kind != "" && (j.Location == nil || ev.Location.Kind == "wrapper") {
		j.Location = ev.Location
	}
	if ev.PID > 0 {
		j.PID = ev.PID
	}
	if j.Cwd == "" {
		j.Cwd = ev.Cwd
	}
	if j.TranscriptPath == "" {
		j.TranscriptPath = ev.TranscriptPath
	}
	if ev.TranscriptSize > j.TranscriptSize && ev.TranscriptPath == j.TranscriptPath {
		j.TranscriptSize = ev.TranscriptSize
	}
	if ev.Entrypoint != "" {
		j.Entrypoint = ev.Entrypoint
	}
	if ev.Kind == core.KindUsageLimit && j.Kind != core.KindUsageLimit {
		j.Kind = core.KindUsageLimit
	}
	// A report of the stall read after its limit was lifted early names the
	// reset it was lifted from: it doesn't put the resume back.
	if !ev.ResetAt.IsZero() && (!j.ResetKnown || ev.ResetAt.After(j.ResetAt)) && (j.LiftedAt.IsZero() || ev.At.After(j.LiftedAt)) {
		j.ResetAt, j.ResetKnown = ev.ResetAt, true
	}
	if j.Message == "" {
		j.Message = ev.Message
	}
	if j.LimitID == "" {
		j.LimitID = ev.LimitID
	}
	j.UpdatedAt = d.Now()
	d.dirty = true
	if j.State == core.StatePending && d.stopAtMaxAttempts(j) {
		return
	}
	d.reschedule(j)
	d.debugf("%s: merged %s from %s (reset_known=%v due=%s)", j.Key, ev.Kind, ev.Source, j.ResetKnown, j.DueAt.Format(time.RFC3339))
}

func recentlyDelivered(j *core.Job, at time.Time) bool {
	return j != nil && j.State == core.StateDelivered && at.Sub(j.DeliveredAt) < 6*time.Hour
}

func staleReset(prev *core.Job, resetAt time.Time) bool {
	return !resetAt.IsZero() && prev != nil && prev.ResetKnown && !resetAt.After(prev.ResetAt)
}

func (d *Daemon) stopAtMaxAttempts(j *core.Job) bool {
	if j.Attempts < d.Rules.Defaults.MaxAttempts {
		return false
	}
	j.State = core.StateNeedsUser
	j.Result = fmt.Sprintf("stopped after %d attempts", j.Attempts)
	d.history("needs_user", j, nil)
	d.notify(j, "Stopped retrying", j.Result)
	d.dirty = true
	return true
}

func backoff(list []int, attempt int) time.Duration {
	if len(list) == 0 {
		return 15 * time.Minute
	}
	if attempt >= len(list) {
		attempt = len(list) - 1
	}
	return time.Duration(list[attempt]) * time.Second
}

func (d *Daemon) delay() time.Duration {
	if d.Config.DelayAfterReset != nil {
		return time.Duration(*d.Config.DelayAfterReset) * time.Second
	}
	return time.Duration(d.Rules.Defaults.DelayAfterResetSeconds) * time.Second
}

func (d *Daemon) grace(j *core.Job) time.Duration {
	if j.Kind != core.KindUsageLimit || j.NativeGaveUp {
		return 0
	}
	h := d.Rules.Harness(j.Harness)
	g := h.Grace(d.Rules.Defaults)
	if v, ok := d.Config.NativeGrace[j.Harness]; ok {
		g = v
	}
	if g <= 0 {
		return 0
	}
	// The agent can only resume by itself while its process is alive.
	if j.PID > 0 && !proc.Alive(j.PID) {
		return 0
	}
	return time.Duration(g) * time.Second
}

// reschedule computes DueAt from the reset time, attempt count and grace.
func (d *Daemon) reschedule(j *core.Job) {
	now := d.Now()
	switch {
	case j.ResetKnown:
		j.DueAt = j.ResetAt.Add(max(d.delay(), d.grace(j)))
	case j.Kind == core.KindTransient:
		j.DueAt = j.LimitAt.Add(backoff(d.Rules.Defaults.TransientBackoffSeconds, j.Attempts))
	default:
		j.DueAt = j.LimitAt.Add(backoff(d.Rules.Defaults.UnknownResetBackoffSeconds, j.Attempts))
	}
	if j.DueAt.Sub(now) > d.Config.MaxWait() {
		j.State = core.StateNeedsUser
		j.Result = fmt.Sprintf("reset is %s away, beyond max_wait_hours", j.DueAt.Sub(now).Round(time.Hour))
	}
}

// sameAccount: two jobs of one agent whose transcripts live under the same
// config folder, so their usage limits are the same account's. A job without
// a transcript (read off the screen) can't tell, and counts as the same.
func sameAccount(a, b *core.Job) bool {
	if a.Harness != b.Harness {
		return false
	}
	// An OpenCode server can host several providers and credentials. Its
	// final outcome does not identify the account that reported the limit.
	if (a.Location != nil && a.Location.Kind == opencode.LocationKind) ||
		(b.Location != nil && b.Location.Kind == opencode.LocationKind) {
		return false
	}
	if a.TranscriptPath == "" || b.TranscriptPath == "" {
		return true
	}
	return accountRoot(a.TranscriptPath) == accountRoot(b.TranscriptPath)
}

// limitWindow: how long before a known reset another stall of the same
// account is taken to be the same limit (Claude's session limit lasts 5 hours).
const limitWindow = 5 * time.Hour

// sameLimit finds the latest stall of j's account whose reset is known and
// still ahead of j's: a limit is the account's, so j waits for that reset too.
// Claude Code's grace stop, for one, ends a turn on a limit without saying
// when it resets.
func (d *Daemon) sameLimit(j *core.Job) *core.Job {
	var best *core.Job
	for _, o := range d.Jobs {
		if o == j || o.Kind != core.KindUsageLimit || !o.ResetKnown || !sameAccount(o, j) {
			continue
		}
		if !o.ResetAt.After(j.LimitAt) || o.LimitAt.After(j.LimitAt.Add(2*time.Minute)) || o.LimitAt.Before(j.LimitAt.Add(-limitWindow)) {
			continue
		}
		if best == nil || o.LimitAt.After(best.LimitAt) {
			best = o
		}
	}
	return best
}

// shareReset gives j's reset time to the same account's pending stalls that
// hit the limit shortly before j without learning when it resets.
func (d *Daemon) shareReset(j *core.Job) {
	for _, o := range d.Jobs {
		if o == j || o.State != core.StatePending || o.Kind != core.KindUsageLimit || o.ResetKnown || !sameAccount(o, j) {
			continue
		}
		if o.LimitAt.After(j.LimitAt.Add(2*time.Minute)) || o.LimitAt.Before(j.LimitAt.Add(-limitWindow)) || !j.ResetAt.After(o.LimitAt) {
			continue
		}
		o.ResetAt, o.ResetKnown = j.ResetAt, true
		o.UpdatedAt = d.Now()
		d.reschedule(o)
		d.dirty = true
		d.logf("⏸  %s · reset %s, learned from %s · resumes %s", d.describe(o), o.ResetAt.In(d.Local).Format("15:04"), d.describe(j), when(o.DueAt, d.Now()))
	}
}

// resetHint asks an external command (for example a quota tracker) when the
// harness's limit resets. Expected output: {"claude": "<RFC3339 or epoch>", ...}.
func (d *Daemon) resetHint(harness string) (time.Time, bool) {
	if len(d.Config.ResetHintsCommand) == 0 {
		return time.Time{}, false
	}
	out, err := proc.Run(15*time.Second, "", d.Config.ResetHintsCommand...)
	if err != nil {
		d.logf("reset hints command: %v", err)
		return time.Time{}, false
	}
	var m map[string]any
	if json.Unmarshal([]byte(out), &m) != nil {
		return time.Time{}, false
	}
	switch v := m[harness].(type) {
	case string:
		if t, err := time.Parse(time.RFC3339, v); err == nil && t.After(d.Now()) {
			return t, true
		}
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Unix(n, 0), true
		}
	case float64:
		return time.Unix(int64(v), 0), true
	}
	return time.Time{}, false
}

// ---- delivery ---------------------------------------------------------------

func (d *Daemon) runDue() {
	now := d.Now()
	for _, j := range store.SortedJobs(d.Jobs) {
		if j.State != core.StatePending || j.DueAt.After(now) {
			continue
		}
		d.runJob(j)
	}
}

func (d *Daemon) sessionActiveSince(j *core.Job) bool {
	if j.TranscriptPath == "" || j.TranscriptSize == 0 {
		return false
	}
	lines := sources.ReadFrom(j.TranscriptPath, j.TranscriptSize)
	if len(lines) == 0 {
		return false
	}
	var evs []core.Event
	switch j.Harness {
	case "claude":
		evs = sources.ParseClaude(d.Rules, j.TranscriptPath, lines, d.Local)
	case "codex":
		st := &sources.CodexState{SessionID: j.SessionID}
		evs = sources.ParseCodex(d.Rules, j.TranscriptPath, lines, st, d.Local)
	}
	for _, ev := range evs {
		if ev.Kind == core.KindActivity && ev.At.After(j.LimitAt.Add(time.Second)) {
			return true
		}
	}
	return false
}

func (d *Daemon) runJob(j *core.Job) {
	d.dirty = true
	if j.Harness == "opencode" && d.Config.OpenCodeServer != "" &&
		(j.Location == nil || j.Location.Kind != opencode.LocationKind) {
		d.skip(j, "OpenCode now uses the configured server for resume")
		return
	}
	d.debugf("%s: due (attempt %d, reset %s, limit at %s); checking", j.Key, j.Attempts+1, j.ResetAt.Local().Format("15:04"), j.LimitAt.Local().Format("15:04"))
	if d.sessionActiveSince(j) {
		j.State = core.StateSkipped
		j.Result = "the session continued on its own or you continued it"
		j.UpdatedAt = d.Now()
		d.history("skipped", j, map[string]any{"reason": j.Result})
		d.logf("%s: %s", j.Key, j.Result)
		return
	}
	// Refresh what we know about where the session lives (not from a session
	// that has ended: its pane belongs to whatever runs there now).
	sess := d.Sessions[store.SessionKey(j.Harness, j.SessionID)]
	if sess != nil && !sess.Ended {
		if sess.Location != nil && (j.Location == nil || sess.LastSeen.After(j.LimitAt)) {
			j.Location = sess.Location
		}
		if sess.PID > 0 {
			j.PID = sess.PID
		}
	}
	if j.Harness == "codex" {
		if sess != nil && live.CodexAppServer(proc.Args(sess.PID)) && j.Location.String() == sess.Location.String() {
			j.Location = nil // the shared server's inherited pane
		}
		if j.PID > 0 && live.CodexAppServer(proc.Args(j.PID)) {
			j.Location, j.PID = nil, 0
		}
	}
	var st *live.State
	if d.LiveFn != nil {
		st = d.LiveFn(j)
	}
	if st != nil && d.Log.Enabled(logx.Debug) {
		pid, status, waiting := 0, "", ""
		if st.Session != nil {
			pid, status, waiting = st.Session.PID, st.Session.Status, st.Session.WaitingFor
		}
		d.debugf("%s: open=%v (source %s) pid=%d status=%q waiting=%q busy=%v background=%v desktop=%q moved_on=%v", j.Key, st.Open, st.Source, pid, status, waiting, st.Busy, st.Background, st.DesktopApp, st.MovedOn)
	} else if st == nil {
		d.debugf("%s: whether the session is open can't be checked for this agent", j.Key)
	}
	ambiguousProcess := j.Harness == "codex" && st != nil && st.Source == "processes" && !st.ProcessMatch
	if st != nil {
		s := st.Session
		switch {
		case st.Busy && s != nil && !s.StatusSince.IsZero() && s.StatusSince.After(j.LimitAt):
			d.skip(j, "the session is working again (the agent reports it busy since "+s.StatusSince.Local().Format("15:04")+")")
			return
		case st.MovedOn:
			d.skip(j, "the agent's window moved on to another conversation; leaving the old one alone")
			return
		}
		if s != nil {
			if s.PID > 0 && !ambiguousProcess {
				j.PID = s.PID
			}
			if st.ProcessMatch && (j.Location == nil || j.Location.Kind == "wrapper" && !deliver.WrapperAlive(d.Paths, j.Location.Target)) {
				j.Location = deliver.LocationFromEnv(d.Paths, d.Rules, proc.Environ(s.PID))
			}
			// The agent's own record of its pane is the freshest; only a
			// Continuum wrapper (an even more direct route) takes precedence.
			if pane := live.TmuxPane(s.Tmux); pane != "" && (j.Location == nil || j.Location.Kind != "wrapper") {
				j.Location = &core.Location{Kind: "tmux", Target: pane, Socket: d.tmuxServer(j, s.PID, pane)}
			}
		}
	}
	if j.Location == nil && !ambiguousProcess && j.PID > 0 && d.isAgentPID(j) {
		// A session started before Continuum's hooks: its process's
		// environment says which pane (or wrapper) it runs in.
		if loc := deliver.LocationFromEnv(d.Paths, d.Rules, proc.Environ(j.PID)); loc != nil {
			j.Location = loc
			d.debugf("%s: found its pane from the agent's environment: %s", j.Key, loc.String())
		}
	}
	sessEnded := sess != nil && sess.Ended
	liveClosed := st != nil && st.Checked && !st.Open && (st.Authoritative || st.Source == "pid")
	if j.Location == nil && !sessEnded && !liveClosed && !ambiguousProcess {
		j.Location = d.findPane(j)
	}
	// Compare the current route, after resolving a reopened session.
	if j.Location != nil && j.Location.Kind != "" {
		for _, o := range d.Jobs {
			if o != j && o.State == core.StateDelivered && o.Location != nil &&
				o.Location.String() == j.Location.String() && o.DeliveredAt.After(j.LimitAt.Add(-time.Minute)) {
				d.skip(j, "already resumed through "+o.Key)
				return
			}
		}
	}

	h := d.Rules.Harness(j.Harness)
	msgs, err := messages.Load(d.Paths)
	if err != nil {
		d.warnf("messages.json unreadable, using the default messages: %v", err)
	}
	choice := msgs.Resolve(j, j.Kind == core.KindTransient, d.Config, d.Rules)
	prompt := choice.Text
	d.debugf("%s: message from %s (%d characters)", j.Key, choice.Source, len(prompt))
	strategies := d.Config.StrategyOrder()
	opts := deliver.Options{
		Rules: d.Rules, Paths: d.Paths, Config: d.Config, Prompt: prompt,
		DryRun: d.Config.DryRun, Strategies: strategies, Log: d.debugf, Live: st,
	}
	d.debugf("%s: delivering through %s (location %s)", j.Key, strings.Join(strategies, " → "), j.Location.String())
	res, err := d.DeliverFn(opts, j)
	now := d.Now()
	j.UpdatedAt = now
	switch {
	case errors.Is(err, opencode.ErrChanged):
		d.skip(j, err.Error())
		return
	case errors.Is(err, opencode.ErrBusy), errors.Is(err, deliver.ErrCodexBusy):
		j.DueAt, j.Result = now.Add(15*time.Second), err.Error()
		return
	case errors.Is(err, opencode.ErrUncertain), errors.Is(err, deliver.ErrQueued), errors.Is(err, deliver.ErrUnconfirmed):
		j.State, j.Result = core.StateNeedsUser, err.Error()
		j.ManualResumeOnly = true
		d.history("needs_user", j, map[string]any{"error": err.Error()})
		d.notify(j, "Resume needs attention", err.Error())
		return
	}
	if err != nil && errors.Is(err, deliver.ErrStillOpen) {
		// Open somewhere Continuum can't type (a desktop app, a terminal
		// started before the shell hook). Never start a copy: keep checking,
		// and continue as soon as it is reachable or closed. This doesn't
		// use up attempts; it is bounded by time instead.
		j.Result = err.Error()
		if !j.WaitingOpen {
			j.WaitingOpen = true
			d.history("waiting_open", j, map[string]any{"error": err.Error()})
			msg := h.DisplayName + " is open in a window Continuum can't type into, so it won't start a copy. Continue it there yourself, or leave it: Continuum continues it automatically once it's closed."
			if st != nil && st.DesktopApp != "" {
				msg = "This session is open in " + st.DesktopApp + ", where Continuum can't type. Send any message there to continue now, or leave it: Continuum continues it by itself once " + st.DesktopApp + " lets go of it."
			}
			d.notify(j, "Limit reset", msg)
		}
		if now.Sub(j.LimitAt) > 24*time.Hour {
			j.State = core.StateNeedsUser
			j.Result = "still open somewhere Continuum can't type after 24 hours: " + err.Error()
			d.history("needs_user", j, nil)
			return
		}
		j.DueAt = now.Add(2 * time.Minute)
		d.logf("%s: %v; checking again at %s", j.Key, err, j.DueAt.Format("15:04"))
		return
	}
	if err != nil {
		j.Attempts++
		j.Result = err.Error()
		if errors.Is(err, deliver.ErrGone) {
			// Nothing can ever reach it again: don't retry (and keep the
			// computer awake) for nothing.
			j.State = core.StateFailed
			d.history("failed", j, map[string]any{"error": err.Error()})
			d.notify(j, "Could not resume", deliver.ErrGone.Error())
			d.warnf("%s · not resuming: %s", d.describe(j), deliver.ErrGone.Error())
			return
		}
		if j.Attempts >= d.Rules.Defaults.MaxAttempts {
			j.State = core.StateFailed
			d.history("failed", j, map[string]any{"error": err.Error()})
			d.notify(j, "Could not resume", err.Error())
		} else {
			j.DueAt = now.Add(backoff([]int{60, 180, 600, 1800}, j.Attempts-1))
			d.history("retry", j, map[string]any{"error": err.Error(), "next": j.DueAt.Format(time.RFC3339)})
		}
		d.warnf("%s · could not resume yet (attempt %d, next try %s): %v", d.describe(j), j.Attempts, when(j.DueAt, now), err)
		return
	}
	j.Attempts++
	j.State = core.StateDelivered
	j.WaitingOpen = false
	j.DeliveredAt = now
	j.DeliveredVia = res.Via
	j.Result = res.Detail
	d.history("delivered", j, map[string]any{"via": res.Via, "detail": res.Detail})
	d.logf("▶  %s · resumed via %s: %s", d.describe(j), res.Via, res.Detail)
	d.notify(j, "Resumed", "Continued via "+res.Via)
}

// tmuxServer is the socket of the tmux server pane runs on, for a pane taken
// from the agent's own session list (which names the pane, not the server):
// the agent's environment says, else the server the hooks recorded. A pane id
// like %3 exists on every tmux server, so without it the default server's %3
// would be typed into, whatever runs there.
func (d *Daemon) tmuxServer(j *core.Job, pid int, pane string) string {
	if pid > 0 {
		if loc := mux.LocateFromEnv(d.Rules, proc.Environ(pid)); loc != nil && loc.Kind == "tmux" && loc.Target == pane {
			return loc.Socket
		}
	}
	if j.Location != nil && j.Location.Kind == "tmux" {
		return j.Location.Socket
	}
	return ""
}

// findPane looks for the session's pane when no hook recorded it: the tmux
// pane whose terminal is the agent process's terminal, or else the one tmux
// pane running the agent in the session's directory.
func (d *Daemon) findPane(j *core.Job) *core.Location {
	if j.Cwd == "" && j.PID <= 0 {
		return nil
	}
	if _, ok := d.Rules.Multiplexers["tmux"]; !ok {
		return nil
	}
	if _, err := proc.LookPath("tmux"); err != nil {
		return nil
	}
	argv := []string{"tmux"}
	if sock := d.Config.ScanTmuxSocket; sock != "" {
		argv = append(argv, "-S", sock)
	}
	argv = append(argv, "list-panes", "-a", "-F", "#{pane_id}\t#{pane_current_command}\t#{pane_current_path}\t#{pane_tty}\t#{socket_path}")
	out, err := mux.Runner(5*time.Second, "", argv...)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	// The server's own socket path, so the pane compares equal to the one
	// its hooks report and is never confused with the same pane id on
	// another tmux server.
	socket := func(f []string) string {
		if len(f) >= 5 && strings.TrimSpace(f[4]) != "" {
			return strings.TrimSpace(f[4])
		}
		return d.Config.ScanTmuxSocket
	}
	if j.PID > 0 {
		if tty := proc.TTY(j.PID); tty != "" {
			for _, line := range lines {
				if f := strings.Split(line, "\t"); len(f) >= 4 && f[3] == tty {
					return &core.Location{Kind: "tmux", Target: f[0], Socket: socket(f)}
				}
			}
		}
	}
	if j.Cwd == "" {
		return nil
	}
	h := d.Rules.Harness(j.Harness)
	var found *core.Location
	n := 0
	for _, line := range lines {
		f := strings.Split(line, "\t")
		if len(f) < 3 || !h.IsHarnessProcess(f[1]) {
			continue
		}
		if filepath.Clean(f[2]) == filepath.Clean(j.Cwd) {
			n++
			found = &core.Location{Kind: "tmux", Target: f[0], Socket: socket(f)}
		}
	}
	if n == 1 {
		return found
	}
	return nil
}

type paneLimit struct {
	line    string
	count   int
	kind    string
	seenAt  time.Time
	resetAt time.Time
}

// scanPanes finds limit messages in tmux panes running agents that have no
// transcript source (any harness other than Claude and Codex).
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
		if h == nil || h.Name == "opencode" && d.Config.OpenCodeServer != "" {
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

func (d *Daemon) prune() {
	cutoff := d.Now().Add(-72 * time.Hour)
	for _, j := range d.Jobs {
		// A resume known only by its wrapper (no session id for the agent's
		// own resume command) can never be delivered once that wrapper has
		// exited: wrapper ids are never reused. Don't list it, keep the
		// computer awake for it, or retry it. Not straight away: an event
		// with its session id may still arrive and adopt it (findTwin).
		if j.State == core.StatePending && j.SessionID == "" && j.Location != nil &&
			d.Now().Sub(j.UpdatedAt) > 5*time.Minute &&
			j.Location.Kind == "wrapper" && !deliver.WrapperAlive(d.Paths, j.Location.Target) {
			d.skip(j, "its terminal closed, and without a session id nothing else can continue it")
		}
	}
	for k, j := range d.Jobs {
		if j.LiftFailed && j.ResetKnown && j.ResetAt.After(d.Now()) {
			continue // still keeps answers from lifting its limit (lifted.go)
		}
		if j.State != core.StatePending && j.UpdatedAt.Before(cutoff) {
			delete(d.Jobs, k)
			d.dirty = true
		}
	}
	scut := d.Now().Add(-14 * 24 * time.Hour)
	for k, s := range d.Sessions {
		if s.LastSeen.Before(scut) {
			delete(d.Sessions, k)
			d.dirty = true
		}
	}
	// The resume history: keep the current file and one older one.
	if st, err := os.Stat(d.Paths.History); err == nil && st.Size() > 5<<20 {
		_ = os.Rename(d.Paths.History, d.Paths.History+".1")
	}
	// Folders of wrappers that were killed (kill -9) and couldn't clean up.
	if entries, err := os.ReadDir(d.Paths.Wrappers); err == nil {
		for _, e := range entries {
			dir := filepath.Join(d.Paths.Wrappers, e.Name())
			info, err := e.Info()
			if err != nil || !e.IsDir() || time.Since(info.ModTime()) < 10*time.Minute || deliver.WrapperAlive(d.Paths, e.Name()) {
				continue
			}
			_ = os.RemoveAll(dir)
		}
	}
}

func (d *Daemon) notify(j *core.Job, title, body string) {
	if !d.Config.NotifyEnabled() || d.NotifyFn == nil {
		return
	}
	name := d.Rules.Harness(j.Harness).DisplayName
	if name == "" {
		name = j.Harness
	}
	where := filepath.Base(j.Cwd)
	if where == "." || where == "/" {
		where = ""
	}
	t := "Continuum · " + name
	if where != "" {
		t += " · " + where
	}
	d.NotifyFn(t, title+": "+body)
}

// ---- control ---------------------------------------------------------------

func (d *Daemon) control(cmd string) {
	f := strings.Fields(cmd)
	if len(f) < 2 {
		return // a selector is required; an empty one never means "all"
	}
	action, sel := f[0], f[1]
	if action == "recover" {
		d.recoverCodex(sel)
		return
	}
	for _, j := range d.Jobs {
		if !SelectorMatches(j, sel) {
			continue
		}
		switch action {
		case "cancel":
			if j.State != core.StatePending {
				continue
			}
			j.State = core.StateCancelled
			j.Result = "cancelled by you"
			j.UpdatedAt = d.Now()
			d.history("cancelled", j, nil)
		case "resume-now":
			if j.ManualResumeOnly {
				d.logf("%s: %s", j.Key, j.Result)
				continue
			}
			if j.Kind == core.KindNeverResume {
				d.logf("%s: not resuming: it needs you (billing, credits, spend cap or sign-in)", j.Key)
				continue
			}
			// "all" means every pending resume. A finished one (resumed,
			// cancelled, skipped, failed) is only resumed when named.
			if sel == "all" && j.State != core.StatePending {
				continue
			}
			j.State = core.StatePending
			j.DueAt = d.Now()
			j.UpdatedAt = d.Now()
			d.history("resume-now", j, nil)
		default:
			continue
		}
		d.dirty = true
	}
}

// SelectorMatches reports whether a `cancel`/`resume-now` selector names job
// j: "all", its key (claude:<id>), harness:<prefix>, location (<kind>:<target>
// or harness@<kind>:<target>), its session id, or the start of the session id
// (at least 4 characters).
func SelectorMatches(j *core.Job, sel string) bool {
	switch {
	case sel == "":
		return false
	case sel == "all":
		return true
	case j.Key == sel || j.SessionID == sel:
		return true
	case j.Location != nil && (j.Location.String() == sel || j.Harness+"@"+j.Location.String() == sel):
		return true
	case len(sel) >= 4 && j.SessionID != "" && strings.HasPrefix(j.SessionID, sel):
		return true
	}
	if i := strings.IndexByte(sel, ':'); i > 0 && j.Harness == sel[:i] && j.SessionID != "" {
		prefix := sel[i+1:]
		return len(prefix) >= 4 && strings.HasPrefix(j.SessionID, prefix)
	}
	return false
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// ErrRunning means another daemon holds the lock.
var ErrRunning = errors.New("a Continuum daemon is already running")

func (d *Daemon) skip(j *core.Job, why string) {
	j.State = core.StateSkipped
	j.Result = why
	j.UpdatedAt = d.Now()
	d.dirty = true
	d.history("skipped", j, map[string]any{"reason": why})
	d.logf("·  %s · not resuming: %s", d.describe(j), why)
}

// liveState asks the agent (or the process table) whether the job's session
// is open right now.
func (d *Daemon) liveState(j *core.Job) *live.State {
	if j.Location != nil && j.Location.Kind == opencode.LocationKind {
		return nil // the API verifies the exact session before delivery
	}
	h := d.Rules.Harness(j.Harness)
	if (h.LiveSessions == nil && len(h.DesktopApps) == 0) || j.SessionID == "" {
		return nil
	}
	bin, _ := deliver.Binary(d.Config, h)
	st := live.Default().Lookup(h, live.Query{
		SessionID: j.SessionID, Cwd: j.Cwd, PID: j.PID, Bin: bin, Homes: d.homes(),
		Entrypoint: j.Entrypoint, TrustRegistry: d.trustRegistry(h),
	})
	return &st
}

// keepAwake holds off idle sleep while a resume is due soon or a resumed
// session is still working, so a locked, unattended computer keeps going.
func (d *Daemon) keepAwake() {
	if d.Awake == nil {
		return
	}
	on, why := d.wantAwake()
	changed, err := d.Awake.Set(on, why)
	if err != nil {
		d.warnf("keep awake: %v", err)
	} else if changed && on {
		d.logf("keeping the computer awake: %s", why)
	} else if changed {
		d.logf("no longer keeping the computer awake")
	}
}

func (d *Daemon) wantAwake() (bool, string) {
	horizon := d.Config.KeepAwake()
	if horizon <= 0 || d.Config.DryRun {
		return false, ""
	}
	now := d.Now()
	for _, j := range d.Jobs {
		switch j.State {
		case core.StatePending:
			// A session held open in a desktop app waits for the user, who
			// is at the computer anyway: no reason to keep it awake.
			if j.DueAt.Sub(now) <= horizon && !j.WaitingOpen {
				return true, "a resume is due at " + j.DueAt.Local().Format("15:04")
			}
		case core.StateDelivered:
			since := now.Sub(j.DeliveredAt)
			if since >= horizon {
				continue
			}
			if since < 10*time.Minute {
				return true, "a session was just resumed"
			}
			if s := d.Sessions[store.SessionKey(j.Harness, j.SessionID)]; s != nil && now.Sub(s.LastSeen) < 10*time.Minute {
				return true, "a resumed session is still working"
			}
		}
	}
	return false, ""
}

// applyScope follows changes to the scope: a session chosen after it stalled
// is scheduled, and a pending resume that is no longer in scope is dropped.
func (d *Daemon) applyScope() {
	_ = messages.Update(d.Paths, func(m *messages.Messages) (bool, error) {
		return m.PruneWrappers(func(id string) bool { return deliver.WrapperAlive(d.Paths, id) }), nil
	})
	// Read afresh inside a file lock so pruning never undoes a concurrent `continuum choose`.
	if sc, err := scope.Update(d.Paths, func(cur scope.Scope) (scope.Scope, bool, error) {
		changed := cur.PruneWrappers(func(id string) bool { return deliver.WrapperAlive(d.Paths, id) })
		return cur, changed, nil
	}); err == nil {
		d.Scope = sc
	} else {
		d.logf("scope: %v", err)
	}
	now := d.Now()
	for _, j := range d.Jobs {
		switch j.State {
		case core.StatePending:
			if ok, why := d.Scope.Covers(j); !ok {
				j.State, j.Result, j.UpdatedAt = core.StateIgnored, why, now
				d.history("ignored", j, map[string]any{"reason": why})
				d.logf("%s: %s", j.Key, why)
				d.dirty = true
			}
		case core.StateIgnored:
			if ok, _ := d.Scope.Covers(j); ok && j.LimitAt.After(now.Add(-d.Config.CatchUp())) {
				j.State, j.Result, j.UpdatedAt = core.StatePending, "", now
				d.reschedule(j)
				d.dirty = true
				if j.State == core.StatePending {
					d.history("scheduled", j, map[string]any{"due": j.DueAt.Format(time.RFC3339), "kind": j.Kind, "source": "chosen"})
					d.logf("%s: chosen, resume at %s", j.Key, j.DueAt.Format(time.RFC1123))
					d.notify(j, "Chosen", "Will continue at "+j.DueAt.In(d.Local).Format("Mon 3:04 PM"))
				}
			}
		}
	}
}

// applyTiming re-plans scheduled resumes when the wait settings change
// (`continuum grace`, delay_after_reset_seconds), so a change takes effect
// for sessions that are already waiting.
func (d *Daemon) applyTiming() {
	sig := fmt.Sprint(d.delay(), d.Config.NativeGrace)
	if d.timingSig == sig {
		return
	}
	first := d.timingSig == ""
	d.timingSig = sig
	if first {
		return
	}
	for _, j := range d.Jobs {
		if j.State == core.StatePending && j.ResetKnown && j.Attempts == 0 {
			old := j.DueAt
			d.reschedule(j)
			if !j.DueAt.Equal(old) {
				d.dirty = true
				d.logf("%s: wait settings changed, resume now at %s", j.Key, j.DueAt.Format(time.RFC1123))
			}
		}
	}
}

// paneHarness identifies the agent in a pane: by its process name, or, for an
// agent running under an interpreter (tmux then says "node" or "python"), by
// the command line of the pane's foreground process. Claude and Codex are
// skipped: their transcripts and hooks report them more precisely.
func (d *Daemon) paneHarness(p mux.Pane) *rules.Harness {
	names := d.Rules.HarnessNames()
	pick := func(match func(*rules.Harness) bool) *rules.Harness {
		for _, name := range names {
			if name == "claude" || name == "codex" || name == "generic" || !d.Config.HarnessEnabled(name) {
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

func (d *Daemon) debugf(format string, args ...any) { d.Log.Debugf(format, args...) }
func (d *Daemon) warnf(format string, args ...any)  { d.Log.Warnf(format, args...) }

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	if id == "" {
		return "-"
	}
	return id
}

// describe names a job for people: agent · folder · short session id.
func (d *Daemon) describe(j *core.Job) string {
	name := j.Harness
	if h := d.Rules.Harness(j.Harness); h != nil && h.DisplayName != "" {
		name = h.DisplayName
	}
	parts := []string{name}
	if dir := filepath.Base(j.Cwd); j.Cwd != "" && dir != "." && dir != "/" {
		parts = append(parts, dir)
	}
	if j.SessionID != "" {
		parts = append(parts, short(j.SessionID))
	} else if j.Location != nil {
		parts = append(parts, j.Location.String())
	}
	return strings.Join(parts, " · ")
}

// when describes a time relative to now: "now", "10:01 (in 2h10m)",
// "Wed 10:01 (in 1d2h)".
func when(t, now time.Time) string {
	d := t.Sub(now)
	if d <= 0 {
		return "now"
	}
	in := d.Round(time.Minute).String()
	in = strings.TrimSuffix(in, "0s")
	if strings.HasSuffix(in, "h0m") {
		in = strings.TrimSuffix(in, "0m")
	}
	if d < time.Minute {
		in = d.Round(time.Second).String()
	}
	layout := "15:04"
	if t.Local().YearDay() != now.Local().YearDay() {
		layout = "Mon 15:04"
	}
	return t.Local().Format(layout) + " (in " + in + ")"
}

// name is an agent's display name.
func (d *Daemon) name(harness string) string {
	if h := d.Rules.Harness(harness); h != nil && h.DisplayName != "" {
		return h.DisplayName
	}
	return harness
}

// isAgentPID: the job's recorded process still runs and is the agent (a pid
// can be reused by another program once the agent exits).
func (d *Daemon) isAgentPID(j *core.Job) bool {
	_, name, ok := proc.Info(j.PID)
	return ok && d.Rules.Harness(j.Harness).IsHarnessProcess(name)
}
