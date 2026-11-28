package main

// The session table shown by `continuum status` and `continuum sessions`:
// every open or recent session, what it is doing, where it runs, and what
// Continuum will do with it.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/daemon"
	"github.com/continuum-dev/continuum/internal/messages"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/scope"
	"github.com/continuum-dev/continuum/internal/store"
)

type sessionRow struct {
	Agent    string    `json:"agent"`
	ID       string    `json:"session_id"`
	Name     string    `json:"name,omitempty"`
	Folder   string    `json:"folder,omitempty"`
	State    string    `json:"state"`  // short code: working, idle, waiting, limit, outage, resumed, needs_you, not_resuming, active, closed, not_running, copy, stale_copy (see `continuum status -h`)
	Detail   string    `json:"detail"` // human text for the STATE column
	Where    string    `json:"where"`
	Open     bool      `json:"open"`
	Covered  bool      `json:"covered"`
	Wrapper  string    `json:"wrapper,omitempty"` // Continuum's wrapper it runs in
	Message  string    `json:"message_from"`
	LastSeen time.Time `json:"last_seen,omitempty"`
	PID      int       `json:"pid,omitempty"`
	// Copy: another process holding the same conversation (same session_id),
	// which Continuum doesn't resume.
	Copy bool `json:"copy,omitempty"`
}

// sessionRows lists open sessions, sessions seen within window, and any with
// a resume pending or needing the user.
func sessionRows(window time.Duration) []sessionRow {
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	r, _ := rules.Cached(p.Rules)
	sc, _ := scope.Load(p)
	msgs, _ := messages.Load(p)
	jobs, _ := store.LoadJobs(p.Jobs)
	now := time.Now()
	var rows []sessionRow
	snap := daemon.LoadOrBuildSnapshot(p, cfg, r, daemonRunning(p))
	known := knownSessionsFromSnapshot(snap)
	for _, s := range known {
		j := jobs[s.Harness+":"+s.ID]
		pendingish := cfg.ClosedSessionsTracked() && j != nil && (j.State == core.StatePending || j.State == core.StateNeedsUser)
		if !s.Open && !pendingish && (window <= 0 || now.Sub(s.LastSeen) > window) {
			continue
		}
		s.ResumesClosed = resumesClosed(r, cfg, s.Harness)
		probe := &core.Job{Harness: s.Harness, SessionID: s.ID, Cwd: s.Cwd, Location: s.Location}
		covered, _ := sc.Covers(probe)
		row := sessionRow{
			Agent: s.Harness, ID: s.ID, Name: s.Name, Folder: s.Cwd, Open: s.Open, Covered: covered,
			Message: msgs.Resolve(probe, false, cfg, r).Source, LastSeen: s.LastSeen, PID: s.PID,
		}
		if r != nil && r.Harness(s.Harness) != nil {
			row.Agent = r.Harness(s.Harness).DisplayName
		}
		row.State, row.Detail = sessionState(s, j, covered, now)
		row.Where = sessionWhere(s, r)
		if s.Location != nil && s.Location.Kind == "wrapper" {
			row.Wrapper = s.Location.Target
		}
		rows = append(rows, row)
		for _, c := range s.Copies {
			cr := row
			cr.Name, cr.Open, cr.LastSeen, cr.Wrapper, cr.PID, cr.Copy = c.Name, c.Open, c.LastSeen, "", c.PID, true
			cr.State, cr.Detail = "copy", "another copy of this conversation · Continuum doesn't resume this one"
			cr.Where = sessionWhere(c, r)
			if c.Location != nil && c.Location.Kind == "wrapper" {
				cr.Wrapper = c.Location.Target
			}
			rows = append(rows, cr)
		}
	}
	return append(rows, wrapperRows(snap.Wrappers, cfg, r, sc, msgs, jobs, rows, now)...)
}

