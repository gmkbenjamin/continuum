package daemon

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/logx"
	"github.com/continuum-dev/continuum/internal/messages"
	"github.com/continuum-dev/continuum/internal/mux"
	"github.com/continuum-dev/continuum/internal/opencode"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/sources"
	"github.com/continuum-dev/continuum/internal/store"
)

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
	case "antigravity":
		evs = sources.ParseAntigravity(d.Rules, j.TranscriptPath, lines, d.Local)
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
	if drv := DriverFor(j.Harness); drv.IgnoreScreenEvents(d.Config) && !drv.ManagesOwnLifecycle(j) {
		d.skip(j, d.name(j.Harness)+" now uses the configured server for resume")
		return
	}
	d.debugf("%s: due (attempt %d, reset %s, limit at %s); checking", j.Key, j.Attempts+1, j.ResetAt.Local().Format("15:04"), j.LimitAt.Local().Format("15:04"))
	if !d.adoptPaneSession(j) {
		return
	}
	if d.sessionActiveSince(j) {
		markJobSkipped(j, "the session continued on its own or you continued it", d.Now())
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
	DriverFor(j.Harness).SanitizeJob(j, sess)
	if j.SessionID != "" && (j.Location == nil || (j.Location.Kind == "wrapper" && !deliver.WrapperAlive(d.Paths, j.Location.Target))) {
		if w, ok := d.boundWrapper(j.Harness, j.SessionID); ok {
			j.Location = &core.Location{Kind: "wrapper", Target: w.ID}
			d.debugf("%s: bound to uniquely matching live wrapper %s", j.Key, w.ID)
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
	if !d.Config.ClosedSessionsTracked() && d.sessionClosed(j, sess, st) {
		d.skip(j, "the session closed (track_closed_sessions is off)")
		return
	}
	if pid := d.staleCopy(j, sess, st); pid > 0 {
		d.waitForStaleCopy(j, pid)
		return
	}
	if j.StaleCopyPID > 0 {
		// That wait is over (the copy closed, or is in use now).
		clearJobWait(j)
	}
	ambiguousProcess := st != nil && st.Source == "processes" && !st.ProcessMatch
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
	j.DeliveringAt = d.Now()
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
		markJobNeedsUser(j, err.Error(), true, now)
		d.history("needs_user", j, map[string]any{"error": err.Error()})
		d.notify(j, "Resume needs attention", err.Error())
		return
	}
	if err != nil && errors.Is(err, deliver.ErrStillOpen) {
		// Open somewhere Continuum can't type (a desktop app, a terminal
		// started before the shell hook). Never start a copy: keep checking,
		// and continue as soon as it is reachable or closed. This doesn't
		// use up attempts; it is bounded by time instead.
		if markJobWaitingOpen(j, err.Error(), now) {
			d.history("waiting_open", j, map[string]any{"error": err.Error()})
			msg := h.DisplayName + " is open in a window Continuum can't type into, so it won't start a copy. Continue it there yourself, or leave it: Continuum continues it automatically once it's closed."
			if st != nil && st.DesktopApp != "" {
				msg = "This session is open in " + st.DesktopApp + ", where Continuum can't type. Send any message there to continue now, or leave it: Continuum continues it by itself once " + st.DesktopApp + " lets go of it."
			}
			d.notify(j, "Limit reset", msg)
		}
		if now.Sub(waitingSince(j)) > 24*time.Hour {
			markJobNeedsUser(j, "still open somewhere Continuum can't type after 24 hours: "+err.Error(), false, now)
			d.history("needs_user", j, nil)
			d.notify(j, "Resume needs you", "Still open somewhere Continuum can't type after 24 hours. Continue it yourself.")
			return
		}
		j.DueAt = now.Add(2 * time.Minute)
		d.logf("%s: %v; checking again at %s", j.Key, err, j.DueAt.Format("15:04"))
		return
	}
	if err != nil {
		j.Attempts++
		if errors.Is(err, deliver.ErrGone) {
			// Nothing can ever reach it again: don't retry (and keep the
			// computer awake) for nothing.
			markJobFailed(j, err.Error(), now)
			d.history("failed", j, map[string]any{"error": err.Error()})
			d.notify(j, "Could not resume", deliver.ErrGone.Error())
			d.warnf("%s · not resuming: %s", d.describe(j), deliver.ErrGone.Error())
			return
		}
		if j.Attempts >= d.Rules.Defaults.MaxAttempts {
			markJobFailed(j, err.Error(), now)
			d.history("failed", j, map[string]any{"error": err.Error()})
			d.notify(j, "Could not resume", err.Error())
		} else {
			j.Result = err.Error()
			j.DueAt = now.Add(backoff([]int{60, 180, 600, 1800}, j.Attempts-1))
			d.history("retry", j, map[string]any{"error": err.Error(), "next": j.DueAt.Format(time.RFC3339)})
		}
		d.warnf("%s · could not resume yet (attempt %d, next try %s): %v", d.describe(j), j.Attempts, when(j.DueAt, now), err)
		return
	}
	j.Attempts++
	markJobDelivered(j, res.Via, res.Detail, now)
	d.history("delivered", j, map[string]any{"via": res.Via, "detail": res.Detail})
	d.logf("▶  %s · resumed via %s: %s", d.describe(j), res.Via, res.Detail)
	d.notify(j, "Resumed", "Continued via "+res.Via)
}

// adoptPaneSession: a resume known only by its pane (its limit was read off
// the screen) belongs to the conversation the process in that pane holds, if
// the agent's own list says which. It becomes that conversation's resume, so
// it gets the same checks (an out-of-date copy is never typed into), or is
// dropped when it isn't that conversation's to resume. false: dropped.
func (d *Daemon) adoptPaneSession(j *core.Job) bool {
	if j.SessionID != "" || j.Location == nil {
		return true
	}
	ps := d.paneSession(j.Harness, j.Location)
	if ps.id == "" {
		return true
	}
	harness, id := ps.harness, ps.id
	if !ps.started.IsZero() && ps.started.After(j.LimitAt.Add(time.Minute)) {
		// Started after the limit hit: someone else's conversation now, not
		// the one that stalled (the pane check would refuse it anyway).
		d.skip(j, "the "+d.name(harness)+" in that pane started after the limit; it isn't the conversation that stalled")
		return false
	}
	key := store.SessionKey(harness, id)
	o := d.Jobs[key]
	if o == j {
		o = nil
	}
	if o != nil {
		recent := !o.UpdatedAt.Before(j.LimitAt.Add(-5 * time.Minute)) // about this stall
		switch {
		case o.State == core.StatePending,
			o.State == core.StateNeedsUser && recent,
			// Resumed after this pane's limit: that resume covered it.
			o.State == core.StateDelivered && o.DeliveredAt.After(j.LimitAt):
			d.skip(j, "the conversation in that pane ("+short(id)+") has its own resume")
			return false
		case (o.State == core.StateCancelled || o.State == core.StateIgnored) && recent:
			d.skip(j, "the conversation in that pane ("+short(id)+"): "+firstLine(o.Result))
			return false
		}
	}
	sess := d.Sessions[key]
	if sess != nil && sess.PID > 0 && sess.PID != ps.pid && !sess.Ended && d.holds(harness, id, sess.PID) {
		// The conversation is open in another process too, the one its
		// hooks last heard from: this pane holds a copy of it.
		d.skip(j, fmt.Sprintf("the conversation in that pane (%s) is open in pid %d, where it continued; this pane holds a copy of it", short(id), sess.PID))
		return false
	}
	if sess != nil {
		if h := d.Rules.Harness(harness); h.IgnoredEntrypoint(sess.Entrypoint) {
			d.skip(j, "the conversation in that pane is a print-mode or SDK run")
			return false
		}
		if j.Cwd == "" {
			j.Cwd = sess.Cwd
		}
		if j.TranscriptPath == "" {
			j.TranscriptPath = sess.TranscriptPath
		}
		if j.Entrypoint == "" {
			j.Entrypoint = sess.Entrypoint
		}
		if j.Model == "" {
			j.Model = sess.Model
		}
	}
	d.logf("%s: the pane holds %s conversation %s; resuming it as that conversation", j.Key, d.name(harness), short(id))
	from := j.Key
	delete(d.Jobs, j.Key)
	j.Key, j.SessionID, j.Harness = key, id, harness
	d.Jobs[key] = j
	d.dirty = true
	d.history("adopted", j, map[string]any{"from": from})
	if ok, why := d.Scope.Covers(j); !ok {
		markJobIgnored(j, why, d.Now())
		d.history("ignored", j, map[string]any{"reason": why})
		d.logf("%s: %s", j.Key, why)
		return false
	}
	if recentlyDelivered(o, j.LimitAt) {
		// Resumed shortly before and on a limit again: one more attempt of
		// the same resume (as a session's own re-limit is), so the attempt
		// limit holds, and the reset that was used up isn't trusted again:
		// the same notice (the limit hadn't lifted yet) is retried with
		// backoff. A screen merely not redrawn yet is dropped before then,
		// when the wrapper reports the pane active.
		j.Attempts = o.Attempts
		if d.stopAtMaxAttempts(j) {
			return false
		}
		if staleReset(o, j.ResetAt) {
			j.ResetAt, j.ResetKnown = time.Time{}, false
			d.reschedule(j)
			if j.State != core.StatePending || j.DueAt.After(d.Now()) {
				return false
			}
		}
	}
	return true
}

// paneProcess: the conversation held by the agent process at a location.
type paneProcess struct {
	harness, id string
	pid         int
	started     time.Time // zero when unknown
}

// paneSession: the conversation held by the agent process running at loc,
// from the agent's own list of open sessions and each process's environment
// (its wrapper or pane). Only agents whose hooks name their process
// (ExactPIDs); a "generic" resume (the wrapper didn't know the agent) tries
// each. Nothing when none is known, or more than one process matches.
func (d *Daemon) paneSession(harness string, loc *core.Location) paneProcess {
	if d.PaneSessionFn != nil {
		p := d.PaneSessionFn(loc)
		if p.id != "" && p.harness == "" {
			p.harness = harness
			if p.harness == "generic" {
				p.harness = "claude"
			}
		}
		return p
	}
	names := []string{harness}
	if harness == "generic" {
		names = d.Rules.HarnessNames()
	}
	for _, name := range names {
		h := d.Rules.Harness(name)
		if h == nil || h.LiveSessions == nil || !d.Config.HarnessEnabled(name) || !d.Compat.ExactPIDs(name) {
			continue
		}
		bin, _ := deliver.Binary(d.Config, h)
		list, _ := live.Default().List(h, live.Query{Bin: bin, Homes: d.homes()})
		p := matchPane(h, list, loc, func(pid int) *core.Location {
			return deliver.LocationFromEnv(d.Paths, d.Rules, proc.Environ(pid))
		})
		if p.id != "" {
			p.harness = name
			if p.started.IsZero() {
				p.started, _ = proc.StartTime(p.pid)
			}
			return p
		}
	}
	return paneProcess{}
}

// matchPane picks the one interactive session in list whose process runs at
// loc. A print-mode or SDK run started from it inherits its pane, so those
// don't count; two candidates means it can't be told.
func matchPane(h *rules.Harness, list []live.Session, loc *core.Location, at func(pid int) *core.Location) paneProcess {
	var found paneProcess
	n := 0
	for _, s := range list {
		if s.SessionID == "" || s.PID <= 0 || h.IgnoredEntrypoint(s.Entrypoint) ||
			h.LiveSessions != nil && containsStr(h.LiveSessions.BackgroundKinds, s.Kind) {
			continue
		}
		if l := at(s.PID); l != nil && l.String() == loc.String() {
			found = paneProcess{id: s.SessionID, pid: s.PID, started: s.StartedAt}
			n++
		}
	}
	if n != 1 {
		return paneProcess{}
	}
	return found
}

// staleCopy returns the pid of an out-of-date copy that is the only process
// holding j's session: the agent lists the session in another process than
// the one its hooks last heard from, and that one has exited. The copy's
// memory of the conversation stops where that second process took over, so
// typing into it would fork the conversation. Only for agents whose hooks
// report their exact process (ExactPIDs).
//
// The listed process is not the one the hooks last heard from (which has
// quit, or now holds another conversation), and hasn't done anything since.
func (d *Daemon) staleCopy(j *core.Job, sess *store.Session, st *live.State) int {
	if !d.Compat.ExactPIDs(j.Harness) || sess == nil || sess.PID <= 0 || st == nil || !st.Open || !st.Authoritative || !st.Listed || st.Session == nil {
		return 0
	}
	open := st.Session.PID
	if open <= 0 || open == sess.PID {
		return 0
	}
	// The recorded process isn't listed holding the session (the agent's
	// list would have picked it): it has quit or moved on to another
	// conversation, whether or not it still runs.
	if h := d.Rules.Harness(j.Harness); h.LiveSessions != nil && live.ActiveSince(st.Session, h.LiveSessions.BusyStates, sess.LastSeen) {
		// It has done something since the recorded process last reported:
		// it is the copy in use (hooks don't report a turn's start).
		return 0
	}
	return open
}

// copyEnded: ev reports that a process quit which isn't the one the session's
// record names, while that one still runs: a second copy of the conversation
// closed, not the session.
// The agent's own list says so: a process name can be reused or look like
// any Node program. Only for agents whose hooks name the process (ExactPIDs).
func (d *Daemon) copyEnded(ev core.Event) bool {
	if ev.Kind != core.KindSessionEnd || !d.Compat.ExactPIDs(ev.Harness) || ev.SessionID == "" || ev.PID <= 0 {
		return false
	}
	s := d.Sessions[store.SessionKey(ev.Harness, ev.SessionID)]
	if s == nil || s.Ended || s.PID <= 0 || ev.PID == s.PID {
		return false
	}
	return d.holds(ev.Harness, ev.SessionID, s.PID)
}

// holds: the agent lists pid as holding session id right now.
func (d *Daemon) holds(harness, id string, pid int) bool {
	if d.HoldsFn != nil {
		return d.HoldsFn(harness, id, pid)
	}
	if d.LiveFn == nil {
		return false
	}
	st := d.LiveFn(&core.Job{Harness: harness, SessionID: id, PID: pid})
	return st != nil && st.Checked && st.Authoritative && st.Listed && st.Session != nil && st.Session.PID == pid
}

// waitingSince: when the current wait began (older jobs: their limit).
func waitingSince(j *core.Job) time.Time {
	if !j.WaitingSince.IsZero() {
		return j.WaitingSince
	}
	if j.ResetAt.After(j.LimitAt) {
		return j.ResetAt
	}
	return j.LimitAt
}

// resumesClosed: the agent has its own command to continue a closed session,
// headless continuing is allowed, and closed sessions are tracked.
func (d *Daemon) resumesClosed(harness string) bool {
	if !d.Config.HeadlessAllowed() || !d.Config.ClosedSessionsTracked() {
		return false
	}
	for _, nc := range d.Rules.Harness(harness).NativeCommands {
		if nc.UsedWhen() == "stopped" {
			return true
		}
	}
	return false
}

// waitForStaleCopy holds a resume while only an out-of-date copy is open: it
// continues (through the agent's own command) once that copy closes, or the
// copy becomes the current process if the user types in it.
func (d *Daemon) waitForStaleCopy(j *core.Job, pid int) {
	now := d.Now()
	reason := fmt.Sprintf("only an out-of-date copy of this conversation is open (pid %d)", pid)
	if markJobWaitingStaleCopy(j, pid, reason, now) {
		// A new wait (or a different reason than the last one): say so.
		d.history("waiting_open", j, map[string]any{"error": j.Result})
		then := "Close it and Continuum continues the conversation by itself"
		if !d.resumesClosed(j.Harness) {
			then = "Close it and continue the conversation yourself (Continuum can't continue it closed here)"
		}
		d.notify(j, "Limit reset", fmt.Sprintf("Only an out-of-date copy of this conversation is open (pid %d), so Continuum won't type into it. %s, or type in the copy to carry on from there.", pid, then))
	}
	if now.Sub(waitingSince(j)) > 24*time.Hour {
		markJobNeedsUser(j, "still only an out-of-date copy open after 24 hours: "+j.Result, false, now)
		d.history("needs_user", j, nil)
		d.notify(j, "Resume needs you", "Only an out-of-date copy of this conversation has been open for 24 hours. Continue it yourself.")
		return
	}
	j.DueAt = now.Add(2 * time.Minute)
	d.logf("%s: %s; not typing into it, checking again at %s", j.Key, j.Result, j.DueAt.Format("15:04"))
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

// liveState asks the agent (or the process table) whether the job's session
// is open right now.
func (d *Daemon) liveState(j *core.Job) *live.State {
	if DriverFor(j.Harness).ManagesOwnLifecycle(j) {
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

// isAgentPID: the job's recorded process still runs and is the agent (a pid
// can be reused by another program once the agent exits).
func (d *Daemon) isAgentPID(j *core.Job) bool {
	_, name, ok := proc.Info(j.PID)
	return ok && d.Rules.Harness(j.Harness).IsHarnessProcess(name)
}
