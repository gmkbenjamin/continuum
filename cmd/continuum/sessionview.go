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
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/messages"
	"github.com/continuum-dev/continuum/internal/mux"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/scope"
	"github.com/continuum-dev/continuum/internal/store"
)

type sessionRow struct {
	Agent    string    `json:"agent"`
	ID       string    `json:"session_id"`
	Name     string    `json:"name,omitempty"`
	Folder   string    `json:"folder,omitempty"`
	State    string    `json:"state"`  // short code: working, idle, waiting, limit, outage, resumed, needs_you, not_resuming, active, closed, not_running
	Detail   string    `json:"detail"` // human text for the STATE column
	Where    string    `json:"where"`
	Open     bool      `json:"open"`
	Covered  bool      `json:"covered"`
	Wrapper  string    `json:"wrapper,omitempty"` // Continuum's wrapper it runs in
	Message  string    `json:"message_from"`
	LastSeen time.Time `json:"last_seen,omitempty"`
}

// sessionRows lists open sessions, sessions seen within window, and any with
// a resume pending or needing the user.
func sessionRows(window time.Duration, includeClosed bool) []sessionRow {
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	r, _ := rules.Cached(p.Rules)
	sc, _ := scope.Load(p)
	msgs, _ := messages.Load(p)
	jobs, _ := store.LoadJobs(p.Jobs)
	now := time.Now()
	var rows []sessionRow
	known := knownSessions()
	for _, s := range known {
		j := jobs[s.Harness+":"+s.ID]
		pendingish := j != nil && (j.State == core.StatePending || j.State == core.StateNeedsUser)
		if !s.Open && !pendingish && (now.Sub(s.LastSeen) > window || !includeClosed && !s.Open) {
			continue
		}
		probe := &core.Job{Harness: s.Harness, SessionID: s.ID, Cwd: s.Cwd, Location: s.Location}
		covered, _ := sc.Covers(probe)
		row := sessionRow{
			Agent: s.Harness, ID: s.ID, Name: s.Name, Folder: s.Cwd, Open: s.Open, Covered: covered,
			Message: msgs.Resolve(probe, false, cfg, r).Source, LastSeen: s.LastSeen,
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
	}
	return append(rows, wrapperRows(p, r, sc, jobs, known, now)...)
}

// wrapperRows lists agents running in Continuum's wrapper that no session
// list knows of (agents without one: Antigravity, Cursor, Gemini, ...).
func wrapperRows(p core.Paths, r *rules.Rules, sc scope.Scope, jobs map[string]*core.Job, known []knownSession, now time.Time) []sessionRow {
	seen := map[string]bool{}
	for _, s := range known {
		if s.Location != nil && s.Location.Kind == "wrapper" {
			seen[s.Location.Target] = true
		}
	}
	entries, _ := os.ReadDir(p.Wrappers)
	var rows []sessionRow
	for _, e := range entries {
		id := e.Name()
		if !e.IsDir() || seen[id] || !deliver.WrapperAlive(p, id) {
			continue
		}
		var meta struct {
			Harness string    `json:"harness"`
			Cwd     string    `json:"cwd"`
			Started time.Time `json:"started"`
		}
		data, _ := os.ReadFile(filepath.Join(p.Wrappers, id, "meta.json"))
		if json.Unmarshal(data, &meta) != nil || meta.Harness == "" {
			continue
		}
		loc := &core.Location{Kind: "wrapper", Target: id}
		probe := &core.Job{Harness: meta.Harness, Cwd: meta.Cwd, Location: loc}
		covered, _ := sc.Covers(probe)
		row := sessionRow{Agent: meta.Harness, Folder: meta.Cwd, Open: true, Covered: covered, LastSeen: meta.Started, Wrapper: id,
			State: "active", Detail: "open (in Continuum's wrapper)"}
		if h := r.Harness(meta.Harness); h != nil && h.DisplayName != "" {
			row.Agent = h.DisplayName
		}
		if j := jobs[meta.Harness+"@"+loc.String()]; j != nil {
			if st, detail := sessionState(knownSession{Open: true}, j, covered, now); st != "active" {
				row.State, row.Detail = st, detail
			}
		}
		pid, _ := strconv.Atoi(strings.TrimSpace(readSmall(filepath.Join(p.Wrappers, id, "pid"))))
		row.Where = sessionWhere(knownSession{Open: true, Location: loc, Host: hostFromEnv(r, proc.Environ(pid))}, r)
		rows = append(rows, row)
	}
	return rows
}

func readSmall(path string) string {
	data, _ := os.ReadFile(path)
	return string(data)
}

// terminalApps names terminal apps from TERM_PROGRAM.
var terminalApps = map[string]string{
	"apple_terminal": "Terminal", "iterm.app": "iTerm2", "ghostty": "Ghostty", "wezterm": "WezTerm",
	"vscode": "VS Code", "warpterminal": "Warp", "hyper": "Hyper", "tabby": "Tabby", "zed": "Zed",
	"rio": "Rio", "kitty": "kitty", "alacritty": "Alacritty", "tmux": "tmux", "rex": "Rex",
}

// hostFromEnv names where a process runs from its environment: the
// multiplexer pane it is in, else the terminal app.
func hostFromEnv(r *rules.Rules, env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	if r != nil {
		if loc := mux.LocateFromEnv(r, env); loc != nil {
			if name, ok := muxNames[loc.Kind]; ok {
				return name
			}
			return loc.Kind
		}
	}
	switch {
	case env["CURSOR_TRACE_ID"] != "" && strings.EqualFold(env["TERM_PROGRAM"], "vscode"):
		return "Cursor"
	case env["KITTY_WINDOW_ID"] != "":
		return "kitty"
	case env["ALACRITTY_WINDOW_ID"] != "":
		return "Alacritty"
	}
	if name, ok := terminalApps[strings.ToLower(env["TERM_PROGRAM"])]; ok {
		return name
	}
	return ""
}

// sessionState says what a session is doing, most important first: what
// Continuum is about to do, then what the agent reports, then recency.
func sessionState(s knownSession, j *core.Job, covered bool, now time.Time) (string, string) {
	if j != nil {
		switch j.State {
		case core.StatePending:
			if j.Kind == core.KindTransient {
				return "outage", "API error · retrying " + whenShort(j.DueAt, now)
			}
			if j.WaitingOpen {
				return "limit", "limit reset · waiting until Continuum can type into it"
			}
			return "limit", "usage limit · resumes " + whenShort(j.DueAt, now)
		case core.StateNeedsUser:
			return "needs_you", "needs you: " + clip(firstLineOf(j.Result), 60)
		case core.StateIgnored:
			if now.Sub(j.UpdatedAt) < 24*time.Hour && !(s.Live && strings.EqualFold(s.Status, "busy")) {
				return "not_resuming", "usage limit · not resuming (" + map[bool]string{true: "excluded", false: "not chosen"}[strings.HasPrefix(j.Result, "excluded")] + ")"
			}
		case core.StateDelivered:
			if now.Sub(j.DeliveredAt) < 2*time.Hour && !(s.Live && strings.EqualFold(s.Status, "busy")) {
				return "resumed", "resumed " + j.DeliveredAt.Local().Format("15:04") + " by Continuum"
			}
		case core.StateFailed:
			if now.Sub(j.UpdatedAt) < 24*time.Hour {
				return "needs_you", "couldn't resume: " + clip(firstLineOf(j.Result), 50)
			}
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