// wrapperRows lists agents running in Continuum's wrapper that no listed
// session already covers (agents without one: Gemini, or sessions idle past
// the knownSessions window).
func wrapperRows(wrappers []daemon.SnapshotWrapper, cfg core.Config, r *rules.Rules, sc scope.Scope, msgs *messages.Messages, jobs map[string]*core.Job, listed []sessionRow, now time.Time) []sessionRow {
	seen := map[string]bool{}
	for _, row := range listed {
		if row.Wrapper != "" {
			seen[row.Wrapper] = true
		}
	}
	var rows []sessionRow
	for _, w := range wrappers {
		if seen[w.ID] {
			continue
		}
		loc := &core.Location{Kind: "wrapper", Target: w.ID}
		probe := &core.Job{Harness: w.Harness, Cwd: w.Cwd, Location: loc}
		covered, _ := sc.Covers(probe)
		row := sessionRow{Agent: w.Harness, Folder: w.Cwd, Open: true, Covered: covered,
			Message: msgs.Resolve(probe, false, cfg, r).Source, LastSeen: w.Started, Wrapper: w.ID,
			State: "active", Detail: "open (in Continuum's wrapper)"}
		if h := r.Harness(w.Harness); h != nil && h.DisplayName != "" {
			row.Agent = h.DisplayName
		}
		if j := jobs[w.Harness+"@"+loc.String()]; j != nil {
			if st, detail := sessionState(knownSession{Open: true}, j, covered, now); st != "active" {
				row.State, row.Detail = st, detail
			}
		}
		row.Where = sessionWhere(knownSession{Open: true, Location: loc, Host: w.Host}, r)
		rows = append(rows, row)
	}
	return rows
}

// resumesClosed: the agent has its own command to continue a closed session,
// headless continuing is allowed, and closed sessions are tracked.
func resumesClosed(r *rules.Rules, cfg core.Config, harness string) bool {
	if r == nil || !cfg.HeadlessAllowed() || !cfg.ClosedSessionsTracked() {
		return false
	}
	h := r.Harness(harness)
	if h == nil {
		return false
	}
	for _, nc := range h.NativeCommands {
		if nc.UsedWhen() == "stopped" {
			return true
		}
	}
	return false
}

// hostFromEnv forwards to daemon.HostFromEnv so the CLI and daemon resolve
// terminal hosts identically.
func hostFromEnv(r *rules.Rules, env map[string]string) string {
	return daemon.HostFromEnv(r, env)
}

// sessionState says what a session is doing, most important first: what
// Continuum is about to do, then what the agent reports, then recency.
func sessionState(s knownSession, j *core.Job, covered bool, now time.Time) (string, string) {
	phase := daemon.JobPhaseOf(j)
	if s.StaleCopy || phase == daemon.PhaseWaitingStaleCopy {
		// Only an out-of-date copy is open: Continuum won't type into it.
		closeIt := "close it and Continuum continues the conversation"
		if !s.ResumesClosed {
			closeIt = "close it and continue the conversation yourself"
		}
		switch phase {
		case daemon.PhaseWaitingStaleCopy:
			return "stale_copy", "limit reset · only an out-of-date copy is open, not typing into it · " + closeIt
		case daemon.PhaseScheduled, daemon.PhaseWaitingOpen:
			if s.ResumesClosed {
				closeIt += " after " + whenShort(j.DueAt, now)
			}
			return "stale_copy", "usage limit · only an out-of-date copy is open, won't type into it · " + closeIt
		case daemon.PhaseNeedsUser:
			return "needs_you", "needs you: " + clip(firstLineOf(j.Result), 60)
		}
		return "stale_copy", "out-of-date copy of this conversation (the process that continued it has closed or moved on)"
	}
	switch phase {
	case daemon.PhaseScheduled:
		if j.Kind == core.KindTransient {
			return "outage", "API error · retrying " + whenShort(j.DueAt, now)
		}
		if !s.Open && s.Checked && !(s.Live && s.Kind == "background") {
			// Checked and closed: no pane to type into.
			if s.ResumesClosed {
				return "limit", "closed · usage limit · continues it in the background " + whenShort(j.DueAt, now)
			}
			return "limit", "closed · usage limit · reopen it to continue"
		}
		return "limit", "usage limit · resumes " + whenShort(j.DueAt, now)
	case daemon.PhaseWaitingOpen:
		if j.Kind == core.KindTransient {
			return "outage", "API error · retrying " + whenShort(j.DueAt, now)
		}
		return "limit", "limit reset · waiting until Continuum can type into it"
	case daemon.PhaseNeedsUser:
		return "needs_you", "needs you: " + clip(firstLineOf(j.Result), 60)
	case daemon.PhaseIgnored:
		if now.Sub(j.UpdatedAt) < 24*time.Hour && !(s.Live && strings.EqualFold(s.Status, "busy")) {
			return "not_resuming", "usage limit · not resuming (" + map[bool]string{true: "excluded", false: "not chosen"}[strings.HasPrefix(j.Result, "excluded")] + ")"
		}
	case daemon.PhaseTurnFailed:
		if now.Sub(j.DeliveredAt) < 24*time.Hour && !(s.Live && strings.EqualFold(s.Status, "busy")) {
			return "needs_you", "resumed " + j.DeliveredAt.Local().Format("15:04") + ", but its turn failed: " + clip(j.TurnFailed, 50)
		}
	case daemon.PhaseDelivered:
		if now.Sub(j.DeliveredAt) < 2*time.Hour && !(s.Live && strings.EqualFold(s.Status, "busy")) {
			return "resumed", "resumed " + j.DeliveredAt.Local().Format("15:04") + " by Continuum"
		}
	case daemon.PhaseFailed:
		if now.Sub(j.UpdatedAt) < 24*time.Hour {
			return "needs_you", "couldn't resume: " + clip(firstLineOf(j.Result), 50)
		}
	}
	if s.Live {
		switch {
		case s.Kind == "background" && s.PID <= 0:
			return "not_running", "background session, not running"
		case s.WaitingFor != "":
			return "waiting", "waiting for you: " + s.WaitingFor
		case strings.EqualFold(s.Status, "busy"), strings.EqualFold(s.Status, "working"), strings.EqualFold(s.Status, "running"):
			return "working", "working"
		case strings.EqualFold(s.Status, "waiting"):
			return "waiting", "waiting for you"
		case strings.EqualFold(s.Status, "shell"):
			return "working", "working (running a command)"
		case strings.EqualFold(s.Status, "idle"):
			return "idle", "idle, waiting for your next message"
		case s.Status != "":
			return "active", s.Status
		}
		return "active", "open"
	}
	if !s.Open {
		return "closed", "closed · last active " + ago(s.LastSeen)
	}
	return "active", "open · last active " + ago(s.LastSeen)
}

