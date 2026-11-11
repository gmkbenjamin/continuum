// Package daemon is Continuum's scheduler: it collects events, keeps one job
// per stalled session, waits for the reset, checks the session is still
// stalled, and delivers the resume prompt.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
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
	"github.com/continuum-dev/continuum/internal/mux"
	"github.com/continuum-dev/continuum/internal/notify"
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
	Logger    *log.Logger
	// LiveFn reports whether a job's session is open right now (nil: unknown).
	LiveFn func(*core.Job) *live.State
	// Awake keeps the computer from idle-sleeping while work is pending.
	Awake *awake.Keeper

	baseline     bool
	timingSig    string
	paneSeen     map[string]string
	lastScan     time.Time
	lastPaneScan time.Time
	dirty        bool
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
		Logger:    log.New(os.Stderr, "", log.LstdFlags),
		Sessions:  map[string]*store.Session{},
		paneSeen:  map[string]string{},
		Awake:     awake.New(),
	}
	d.LiveFn = d.liveState
	d.reload()
	var err error
	if d.Jobs, err = store.LoadJobs(p.Jobs); err != nil {
		d.Logger.Printf("jobs file unreadable, starting empty: %v", err)
		d.Jobs = map[string]*core.Job{}
	}
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
	return d, nil
}

func (d *Daemon) reload() {
	cfg, err := core.LoadConfig(d.Paths.Config)
	if err != nil {
		d.logf("config %s ignored: %v", d.Paths.Config, err)
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
	if d.Logger != nil {
		d.Logger.Printf(format, args...)
	}
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
	d.ingestSpool()
	now := d.Now()
	if d.Config.TranscriptsWatched() && now.Sub(d.lastScan) >= 2*d.Config.Poll() {
		d.lastScan = now
		d.scanSources()
	}
	if d.Config.PanesScanned() && now.Sub(d.lastPaneScan) >= 15*time.Second {
		d.lastPaneScan = now
		d.scanPanes()
	}
	d.applyScope()
	d.applyTiming()
	d.runDue()
	d.prune()
	d.keepAwake()
	if d.dirty {
		d.save()
	}
}

func (d *Daemon) save() {
	if err := store.SaveJSON(d.Paths.Jobs, d.Jobs); err != nil {
		d.logf("save jobs: %v", err)
	}
	if err := store.SaveJSON(d.Paths.Sessions, d.Sessions); err != nil {
		d.logf("save sessions: %v", err)
	}
	if err := store.SaveJSON(d.Paths.Offsets, d.Offsets); err != nil {
		d.logf("save offsets: %v", err)
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
			continue
		}
		d.Handle(ev)
	}
}

