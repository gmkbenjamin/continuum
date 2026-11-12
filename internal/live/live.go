// Package live finds out whether an agent session is still open right now,
// where, and whether it is busy. Continuum uses this to keep its promise of
// never starting a second copy of a session: commands that would launch the
// agent again are only used once the original is no longer open.
//
// Sources, all described in the rules so an agent update is a data change:
//   - a command that lists open sessions as JSON (claude agents --json),
//   - a registry directory with one JSON file per running process,
//   - a process scan (agents without either, such as Codex).
package live

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
)

// Session is one open session.
type Session struct {
	SessionID  string `json:"session_id"`
	ShortID    string `json:"short_id,omitempty"`
	PID        int    `json:"pid,omitempty"`
	Cwd        string `json:"cwd,omitempty"`
	Kind       string `json:"kind,omitempty"`   // interactive, background, ...
	Status     string `json:"status,omitempty"` // busy, idle, blocked, ...
	Tmux       string `json:"tmux,omitempty"`
	Entrypoint string `json:"entrypoint,omitempty"`
	Name       string `json:"name,omitempty"`
	WaitingFor string `json:"waiting_for,omitempty"` // e.g. "dialog open"
	Version    string `json:"version,omitempty"`
	// StatusSince is when Status last changed, when the source says.
	StatusSince time.Time `json:"status_since,omitempty"`
}

// State is what is known about one session.
type State struct {
	// Checked: at least one source answered, so Open is a real answer.
	Checked bool `json:"checked"`
	// Open: the session runs somewhere right now.
	Open bool `json:"open"`
	// Background: it runs in the agent's own background service.
	Background bool `json:"background,omitempty"`
	// Busy: the agent reports it is working.
	Busy bool `json:"busy,omitempty"`
	// Dormant: a background session the agent knows of but whose process is
	// not running. Continuing it under its own id does not start a copy, and
	// attaching to it would restart it behind a confirmation menu.
	Dormant bool `json:"dormant,omitempty"`
	// MovedOn: the process the session ran in now holds a different session
	// (the user cleared or switched conversations).
	MovedOn bool     `json:"moved_on,omitempty"`
	Session *Session `json:"session,omitempty"`
	Source  string   `json:"source,omitempty"`
	// DesktopApp names the desktop app that holds (or may hold) the session.
	DesktopApp string `json:"desktop_app,omitempty"`
	// OnlyDesktop: open only in the sense that the desktop app may hold it;
	// no process of the agent is known to.
	OnlyDesktop bool `json:"only_desktop,omitempty"`
	// Authoritative: the answer comes from the agent's own list of open
	// sessions, so "not open" really means closed.
	Authoritative bool `json:"authoritative,omitempty"`
}

// Query identifies the session to look up.
type Query struct {
	SessionID string
	Cwd       string
	PID       int    // pid recorded by a hook, if any
	Bin       string // the agent's executable
	Homes     map[string]string
	// Entrypoint is where the session was started (cli, claude-desktop, ...).
	Entrypoint string
	// TrustRegistry lists desktop apps whose sessions the agent's own session
	// list is known to include (learned for the app's current version). For
	// those, "not listed" means closed; for others, a running app may hold it.
	TrustRegistry map[string]bool
}

// Finder looks sessions up. Its functions can be replaced in tests.
type Finder struct {
	Run     func(timeout time.Duration, argv ...string) (string, error)
	Procs   func() ([]proc.Process, error)
	Cwd     func(pid int) string
	Alive   func(pid int) bool
	HelpOK  func(bin string, tokens []string) bool
	Timeout time.Duration
}

// Default uses the real system.
func Default() *Finder {
	return &Finder{
		Run:   func(t time.Duration, argv ...string) (string, error) { return proc.Run(t, "", argv...) },
		Procs: proc.List,
		Cwd:   proc.Cwd,
		Alive: proc.Alive,
		HelpOK: func(bin string, tokens []string) bool {
			out, _ := proc.Run(20*time.Second, "", bin, "--help")
			for _, t := range tokens {
				if !strings.Contains(out, t) {
					return false
				}
			}
			return true
		},
		Timeout: 20 * time.Second,
	}
}

