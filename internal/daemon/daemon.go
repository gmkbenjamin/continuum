// Package daemon is Continuum's scheduler: it collects events, keeps one job
// per stalled session, waits for the reset, checks the session is still
// stalled, and delivers the resume prompt.
package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/awake"
	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/logx"
	"github.com/continuum-dev/continuum/internal/notify"
	"github.com/continuum-dev/continuum/internal/resettime"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/scope"
	"github.com/continuum-dev/continuum/internal/sources"
	"github.com/continuum-dev/continuum/internal/store"
)

type offsetsFile struct {
	Claude      *sources.Tailer                `json:"claude"`
	Codex       *sources.Tailer                `json:"codex"`
	Antigravity *sources.Tailer                `json:"antigravity,omitempty"`
	State       map[string]*sources.CodexState `json:"codex_state"`
	// ClaudeState: transcripts whose last turn is wrapping up after a limit.
	ClaudeState      map[string]*sources.ClaudeState      `json:"claude_state,omitempty"`
	AntigravityState map[string]*sources.AntigravityState `json:"antigravity_state,omitempty"`
}

// Daemon holds the scheduler state.
type Daemon struct {
	Paths     core.Paths
	Config    core.Config
	Scope     scope.Scope
	Rules     *rules.Rules
	Jobs      map[string]*core.Job
	Sessions  map[string]*store.Session
	Instances map[string]*store.Instance
	Offsets   offsetsFile
	Local     *time.Location

	Now       func() time.Time
	DeliverFn func(deliver.Options, *core.Job) (deliver.Result, error)
	NotifyFn  func(title, body string)
	Log       *logx.Logger
	// LiveFn reports whether a job's session is open right now (nil: unknown).
	LiveFn func(*core.Job) *live.State
	// HoldsFn: whether the agent lists pid as holding session id (nil: ask
	// the agent through LiveFn).
	HoldsFn func(harness, id string, pid int) bool
	// PaneSessionFn: the Claude Code conversation held by the process at a
	// location (nil: ask Claude Code and the processes' environments).
	PaneSessionFn func(loc *core.Location) paneProcess
	// Awake keeps the computer from idle-sleeping while work is pending.
	Awake *awake.Keeper
	// Compat, when set, enables the update watch (see compat.go).
	Compat *Compat
	// PublishSnapshot enables publishing the canonical live Snapshot to
	// Paths.State on each dirty tick / periodic interval for CLI readers.
	PublishSnapshot bool
	// pidProbe: when observeExactPID may next ask each agent's session list.
	pidProbe map[string]*probeState

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
	lastClosedCheck  time.Time
	lastSnapshot     time.Time
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
		Instances: map[string]*store.Instance{},
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
	if err := store.LoadJSON(p.Instances, &d.Instances); err != nil || d.Instances == nil {
		d.Instances = map[string]*store.Instance{}
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
	if d.Offsets.Antigravity == nil {
		d.Offsets.Antigravity = sources.NewTailer()
	}
	d.Offsets.Claude.InitialBytes, d.Offsets.Codex.InitialBytes, d.Offsets.Antigravity.InitialBytes = 512*1024, 512*1024, 512*1024
	if d.Offsets.Claude.Offsets == nil {
		d.Offsets.Claude.Offsets = map[string]int64{}
	}
	if d.Offsets.Codex.Offsets == nil {
		d.Offsets.Codex.Offsets = map[string]int64{}
	}
	if d.Offsets.Antigravity.Offsets == nil {
		d.Offsets.Antigravity.Offsets = map[string]int64{}
	}
	if d.Offsets.State == nil {
		d.Offsets.State = map[string]*sources.CodexState{}
	}
	if d.Offsets.ClaudeState == nil {
		d.Offsets.ClaudeState = map[string]*sources.ClaudeState{}
	}
	if d.Offsets.AntigravityState == nil {
		d.Offsets.AntigravityState = map[string]*sources.AntigravityState{}
	}
	return d, nil
}

// reconcileLoadedJobs repairs pending jobs loaded from jobs.json whose reset
// time could not be parsed by an older binary, that came from a subagent
// transcript, or that matched a user input prompt line.
func (d *Daemon) reconcileLoadedJobs() {
	for _, j := range d.Jobs {
		if j == nil || j.State != core.StatePending {
			continue
		}
		if DriverFor(j.Harness).IgnoreTranscript(j.TranscriptPath) {
			markJobSkipped(j, "ignored subagent conversation", d.Now())
			d.dirty = true
			d.logf("·  %s · skipped subagent conversation", d.describe(j))
			continue
		}
		if j.Message == "" {
			continue
		}
		if len(j.Sources) == 1 && j.Sources[0] == "wrapper" && d.Rules != nil && d.Rules.Harness(j.Harness).Classify(j.Message, d.Rules.Defaults.TailLines) == nil {
			markJobSkipped(j, "ignored user input prompt", d.Now())
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

// Tick runs one pass: ingest, scan, reconcile instances, deliver, prune, save.
func (d *Daemon) Tick() {
	d.reload()
	d.reconcileLoadedJobs()
	d.ingestSpool()
	d.reconcileInstances()
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
	wasDirty := d.dirty
	if d.dirty {
		d.save()
	}
	d.maybePublishSnapshot(wasDirty)
}

func (d *Daemon) save() {
	if err := store.SaveJSON(d.Paths.Jobs, d.Jobs); err != nil {
		d.warnf("save jobs: %v", err)
	}
	if err := store.SaveJSON(d.Paths.Sessions, d.Sessions); err != nil {
		d.warnf("save sessions: %v", err)
	}
	if err := store.SaveJSON(d.Paths.Instances, d.Instances); err != nil {
		d.warnf("save instances: %v", err)
	}
	if err := store.SaveJSON(d.Paths.Offsets, d.Offsets); err != nil {
		d.warnf("save offsets: %v", err)
	}
	d.dirty = false
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
	if DriverFor(ev.Harness).IgnoreScreenEvents(d.Config) && screenSourced(ev.Source) {
		return // an authoritative channel (e.g. OpenCode server) owns this agent
	}
	if ev.At.IsZero() {
		ev.At = d.Now()
	}
	switch ev.Kind {
	case core.KindWrapperStart:
		d.wrapperStarted(ev)
		return
	case core.KindWrapperExit:
		d.wrapperExited(ev)
		return
	}
	// Decided once, before the record changes: a second process holding the
	// same conversation quit, not the one the session's record names.
	copyQuit := d.copyEnded(ev)
	sess := d.touchSession(ev, copyQuit)
	d.observeExactPID(ev)

	switch ev.Kind {
	case core.KindActivity:
		d.settleResume(ev)
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
		if copyQuit {
			return // a copy quit, not the process the resume goes to
		}
		if j := d.jobFor(ev); j != nil && (j.State == core.StatePending || j.State == core.StateNeedsUser) {
			if !d.Config.ClosedSessionsTracked() {
				d.skip(j, "the session closed (track_closed_sessions is off)")
				return
			}
			// The agent quit: its pane (or wrapper) now holds something else, so
			// a pending resume must not be typed there. It continues the closed
			// session through the agent's own command instead, if it has one.
			if j.State == core.StatePending && (j.Location != nil || j.PID > 0) {
				d.debugf("%s: the session ended; forgetting its pane %s", j.Key, j.Location.String())
				j.Location, j.PID = nil, 0
				j.UpdatedAt = d.Now()
				d.dirty = true
			}
		}
	case core.KindTurnFailed:
		d.turnFailed(ev)
		d.settleResume(ev)
	case core.KindNativeGaveUp:
		if j := d.jobFor(ev); j != nil && j.State == core.StatePending {
			j.NativeGaveUp = true
			j.UpdatedAt = d.Now()
			d.reschedule(j)
			d.dirty = true
		}
	case core.KindReply:
		d.settleResume(ev)
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
	now := d.Now()
	for _, j := range d.Jobs {
		if !SelectorMatches(j, sel) {
			continue
		}
		switch action {
		case "cancel":
			if j.State != core.StatePending {
				continue
			}
			markJobCancelled(j, "cancelled by you", now)
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
			markJobResumeNow(j, now)
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
	markJobSkipped(j, why, d.Now())
	d.dirty = true
	d.history("skipped", j, map[string]any{"reason": why})
	d.logf("·  %s · not resuming: %s", d.describe(j), why)
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