func whenShort(t, now time.Time) string {
	d := t.Sub(now)
	if d <= 0 {
		return "now"
	}
	in := strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
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

// sessionWhere says where a session runs, in words.
func sessionWhere(s knownSession, r *rules.Rules) string {
	if r != nil {
		if h := r.Harness(s.Harness); h != nil {
			for _, a := range h.DesktopApps {
				if a.IsEntrypoint(s.Entrypoint) {
					return a.Name
				}
			}
		}
	}
	if s.Location != nil && s.Location.Kind != "" {
		switch s.Location.Kind {
		case "wrapper":
			host := s.Host
			if host == "" {
				host = "terminal"
			}
			return host + " (via Continuum)"
		case "tmux":
			return "tmux " + s.Location.Target
		}
		if name, ok := muxNames[s.Location.Kind]; ok {
			return name
		}
		return s.Location.Kind
	}
	if s.Tmux != "" {
		return "tmux"
	}
	if s.Kind == "background" {
		return "background"
	}
	if s.Open && s.Host != "" {
		return s.Host
	}
	if s.Open {
		return "terminal"
	}
	return "-"
}

func printSessionTable(w io.Writer, rows []sessionRow, withMessage bool) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	head := "AGENT\tSESSION\tFOLDER\tSTATE\tWHERE\tCOVERED"
	if withMessage {
		head += "\tMESSAGE"
	}
	fmt.Fprintln(tw, head)
	for _, r := range rows {
		name := r.Name
		if name == "" {
			name = shortID(r.ID)
		}
		if name == "" && r.Wrapper != "" {
			name = shortID(r.Wrapper)
		}
		folder := filepath.Base(r.Folder)
		if r.Folder == "" {
			folder = "-"
		}
		line := fmt.Sprintf("%s\t%s\t%s\t%s\t%s\t%s", r.Agent, name, folder, r.Detail, r.Where, map[bool]string{true: "yes", false: "no"}[r.Covered])
		if withMessage {
			line += "\t" + r.Message
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()
}

func printRowsJSON(rows []sessionRow) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if rows == nil {
		rows = []sessionRow{}
	}
	return enc.Encode(rows)
}

var muxNames = map[string]string{"screen": "GNU screen", "zellij": "zellij", "herdr": "herdr", "rex": "Rex", "wezterm": "WezTerm", "kitty": "kitty", "ghostty": "Ghostty"}