// List returns the open sessions the command and registry sources know of
// (process scans only answer for a given session, so they are not listed).
func (f *Finder) List(h *rules.Harness, q Query) ([]Session, bool) {
	ls := h.LiveSessions
	if ls == nil {
		return nil, false
	}
	var all []Session
	ok := false
	if len(ls.Command) > 0 && q.Bin != "" {
		if list, good := f.fromCommand(ls, q); good {
			all, ok = list, true
		}
	}
	if ls.RegistryDir != "" {
		if list, good := f.fromRegistry(expand(ls.RegistryDir, q)); good {
			all, ok = mergeSessions(all, list), true
		}
	}
	if ls.RegistryFile != "" {
		if list, good := f.fromRegistryFile(expand(ls.RegistryFile, q)); good {
			all, ok = mergeSessions(all, list), true
		}
	}
	return all, ok
}

// Lookup reports on q's session for harness h.
func (f *Finder) Lookup(h *rules.Harness, q Query) State {
	st := State{}
	ls := h.LiveSessions
	var all []Session
	if ls != nil {
		if len(ls.Command) > 0 && q.Bin != "" && (len(ls.RequiresHelp) == 0 || f.HelpOK == nil || f.HelpOK(q.Bin, ls.RequiresHelp)) {
			if list, ok := f.fromCommand(ls, q); ok {
				st.Checked, st.Source = true, "command"
				all = append(all, list...)
			}
		}
		if ls.RegistryDir != "" {
			if list, ok := f.fromRegistry(expand(ls.RegistryDir, q)); ok {
				if !st.Checked {
					st.Source = "registry"
				}
				st.Checked = true
				all = mergeSessions(all, list)
			}
		}
		if ls.RegistryFile != "" {
			if list, ok := f.fromRegistryFile(expand(ls.RegistryFile, q)); ok {
				if !st.Checked {
					st.Source = "registry"
				}
				st.Checked = true
				all = mergeSessions(all, list)
			}
		}
		if ls.ScanProcesses && q.SessionID != "" {
			if s, ok, checked := f.fromProcesses(h, q); checked {
				if !st.Checked {
					st.Source = "processes"
				}
				st.Checked = true
				if ok {
					all = append(all, s)
				}
			}
		}
	}
	for i := range all {
		s := &all[i]
		if q.SessionID != "" && s.SessionID == q.SessionID {
			st.Open = true
			st.Session = s
			break
		}
	}
	if st.Session == nil && q.PID > 0 {
		// The recorded process now holds another session: the user moved on.
		for i := range all {
			if all[i].PID == q.PID && all[i].SessionID != "" && all[i].SessionID != q.SessionID {
				st.MovedOn = true
			}
		}
		if !st.MovedOn && f.Alive != nil && f.Alive(q.PID) {
			// No registry entry but the agent's process is alive: still open.
			st.Open = true
			st.Session = &Session{SessionID: q.SessionID, PID: q.PID, Cwd: q.Cwd}
			if !st.Checked {
				st.Checked, st.Source = true, "pid"
			}
		} else if !st.Checked && f.Alive != nil {
			st.Checked, st.Source = true, "pid"
		}
	}
	f.applyDesktop(h, q, &st)
	st.Authoritative = st.Source == "command" || st.Source == "registry"
	if st.Session != nil && ls != nil {
		st.Busy = contains(ls.BusyStates, st.Session.Status)
		st.Background = contains(ls.BackgroundKinds, st.Session.Kind)
		if st.Background && st.Session.PID <= 0 {
			st.Open, st.Dormant, st.Busy = false, true, false
		}
	}
	return st
}

func (f *Finder) fromCommand(ls *rules.LiveSessions, q Query) ([]Session, bool) {
	argv := make([]string, len(ls.Command))
	for i, a := range ls.Command {
		argv[i] = expand(a, q)
	}
	out, err := f.Run(f.Timeout, argv...)
	if err != nil {
		return nil, false
	}
	out = strings.TrimSpace(out)
	if i := strings.IndexByte(out, '['); i > 0 {
		out = out[i:] // tolerate a banner line before the JSON
	}
	var raw []map[string]any
	if json.Unmarshal([]byte(out), &raw) != nil {
		return nil, false
	}
	list := make([]Session, 0, len(raw))
	for _, m := range raw {
		list = append(list, fromMap(m))
	}
	return list, true
}

func (f *Finder) fromRegistry(dir string) ([]Session, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	var list []Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		s := fromMap(m)
		if s.PID <= 0 || (f.Alive != nil && !f.Alive(s.PID)) {
			continue // a stale file left by a crashed process
		}
		list = append(list, s)
	}
	return list, true
}