func (d *Daemon) scanSources() {
	since := d.Now().Add(-d.Config.CatchUp())
	baseline := d.baseline
	d.baseline = false

	if d.Config.HarnessEnabled("claude") {
		root := filepath.Join(d.Config.ClaudeDir(), "projects")
		for path, lines := range d.Offsets.Claude.Scan(root, since, 3) {
			if baseline {
				continue
			}
			for _, ev := range sources.ParseClaude(d.Rules, path, lines, d.Local) {
				d.Handle(ev)
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
			if baseline {
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
	d.dirty = true
}

// Handle applies one event.
func (d *Daemon) Handle(ev core.Event) {
	if !d.Config.IsEnabled() {
		return
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
	if ev.At.IsZero() {
		ev.At = d.Now()
	}
	sess := d.touchSession(ev)

	switch ev.Kind {
	case core.KindActivity:
		for _, j := range d.Jobs {
			if j.State != core.StatePending || !ev.At.After(j.LimitAt.Add(time.Second)) {
				continue
			}
			sameSession := ev.SessionID != "" && j.SessionID == ev.SessionID && j.Harness == ev.Harness
			sameLoc := ev.Location != nil && j.Location != nil && ev.Location.String() == j.Location.String()
			if !sameSession && !sameLoc {
				continue
			}
			j.State = core.StateSkipped
			j.Result = "the session continued on its own or you continued it"
			j.UpdatedAt = d.Now()
			d.dirty = true
			d.history("skipped", j, map[string]any{"reason": j.Result})
			d.logf("%s: %s", j.Key, j.Result)
		}
	case core.KindNativeGaveUp:
		if j := d.jobFor(ev); j != nil && j.State == core.StatePending {
			j.NativeGaveUp = true
			j.UpdatedAt = d.Now()
			d.reschedule(j)
			d.dirty = true
		}
	case core.KindUsageLimit, core.KindTransient, core.KindNeverResume:
		d.limitEvent(ev, sess)
	}
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
	if ev.At.Before(now.Add(-d.Config.CatchUp())) {
		return
	}
	key := jobKey(ev)
	if key == "" {
		d.logf("ignoring %s %s event with no session or location", ev.Harness, ev.Kind)
		return
	}
	j := d.Jobs[key]
	if twin := d.findTwin(ev, key); twin != nil {
		if ev.SessionID != "" && twin.SessionID == "" {
			// Re-key the pane/wrapper job under the session it belongs to.
			delete(d.Jobs, twin.Key)
			twin.Key, twin.SessionID, twin.Harness = key, ev.SessionID, ev.Harness
			if old := d.Jobs[key]; old != nil && old.State == core.StatePending {
				twin.Attempts = old.Attempts
			}
			d.Jobs[key] = twin
		}
		d.merge(twin, ev)
		return
	}
	if j != nil {
		switch {
		case j.State == core.StatePending && !ev.At.After(j.LimitAt.Add(2*time.Minute)):
			// The same limit seen by another observer (hook + transcript).
			d.merge(j, ev)
			return
		case (j.State == core.StateDelivered) && !ev.At.After(j.DeliveredAt):
			return // an old line about a limit already handled
		case j.State == core.StateCancelled && !ev.At.After(j.UpdatedAt):
			return
		case j.State == core.StateNeedsUser && !ev.At.After(j.LimitAt.Add(2*time.Minute)):
			return
		case j.State == core.StateIgnored && !ev.At.After(j.LimitAt.Add(2*time.Minute)):
			d.merge(j, ev)
			return
		case !ev.At.After(j.LimitAt):
			return
		}
	}
	attempts := 0
	stale := false
	if j != nil && j.State == core.StateDelivered && ev.At.Sub(j.DeliveredAt) < 6*time.Hour {
		attempts = j.Attempts // the resume hit a limit again: keep counting
		// The same reset time as the limit already resumed means either an old
		// message seen again (a redraw) or a reset that has not happened yet.
		// Either way, do not retry straight away.
		stale = !ev.ResetAt.IsZero() && j.ResetKnown && !ev.ResetAt.After(j.ResetAt)
	}
	nj := &core.Job{
		Key: key, Harness: ev.Harness, SessionID: ev.SessionID, Cwd: ev.Cwd,
		TranscriptPath: ev.TranscriptPath, TranscriptSize: ev.TranscriptSize,
		Location: ev.Location, PID: ev.PID, Entrypoint: ev.Entrypoint,
		Kind: ev.Kind, Message: ev.Message, ResetAt: ev.ResetAt, ResetKnown: !ev.ResetAt.IsZero(),
		LimitAt: ev.At, CreatedAt: now, UpdatedAt: now, Attempts: attempts,
		State: core.StatePending, Sources: []string{ev.Source},
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
	if nj.Attempts >= d.Rules.Defaults.MaxAttempts {
		nj.State = core.StateNeedsUser
		nj.Result = fmt.Sprintf("stopped after %d attempts", nj.Attempts)
		d.history("needs_user", nj, nil)
		d.notify(nj, "Stopped retrying", nj.Result)
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
	d.reschedule(nj)
	if nj.State == core.StatePending {
		if ok, why := d.Scope.Covers(nj); !ok {
			nj.State, nj.Result = core.StateIgnored, why
			d.history("ignored", nj, map[string]any{"reason": why})
			d.logf("%s: %s, %s", key, nj.Kind, why)
			return
		}
		d.history("scheduled", nj, map[string]any{"due": nj.DueAt.Format(time.RFC3339), "kind": nj.Kind, "source": ev.Source})
		d.logf("%s: %s, resume at %s (%s)", key, nj.Kind, nj.DueAt.Format(time.RFC1123), ev.Source)
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
	if !ev.ResetAt.IsZero() && (!j.ResetKnown || ev.ResetAt.After(j.ResetAt)) {
		j.ResetAt, j.ResetKnown = ev.ResetAt, true
	}
	if j.Message == "" {
		j.Message = ev.Message
	}
	j.UpdatedAt = d.Now()
	d.reschedule(j)
	d.dirty = true
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

// resetHint asks an external command when the
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
	if d.sessionActiveSince(j) {
		j.State = core.StateSkipped
		j.Result = "the session continued on its own or you continued it"
		j.UpdatedAt = d.Now()
		d.history("skipped", j, map[string]any{"reason": j.Result})
		d.logf("%s: %s", j.Key, j.Result)
		return
	}
	// Never type into the same pane twice for one stall.
	if j.Location != nil && j.Location.Kind != "" {
		for _, o := range d.Jobs {
			if o != j && o.State == core.StateDelivered && o.Location != nil &&
				o.Location.String() == j.Location.String() && o.DeliveredAt.After(j.LimitAt.Add(-time.Minute)) {
				j.State = core.StateSkipped
				j.Result = "already resumed through " + o.Key
				j.UpdatedAt = d.Now()
				d.history("skipped", j, map[string]any{"reason": j.Result})
				return
			}
		}
	}
	// Refresh what we know about where the session lives.
	if s := d.Sessions[store.SessionKey(j.Harness, j.SessionID)]; s != nil {
		if s.Location != nil && (j.Location == nil || s.LastSeen.After(j.LimitAt)) {
			j.Location = s.Location
		}
		if s.PID > 0 {
			j.PID = s.PID
		}
	}
	var st *live.State
	if d.LiveFn != nil {
		st = d.LiveFn(j)
	}
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
			if s.PID > 0 {
				j.PID = s.PID
			}
			// The agent's own record of its pane is the freshest; only a
			// Continuum wrapper (an even more direct route) takes precedence.
			if pane := live.TmuxPane(s.Tmux); pane != "" && (j.Location == nil || j.Location.Kind != "wrapper") {
				j.Location = &core.Location{Kind: "tmux", Target: pane}
			}
		}
	}
	if j.Location == nil {
		j.Location = d.findPane(j)
	}

	h := d.Rules.Harness(j.Harness)
	prompt := h.Prompt(d.Rules.Defaults)
	if p, ok := d.Config.ResumePrompts[j.Harness]; ok && p != "" {
		prompt = p
	}
	if j.Kind == core.KindTransient {
		prompt = "The previous request failed with a temporary API error. Continue the task from where it stopped; do not repeat work that is already complete."
		if p, ok := d.Config.ResumePrompts[j.Harness+"_transient"]; ok && p != "" {
			prompt = p
		}
	}
	strategies := d.Config.StrategyOrder()
	opts := deliver.Options{
		Rules: d.Rules, Paths: d.Paths, Config: d.Config, Prompt: prompt,
		DryRun: d.Config.DryRun, Strategies: strategies, Log: d.logf, Live: st,
	}
	res, err := d.DeliverFn(opts, j)
	now := d.Now()
	j.UpdatedAt = now
	if err != nil {
		j.Attempts++
		j.Result = err.Error()
		if errors.Is(err, deliver.ErrStillOpen) && j.Attempts == 1 {
			d.notify(j, "Waiting for you", h.DisplayName+" is still open in a window Continuum can't type into, so it won't start a copy. Continue it there, or start agents with `continuum run` (installed as a shell hook) or in tmux.")
		}
		if j.Attempts >= d.Rules.Defaults.MaxAttempts {
			j.State = core.StateFailed
			d.history("failed", j, map[string]any{"error": err.Error()})
			d.notify(j, "Could not resume", err.Error())
		} else {
			j.DueAt = now.Add(backoff([]int{60, 180, 600, 1800}, j.Attempts-1))
			d.history("retry", j, map[string]any{"error": err.Error(), "next": j.DueAt.Format(time.RFC3339)})
		}
		d.logf("%s: delivery failed: %v", j.Key, err)
		return
	}
	j.Attempts++
	j.State = core.StateDelivered
	j.DeliveredAt = now
	j.DeliveredVia = res.Via
	j.Result = res.Detail
	d.history("delivered", j, map[string]any{"via": res.Via, "detail": res.Detail})
	d.logf("%s: resumed via %s: %s", j.Key, res.Via, res.Detail)
	d.notify(j, "Resumed", "Continued via "+res.Via)
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
	out, err := mux.Runner(5*time.Second, "", "tmux", "list-panes", "-a", "-F", "#{pane_id}\t#{pane_current_command}\t#{pane_current_path}\t#{pane_tty}")
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if j.PID > 0 {
		if tty := proc.TTY(j.PID); tty != "" {
			for _, line := range lines {
				if f := strings.Split(line, "\t"); len(f) >= 4 && f[3] == tty {
					return &core.Location{Kind: "tmux", Target: f[0]}
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
			found = &core.Location{Kind: "tmux", Target: f[0]}
		}
	}
	if n == 1 {
		return found
	}
	return nil
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
	panes, err := mux.List(d.Rules, "tmux", "")
	if err != nil {
		return
	}
	for _, p := range panes {
		h := d.paneHarness(p)
		if h == nil {
			continue
		}
		text, err := mux.Capture(d.Rules, p.Location)
		if err != nil {
			continue
		}
		// Only the bottom of the pane counts: that is where a stalled agent's
		// last message sits, just above its input box.
		m := h.Classify(text, 12)
		if m == nil {
			continue
		}
		// The message stays on screen after a resume. Only a changed screen
		// with a limit at the bottom is a new stall.
		screen := strings.Join(rules.TailLines(text, 12), "\n")
		if d.paneSeen[p.Location.String()] == screen {
			continue
		}
		d.paneSeen[p.Location.String()] = screen
		ev := core.Event{Harness: h.Name, Kind: m.Kind, Message: m.Text, At: d.Now(), Source: "pane-scan", Location: p.Location}
		if h.NeverResume(m.Text) {
			ev.Kind = core.KindNeverResume
		}
		if ev.Kind == core.KindUsageLimit {
			if t, ok := resettime.Parse(m.Text, ev.At, d.Local); ok {
				ev.ResetAt = t
			}
		}
		d.Handle(ev)
	}
}

func (d *Daemon) prune() {
	cutoff := d.Now().Add(-72 * time.Hour)
	for k, j := range d.Jobs {
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
	if len(f) == 0 {
		return
	}
	match := func(j *core.Job, sel string) bool {
		return sel == "all" || j.Key == sel || strings.HasPrefix(j.SessionID, sel) || strings.HasSuffix(j.Key, sel)
	}
	sel := "all"
	if len(f) > 1 {
		sel = f[1]
	}
	for _, j := range d.Jobs {
		if j.State != core.StatePending && f[0] != "resume-now" {
			continue
		}
		if !match(j, sel) {
			continue
		}
		switch f[0] {
		case "cancel":
			j.State = core.StateCancelled
			j.Result = "cancelled by you"
			j.UpdatedAt = d.Now()
			d.history("cancelled", j, nil)
		case "resume-now":
			if j.State == core.StateDelivered && sel == "all" {
				continue
			}
			j.State = core.StatePending
			j.DueAt = d.Now()
			j.UpdatedAt = d.Now()
			d.history("resume-now", j, nil)
		}
		d.dirty = true
	}
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
	d.logf("%s: %s", j.Key, why)
}

// liveState asks the agent (or the process table) whether the job's session
// is open right now.
func (d *Daemon) liveState(j *core.Job) *live.State {
	h := d.Rules.Harness(j.Harness)
	if h.LiveSessions == nil || j.SessionID == "" {
		return nil
	}
	bin, _ := deliver.Binary(d.Config, h)
	st := live.Default().Lookup(h, live.Query{
		SessionID: j.SessionID, Cwd: j.Cwd, PID: j.PID, Bin: bin,
		Homes: map[string]string{"claude_home": d.Config.ClaudeDir(), "codex_home": d.Config.CodexDir()},
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
		d.logf("keep awake: %v", err)
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
			if j.DueAt.Sub(now) <= horizon {
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
	if d.Scope.PruneWrappers(func(id string) bool { return deliver.WrapperAlive(d.Paths, id) }) {
		if err := d.Scope.Save(d.Paths); err != nil {
			d.logf("scope: %v", err)
		}
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
	tty, err := mux.Runner(5*time.Second, "", "tmux", "display-message", "-p", "-t", p.Location.Target, "#{pane_tty}")
	if err != nil {
		return nil
	}
	line := proc.ForegroundCommand(strings.TrimSpace(tty))
	if line == "" {
		return nil
	}
	return pick(func(h *rules.Harness) bool { return h.MatchesCommandLine(line) })
}
