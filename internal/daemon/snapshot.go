package daemon

import (
	"os"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/mux"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/store"
)

// Snapshot is the daemon's canonical, resolved view of open and recent
// sessions and live wrapper instances, published atomically to Paths.State so
// CLI commands (`status`, `sessions`, `choose`) share one source of truth with
// the daemon instead of recomputing liveness independently.
type Snapshot struct {
	DaemonPID           int               `json:"daemon_pid"`
	GeneratedAt         time.Time         `json:"generated_at"`
	TrackClosedSessions bool              `json:"track_closed_sessions"`
	Sessions            []SnapshotSession `json:"sessions"`
	Wrappers            []SnapshotWrapper `json:"wrappers"`
}

// SnapshotSession is one resolved conversation entry in a Snapshot.
type SnapshotSession struct {
	Harness     string            `json:"harness"`
	ID          string            `json:"id"`
	Short       string            `json:"short,omitempty"`
	Name        string            `json:"name,omitempty"`
	Cwd         string            `json:"cwd,omitempty"`
	Where       string            `json:"where,omitempty"`
	Open        bool              `json:"open"`
	LastSeen    time.Time         `json:"last_seen,omitzero"`
	Status      string            `json:"status,omitempty"`
	StatusSince time.Time         `json:"status_since,omitzero"`
	Kind        string            `json:"kind,omitempty"`
	WaitingFor  string            `json:"waiting_for,omitempty"`
	Tmux        string            `json:"tmux,omitempty"`
	Entrypoint  string            `json:"entrypoint,omitempty"`
	PID         int               `json:"pid,omitempty"`
	Location    *core.Location    `json:"location,omitempty"`
	Live        bool              `json:"live,omitempty"`
	Checked     bool              `json:"checked,omitempty"`
	StaleCopy   bool              `json:"stale_copy,omitempty"`
	Host        string            `json:"host,omitempty"`
	Copies      []SnapshotSession `json:"copies,omitempty"`
}

// SnapshotWrapper describes a live `continuum run` wrapper instance.
type SnapshotWrapper struct {
	ID              string    `json:"id"`
	Harness         string    `json:"harness"`
	Cwd             string    `json:"cwd,omitempty"`
	Argv            []string  `json:"argv,omitempty"`
	Started         time.Time `json:"started,omitzero"`
	PID             int       `json:"pid,omitempty"`
	Host            string    `json:"host,omitempty"`
	ActiveSessionID string    `json:"active_session_id,omitempty"`
}

var snapshotMuxNames = map[string]string{
	"screen":  "GNU screen",
	"zellij":  "zellij",
	"herdr":   "herdr",
	"rex":     "Rex",
	"wezterm": "WezTerm",
	"kitty":   "kitty",
	"ghostty": "Ghostty",
}

var snapshotTerminalApps = map[string]string{
	"apple_terminal": "Terminal", "iterm.app": "iTerm2", "ghostty": "Ghostty", "wezterm": "WezTerm",
	"vscode": "VS Code", "warpterminal": "Warp", "hyper": "Hyper", "tabby": "Tabby", "zed": "Zed",
	"rio": "Rio", "kitty": "kitty", "alacritty": "Alacritty", "tmux": "tmux", "rex": "Rex",
}