// fromRegistryFile reads a JSON array of open sessions. A missing file means
// no session is open; an unreadable one means nothing is known.
func (f *Finder) fromRegistryFile(path string) ([]Session, bool) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, true
	}
	if err != nil {
		return nil, false
	}
	var rows []map[string]any
	if json.Unmarshal(data, &rows) != nil {
		var wrapped struct {
			Sessions []map[string]any `json:"sessions"`
		}
		if json.Unmarshal(data, &wrapped) != nil {
			return nil, false
		}
		rows = wrapped.Sessions
	}
	var list []Session
	for _, m := range rows {
		s := fromMap(m)
		if s.SessionID == "" || s.PID <= 0 || (f.Alive != nil && !f.Alive(s.PID)) {
			continue // a stale row left by a crashed process
		}
		if s.Kind == "" {
			s.Kind = "interactive"
		}
		list = append(list, s)
	}
	return list, true
}

// fromProcesses finds an agent process that holds the session: one whose
// command line names the session id, or else one running in the session's
// directory (it may hold the session; Continuum then assumes it does).
func (f *Finder) fromProcesses(h *rules.Harness, q Query) (Session, bool, bool) {
	if f.Procs == nil {
		return Session{}, false, false
	}
	ps, err := f.Procs()
	if err != nil {
		return Session{}, false, false
	}
	self := os.Getpid()
	var inDir []proc.Process
	for _, p := range ps {
		if p.PID == self || !isAgentProcess(h, p) {
			continue
		}
		if q.SessionID != "" && strings.Contains(p.Line, q.SessionID) {
			return Session{SessionID: q.SessionID, PID: p.PID, Cwd: q.Cwd, Kind: "interactive"}, true, true
		}
		inDir = append(inDir, p)
	}
	if q.Cwd != "" && f.Cwd != nil {
		want := filepath.Clean(q.Cwd)
		unknown := 0
		for _, p := range inDir {
			c := f.Cwd(p.PID)
			if c != "" && filepath.Clean(c) == want {
				return Session{SessionID: q.SessionID, PID: p.PID, Cwd: c, Kind: "interactive"}, true, true
			}
			if c == "" && unknown == 0 && runtime.GOOS == "windows" {
				unknown = p.PID
			}
		}
		if unknown > 0 {
			// Windows doesn't say which folder a process runs in: it may hold
			// the session, so it counts as open. Never a copy.
			return Session{SessionID: q.SessionID, PID: unknown, Cwd: q.Cwd, Kind: "interactive"}, true, true
		}
	} else if len(inDir) > 0 && f.Cwd == nil {
		return Session{SessionID: q.SessionID, PID: inDir[0].PID, Cwd: q.Cwd, Kind: "interactive"}, true, true
	}
	return Session{}, false, true
}

func isAgentProcess(h *rules.Harness, p proc.Process) bool {
	if len(p.Args) == 0 {
		return false
	}
	named := false
	for i, a := range p.Args {
		if i > 1 {
			break // the program, or the script an interpreter runs
		}
		base := filepath.Base(a)
		base = strings.TrimSuffix(strings.TrimSuffix(base, ".exe"), ".js")
		if h.IsHarnessProcess(base) {
			named = true
		}
	}
	if !named && !h.MatchesCommandLine(p.Line) {
		return false
	}
	for _, a := range p.Args[1:] {
		for _, x := range h.LiveSessions.ProcessExcludeArgs {
			if a == x || filepath.Base(a) == x {
				return false
			}
		}
	}
	for _, x := range h.LiveSessions.ProcessExcludeArgs {
		if filepath.Base(p.Args[0]) == x {
			return false
		}
	}
	return true
}

func fromMap(m map[string]any) Session {
	str := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := m[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	num := func(keys ...string) int {
		for _, k := range keys {
			switch v := m[k].(type) {
			case float64:
				return int(v)
			case string:
				if n, err := strconv.Atoi(v); err == nil {
					return n
				}
			}
		}
		return 0
	}
	s := Session{
		SessionID:  str("sessionId", "session_id", "thread_id", "threadId"),
		PID:        num("pid"),
		Cwd:        str("cwd", "directory"),
		Kind:       str("kind"),
		Status:     str("status", "state"),
		Tmux:       str("tmux"),
		Entrypoint: str("entrypoint"),
		Name:       str("name"),
		WaitingFor: str("waitingFor", "waiting_for"),
		Version:    str("version"),
	}
	for _, k := range []string{"statusUpdatedAt", "status_updated_at", "stateUpdatedAt"} {
		if v, ok := m[k].(float64); ok && v > 0 {
			if v > 1e12 {
				s.StatusSince = time.UnixMilli(int64(v))
			} else {
				s.StatusSince = time.Unix(int64(v), 0)
			}
			break
		}
	}
	if id := str("id", "short_id", "shortId"); id != "" && id != s.SessionID {
		s.ShortID = id
	}
	if s.SessionID == "" {
		s.SessionID = str("id")
	}
	return s
}

// mergeSessions adds registry entries the command did not list, and fills in
// fields (tmux pane, entrypoint) the command left out.
func mergeSessions(a, b []Session) []Session {
	for _, s := range b {
		found := false
		for i := range a {
			if a[i].SessionID == s.SessionID {
				found = true
				if a[i].Tmux == "" {
					a[i].Tmux = s.Tmux
				}
				if a[i].Entrypoint == "" {
					a[i].Entrypoint = s.Entrypoint
				}
				if a[i].PID == 0 {
					a[i].PID = s.PID
				}
				if s.Status != "" && !s.StatusSince.IsZero() {
					// The registry is what the process itself writes, with a
					// timestamp: prefer it.
					a[i].Status, a[i].StatusSince = s.Status, s.StatusSince
				} else if a[i].Status == "" {
					a[i].Status = s.Status
				}
			}
		}
		if !found {
			a = append(a, s)
		}
	}
	return a
}

func expand(s string, q Query) string {
	s = strings.ReplaceAll(s, "{bin}", q.Bin)
	for k, v := range q.Homes {
		s = strings.ReplaceAll(s, "{"+k+"}", v)
	}
	return s
}

func contains(list []string, v string) bool {
	if v == "" {
		return false
	}
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

// TmuxPane extracts the pane id (%N) from a tmux location such as
// "main:@3.%12".
func TmuxPane(s string) string {
	if i := strings.LastIndex(s, "%"); i >= 0 {
		return s[i:]
	}
	return ""
}

// applyDesktop accounts for desktop apps, which run sessions without a
// terminal. A session from a desktop app that no source shows as open is
// still treated as open while the app runs, unless the agent's own session
// list is known to include that app's sessions.
func (f *Finder) applyDesktop(h *rules.Harness, q Query, st *State) {
	if len(h.DesktopApps) == 0 {
		return
	}
	if st.Open && st.Session != nil {
		for _, a := range h.DesktopApps {
			if a.IsEntrypoint(st.Session.Entrypoint) {
				st.DesktopApp = a.Name
			}
		}
		return
	}
	if st.Open || q.SessionID == "" {
		return
	}
	var procs []proc.Process
	listed := false
	for _, a := range h.DesktopApps {
		fromApp := a.IsEntrypoint(q.Entrypoint) || (q.Entrypoint == "" && a.GuardUnknown)
		if !fromApp {
			continue
		}
		if q.TrustRegistry[a.Name] && st.Checked && (st.Source == "command" || st.Source == "registry") {
			continue // the agent's own list covers this app: not listed = closed
		}
		if !listed && f.Procs != nil {
			procs, _ = f.Procs()
			listed = true
		}
		for _, p := range procs {
			if a.MatchesProcess(p.Line) {
				st.Open, st.OnlyDesktop, st.Checked, st.DesktopApp = true, true, true, a.Name
				st.Session = &Session{SessionID: q.SessionID, Cwd: q.Cwd, Entrypoint: q.Entrypoint, Kind: "desktop"}
				if st.Source == "" {
					st.Source = "desktop"
				} else {
					st.Source += "+desktop"
				}
				return
			}
		}
	}
}

// DesktopEntries returns the registry entries started from a desktop app, so
// the daemon can learn that the agent's own session list covers it.
func (f *Finder) DesktopEntries(h *rules.Harness, q Query, app *rules.DesktopApp) []Session {
	if h.LiveSessions == nil || h.LiveSessions.RegistryDir == "" {
		return nil
	}
	list, _ := f.fromRegistry(expand(h.LiveSessions.RegistryDir, q))
	var out []Session
	for _, s := range list {
		if app.IsEntrypoint(s.Entrypoint) {
			out = append(out, s)
		}
	}
	return out
}