// HostFromEnv names where a process runs from its environment: the multiplexer
// pane it is in, else the terminal app.
func HostFromEnv(r *rules.Rules, env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	if r != nil {
		if loc := mux.LocateFromEnv(r, env); loc != nil {
			if name, ok := snapshotMuxNames[loc.Kind]; ok {
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
	if name, ok := snapshotTerminalApps[strings.ToLower(env["TERM_PROGRAM"])]; ok {
		return name
	}
	return ""
}

// SnapshotStaleCopy reports whether k (listed by the agent) is an out-of-date
// copy of a conversation whose recorded process has closed or moved on.
func SnapshotStaleCopy(r *rules.Rules, c *Compat, k SnapshotSession, st *store.Session) bool {
	if !c.ExactPIDs(k.Harness) || !k.Open || k.PID <= 0 || st == nil || st.PID <= 0 || k.PID == st.PID {
		return false
	}
	for _, cp := range k.Copies {
		if cp.PID == st.PID {
			return false
		}
	}
	if r == nil {
		return false
	}
	h := r.Harness(k.Harness)
	return h.LiveSessions == nil || !live.ActiveSince(&live.Session{Status: k.Status, StatusSince: k.StatusSince}, h.LiveSessions.BusyStates, st.LastSeen)
}

// BuildSnapshot computes the canonical live session and wrapper snapshot using
// one shared resolution pass for both the daemon and the CLI.
func BuildSnapshot(
	p core.Paths,
	cfg core.Config,
	r *rules.Rules,
	sessions map[string]*store.Session,
	jobs map[string]*core.Job,
	instances map[string]*store.Instance,
	compat *Compat,
	now time.Time,
) Snapshot {
	if now.IsZero() {
		now = time.Now()
	}
	var out []SnapshotSession
	seen := map[string]bool{}
	index := map[string]int{}

	if r != nil {
		for _, name := range r.HarnessNames() {
			h := r.Harness(name)
			if h.LiveSessions == nil || !cfg.HarnessEnabled(name) {
				continue
			}
			bin, _ := deliver.Binary(cfg, h)
			list, _ := live.Default().List(h, live.Query{Bin: bin, Homes: cfg.Homes()})
			for _, s := range list {
				if s.SessionID == "" {
					continue
				}
				where := s.Kind
				switch {
				case s.PID <= 0 && s.Kind == "background":
					where += ", not running"
				case s.WaitingFor != "":
					where += ", waiting: " + s.WaitingFor
				case s.Status != "":
					where += ", " + s.Status
				}
				open := s.PID > 0
				lastSeen := s.StatusSince
				if open {
					lastSeen = now
				}
				ks := SnapshotSession{
					Harness: name, ID: s.SessionID, Short: s.ShortID, Name: s.Name, Cwd: s.Cwd, Where: where,
					Open: open, LastSeen: lastSeen, Live: true, Checked: true,
					Status: s.Status, StatusSince: s.StatusSince, Kind: s.Kind, WaitingFor: s.WaitingFor,
					Tmux: s.Tmux, Entrypoint: s.Entrypoint, PID: s.PID,
				}
				if seen[name+s.SessionID] {
					first := &out[index[name+s.SessionID]]
					if st := sessions[store.SessionKey(name, s.SessionID)]; st != nil && st.PID > 0 && st.PID == s.PID && first.PID != s.PID {
						ks.Copies = append(first.Copies, *first)
						first.Copies = nil
						*first = ks
					} else {
						first.Copies = append(first.Copies, ks)
					}
					continue
				}
				seen[name+s.SessionID] = true
				index[name+s.SessionID] = len(out)
				out = append(out, ks)
			}
		}
	}

	// Fill in what hooks recorded (where it runs, where it started) for the
	// sessions the agent listed itself.
	for i := range out {
		if st := sessions[store.SessionKey(out[i].Harness, out[i].ID)]; st != nil {
			out[i].StaleCopy = SnapshotStaleCopy(r, compat, out[i], st)
			samePID := st.PID <= 0 || out[i].PID <= 0 || st.PID == out[i].PID
			if out[i].Location == nil && samePID && (st.Location == nil || st.Location.Kind != "wrapper" || deliver.WrapperAlive(p, st.Location.Target)) {
				out[i].Location = st.Location
			}
			if out[i].Entrypoint == "" {
				out[i].Entrypoint = st.Entrypoint
			}
			if !out[i].Open && st.LastSeen.After(out[i].LastSeen) {
				out[i].LastSeen = st.LastSeen
			}
		}
	}

	wrappers := deliver.LiveWrappers(p)
	var procs []proc.Process
	if len(wrappers) > 0 {
		procs, _ = proc.List()
	}
	bindings := ResolveWrapperBindings(r, sessions, instances, wrappers, procs, proc.Cwd)
	wrapperHosts := make(map[string]string, len(wrappers))
	snapWrappers := make([]SnapshotWrapper, 0, len(wrappers))
	for _, w := range wrappers {
		host := HostFromEnv(r, proc.Environ(w.PID))
		wrapperHosts[w.ID] = host
		activeID := ""
		if inst := instances[w.ID]; inst != nil {
			activeID = inst.ActiveSessionID
		}
		snapWrappers = append(snapWrappers, SnapshotWrapper{
			ID: w.ID, Harness: w.Harness, Cwd: w.Cwd, Argv: w.Argv,
			Started: w.Started, PID: w.PID, Host: host, ActiveSessionID: activeID,
		})
	}

	for _, s := range sessions {
		if s == nil || s.SessionID == "" || DriverFor(s.Harness).IgnoreTranscript(s.TranscriptPath) {
			continue
		}
		key := store.SessionKey(s.Harness, s.SessionID)
		j := jobs[key]
		pending := cfg.ClosedSessionsTracked() && j != nil && (j.State == core.StatePending || j.State == core.StateNeedsUser)
		w, bound := bindings[key]
		if seen[s.Harness+s.SessionID] || (!pending && !bound && (now.Sub(s.LastSeen) > 3*24*time.Hour || s.Ended)) {
			continue
		}
		seen[s.Harness+s.SessionID] = true
		loc, pid := DriverFor(s.Harness).SanitizeSession(s)
		ks := SnapshotSession{
			Harness: s.Harness, ID: s.SessionID, Cwd: s.Cwd, LastSeen: s.LastSeen,
			Location: loc, Entrypoint: s.Entrypoint, PID: pid,
		}
		if bound {
			ks.Location = &core.Location{Kind: "wrapper", Target: w.ID}
			ks.Open, ks.Checked = true, true
			ks.Host = wrapperHosts[w.ID]
			out = append(out, ks)
			continue
		}
		if r != nil {
			if h := r.Harness(s.Harness); h != nil && (pending || now.Sub(s.LastSeen) < 12*time.Hour) && (h.LiveSessions != nil || len(h.DesktopApps) > 0) {
				checkPID := ks.PID
				wrapperAlive := false
				if ks.Location != nil && ks.Location.Kind == "wrapper" {
					if deliver.WrapperAlive(p, ks.Location.Target) {
						wrapperAlive = true
					} else {
						checkPID = 0
						ks.PID = 0
						ks.Location = nil
					}
				}
				if !wrapperAlive && checkPID > 0 {
					if _, name, ok := proc.Info(checkPID); !ok || !h.IsHarnessProcess(name) {
						checkPID = 0
					}
				}
				bin, _ := deliver.Binary(cfg, h)
				st := live.Default().Lookup(h, live.Query{
					SessionID: s.SessionID, Cwd: s.Cwd, PID: checkPID, Bin: bin,
					Entrypoint: s.Entrypoint, Homes: cfg.Homes(),
				})
				ks.Open, ks.Checked = st.Open, st.Checked
				if st.Source == "processes" && !st.ProcessMatch {
					ks.Open = wrapperAlive || (checkPID > 0 && st.Session != nil && st.Session.PID == checkPID)
					if !ks.Open && st.Open {
						// An agent process is running in this folder, so we
						// cannot prove this session is closed — only that no
						// single PID matched unambiguously.
						ks.Checked = false
					}
				}
				if st.ProcessMatch && st.Session != nil {
					ks.PID = st.Session.PID
					ks.Location = nil // use the matched process's current route below
				}
			}
		}
		out = append(out, ks)
	}

	locate := func(k *SnapshotSession) {
		if k.PID <= 0 || !k.Open {
			return
		}
		env := proc.Environ(k.PID)
		if loc := deliver.LocationFromEnv(p, r, env); loc != nil && (loc.Kind == "wrapper" || k.Location == nil) {
			k.Location = loc
		}
		if host := HostFromEnv(r, env); host != "" {
			k.Host = host
		}
	}
	for i := range out {
		locate(&out[i])
		for c := range out[i].Copies {
			locate(&out[i].Copies[c])
		}
	}

	// Open sessions first, then by recency.
	for i := 1; i < len(out); i++ {
		for k := i; k > 0 && snapshotSessionLess(out[k], out[k-1]); k-- {
			out[k], out[k-1] = out[k-1], out[k]
		}
	}

	return Snapshot{
		DaemonPID:           os.Getpid(),
		GeneratedAt:         time.Now(),
		TrackClosedSessions: cfg.ClosedSessionsTracked(),
		Sessions:            out,
		Wrappers:            snapWrappers,
	}
}

func snapshotSessionLess(a, b SnapshotSession) bool {
	if a.Open != b.Open {
		return a.Open
	}
	return a.LastSeen.After(b.LastSeen)
}

const snapshotMaxAge = 15 * time.Second

// SaveSnapshot atomically persists snap to p.State.
func SaveSnapshot(p core.Paths, snap Snapshot) error {
	if p.State == "" {
		return nil
	}
	return store.SaveJSON(p.State, snap)
}

// LoadSnapshot reads p.State if present.
func LoadSnapshot(p core.Paths) (Snapshot, bool) {
	if p.State == "" {
		return Snapshot{}, false
	}
	var snap Snapshot
	if err := store.LoadJSON(p.State, &snap); err != nil || snap.GeneratedAt.IsZero() {
		return Snapshot{}, false
	}
	return snap, true
}

func snapshotFresh(p core.Paths, cfg core.Config, snap Snapshot, now time.Time) bool {
	if snap.DaemonPID <= 0 || !proc.Alive(snap.DaemonPID) {
		return false
	}
	age := now.Sub(snap.GeneratedAt)
	if age < -2*time.Second || age > snapshotMaxAge {
		return false
	}
	if snap.TrackClosedSessions != cfg.ClosedSessionsTracked() {
		return false
	}
	for _, path := range []string{p.Config, p.Jobs, p.Sessions, p.Instances, p.Wrappers} {
		if path == "" {
			continue
		}
		if info, err := os.Stat(path); err == nil && info.ModTime().After(snap.GeneratedAt) {
			return false
		}
	}
	return true
}

// LoadOrBuildSnapshot returns the daemon's freshly published Snapshot when the
// daemon is running and its state file is up to date; otherwise it computes an
// identical Snapshot directly via BuildSnapshot.
func LoadOrBuildSnapshot(p core.Paths, cfg core.Config, r *rules.Rules, daemonRunning bool) Snapshot {
	now := time.Now()
	if daemonRunning {
		if snap, ok := LoadSnapshot(p); ok && snapshotFresh(p, cfg, snap, now) {
			return snap
		}
	}
	sessions := map[string]*store.Session{}
	_ = store.LoadJSON(p.Sessions, &sessions)
	instances := map[string]*store.Instance{}
	_ = store.LoadJSON(p.Instances, &instances)
	jobs, _ := store.LoadJobs(p.Jobs)
	compat := LoadCompat(p)
	return BuildSnapshot(p, cfg, r, sessions, jobs, instances, compat, now)
}

// SnapshotNow builds the current live Snapshot from the daemon's in-memory state.
func (d *Daemon) SnapshotNow() Snapshot {
	return BuildSnapshot(d.Paths, d.Config, d.Rules, d.Sessions, d.Jobs, d.Instances, d.Compat, d.Now())
}

func (d *Daemon) maybePublishSnapshot(wasDirty bool) {
	if !d.PublishSnapshot {
		return
	}
	now := d.Now()
	if !wasDirty && !d.lastSnapshot.IsZero() && now.Sub(d.lastSnapshot) < 10*time.Second {
		return
	}
	d.lastSnapshot = now
	if err := SaveSnapshot(d.Paths, d.SnapshotNow()); err != nil {
		d.debugf("save state snapshot: %v", err)
	}
}
