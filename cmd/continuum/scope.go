package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/daemon"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/scope"
	"github.com/continuum-dev/continuum/internal/store"
)

// knownSession is a session the user can choose.
type knownSession struct {
	Harness, ID, Short, Name, Cwd, Where string
	Open                                 bool
	LastSeen                             time.Time
	// From the agent's own session list (Claude Code), when it has one.
	Status, Kind, WaitingFor, Tmux, Entrypoint string
	PID                                        int
	StatusSince                                time.Time // when Status last changed, when the agent says
	// From hooks: where the session runs.
	Location *core.Location
	Live     bool // the agent reported this session itself, just now
	// Checked: whether it is open was actually checked just now (the agent's
	// list, its registry or the process table), so !Open means closed.
	Checked bool
	// ResumesClosed: Continuum can continue it once closed, with the agent's
	// own resume command (set by sessionRows).
	ResumesClosed bool
	// StaleCopy: an out-of-date copy of the conversation is the only process
	// holding it: the process the hooks last heard from has exited.
	// Continuum won't type into it.
	StaleCopy bool
	// Host: the multiplexer or terminal app the session's process runs in
	// (Rex, tmux, iTerm2, ...), from its environment.
	Host string
	// Copies: other processes that hold the same conversation (it was
	// resumed again in another pane). Continuum resumes only the one its
	// hooks last heard from; these are out of date.
	Copies []knownSession
}

// knownSessions lists open sessions (from the agents) and recently seen ones
// (from Continuum's own records), newest first.
func knownSessions() []knownSession {
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	r, _ := rules.Cached(p.Rules)
	var out []knownSession
	seen := map[string]bool{}
	sessions := map[string]*store.Session{}
	_ = store.LoadJSON(p.Sessions, &sessions)
	jobs, _ := store.LoadJobs(p.Jobs)
	compat := daemon.LoadCompat(p)
	index := map[string]int{}
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
				lastSeen = time.Now()
			}
			ks := knownSession{Harness: name, ID: s.SessionID, Short: s.ShortID, Name: s.Name, Cwd: s.Cwd, Where: where,
				Open: open, LastSeen: lastSeen, Live: true, Checked: true,
				Status: s.Status, StatusSince: s.StatusSince, Kind: s.Kind, WaitingFor: s.WaitingFor, Tmux: s.Tmux, Entrypoint: s.Entrypoint, PID: s.PID}
			if seen[name+s.SessionID] {
				// The same conversation in a second process: the one the
				// hooks last heard from holds it, the other is a copy.
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
	// Fill in what hooks recorded (where it runs, where it started) for the
	// sessions the agent listed itself.
	for i := range out {
		if st := sessions[store.SessionKey(out[i].Harness, out[i].ID)]; st != nil {
			out[i].StaleCopy = staleCopy(r, compat, out[i], st)
			// The pane the hooks recorded belongs to the process they heard
			// from: not to another copy listed in its place.
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
	for _, s := range sessions {
		// A session with a resume pending is listed however it ended and
		// however long ago it was seen: status says how that resume goes.
		j := jobs[store.SessionKey(s.Harness, s.SessionID)]
		pending := j != nil && (j.State == core.StatePending || j.State == core.StateNeedsUser)
		if s.SessionID == "" || seen[s.Harness+s.SessionID] || !pending && (time.Since(s.LastSeen) > 3*24*time.Hour || s.Ended) {
			continue
		}
		seen[s.Harness+s.SessionID] = true
		ks := knownSession{Harness: s.Harness, ID: s.SessionID, Cwd: s.Cwd, Where: "seen " + ago(s.LastSeen), LastSeen: s.LastSeen,
			Location: s.Location, Entrypoint: s.Entrypoint, PID: s.PID}
		if s.Harness == "codex" && s.PID > 0 && live.CodexAppServer(proc.Args(s.PID)) {
			ks.Location, ks.PID = nil, 0
		}
		// Agents without a session list (Codex, ...): ask the process table
		// whether a recent session is still open.
		// One with a resume pending is checked however long ago it was seen.
		if h := r.Harness(s.Harness); h != nil && (pending || time.Since(s.LastSeen) < 12*time.Hour) && (h.LiveSessions != nil || len(h.DesktopApps) > 0) {
			pid := ks.PID
			if ks.Location != nil && ks.Location.Kind == "wrapper" && !deliver.WrapperAlive(p, ks.Location.Target) {
				pid = 0
			} else if pid > 0 {
				if _, name, ok := proc.Info(pid); !ok || !h.IsHarnessProcess(name) {
					pid = 0
				}
			}
			bin, _ := deliver.Binary(cfg, h)
			st := live.Default().Lookup(h, live.Query{SessionID: s.SessionID, Cwd: s.Cwd, PID: pid, Bin: bin, Entrypoint: s.Entrypoint,
				Homes: cfg.Homes()})
			ks.Open, ks.Checked = st.Open, st.Checked
			if s.Harness == "codex" && st.Source == "processes" && !st.ProcessMatch {
				ks.Open = pid > 0 && st.Session != nil && st.Session.PID == pid
			}
			if st.ProcessMatch {
				ks.PID = st.Session.PID
				ks.Location = nil // use the matched process's current route below
			}
		}
		out = append(out, ks)
	}
	// Sessions started before Continuum's hooks (or without them): their
	// process's environment still says which pane they run in.
	locate := func(k *knownSession) {
		if k.PID <= 0 || !k.Open {
			return
		}
		env := proc.Environ(k.PID)
		if loc := deliver.LocationFromEnv(p, r, env); loc != nil && (loc.Kind == "wrapper" || k.Location == nil) {
			k.Location = loc
		}
		k.Host = hostFromEnv(r, env)
	}
	for i := range out {
		locate(&out[i])
		for c := range out[i].Copies {
			locate(&out[i].Copies[c])
		}
	}
	// Open sessions first, then by recency.
	for i := 1; i < len(out); i++ {
		for k := i; k > 0 && less(out[k], out[k-1]); k-- {
			out[k], out[k-1] = out[k-1], out[k]
		}
	}
	return out
}

// staleCopy: k (listed by the agent) is not the process its hooks last heard
// from, which isn't listed holding the session, and k hasn't done anything
// since: k is an out-of-date copy. Mirrors the daemon's check, for agents
// whose hooks name the exact process (Compat.ExactPIDs).
func staleCopy(r *rules.Rules, c *daemon.Compat, k knownSession, st *store.Session) bool {
	if !c.ExactPIDs(k.Harness) || !k.Open || k.PID <= 0 || st.PID <= 0 || k.PID == st.PID {
		return false
	}
	for _, cp := range k.Copies {
		if cp.PID == st.PID {
			return false
		}
	}
	// The recorded process isn't listed holding it: it quit or moved on to
	// another conversation. k is out of date unless it did something since.
	if r == nil {
		return false
	}
	h := r.Harness(k.Harness)
	return h.LiveSessions == nil || !live.ActiveSince(&live.Session{Status: k.Status, StatusSince: k.StatusSince}, h.LiveSessions.BusyStates, st.LastSeen)
}

func less(a, b knownSession) bool {
	if a.Open != b.Open {
		return a.Open
	}
	return a.LastSeen.After(b.LastSeen)
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

var idLike = regexp.MustCompile(`^[0-9a-fA-F][0-9a-fA-F-]{5,}$`)

func hasWindowsDrive(arg string) bool {
	if len(arg) < 3 {
		return false
	}
	c := arg[0]
	return ((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) && arg[1] == ':' && (arg[2] == '\\' || arg[2] == '/')
}

func isDirArg(arg string) bool {
	return arg == "." || arg == ".." ||
		arg == "~" || strings.HasPrefix(arg, "~/") || strings.HasPrefix(arg, `~\`) ||
		strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../") ||
		strings.HasPrefix(arg, `\`) || strings.HasPrefix(arg, `.\`) || strings.HasPrefix(arg, `..\`) ||
		filepath.IsAbs(arg) || filepath.VolumeName(arg) != "" || hasWindowsDrive(arg)
}

// resolveEntry turns an argument into a scope entry: a directory, or a
// session found by id, id prefix, short id or name.
func resolveEntry(arg string, sessions []knownSession) (scope.Entry, error) {
	if isThis(arg) {
		h, err := thisSession()
		if err != nil {
			return scope.Entry{}, err
		}
		return h.entry(), nil
	}
	if isDirArg(arg) {
		path := arg
		if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
			home, _ := os.UserHomeDir()
			rest := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"), `\`)
			path = filepath.Join(home, rest)
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return scope.Entry{}, err
		}
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			return scope.Entry{}, fmt.Errorf("%s is not a directory", abs)
		}
		return scope.Entry{Dir: abs}, nil
	}
	harness := ""
	if i := strings.IndexByte(arg, ':'); i > 0 {
		harness, arg = arg[:i], arg[i+1:]
	}
	var exact, partial []knownSession
	for _, s := range sessions {
		if harness != "" && s.Harness != harness {
			continue
		}
		if s.ID == arg || s.Short == arg || strings.EqualFold(s.Name, arg) {
			exact = append(exact, s)
			continue
		}
		if (len(arg) >= 4 && strings.HasPrefix(s.ID, arg)) || (s.Name != "" && strings.HasPrefix(strings.ToLower(s.Name), strings.ToLower(arg))) {
			partial = append(partial, s)
		}
	}
	hits := partial
	if len(exact) > 0 {
		hits = exact
	}
	switch len(hits) {
	case 1:
		label := hits[0].Name
		if label == "" {
			label = filepath.Base(hits[0].Cwd)
		}
		return scope.Entry{Harness: hits[0].Harness, Session: hits[0].ID, Label: label}, nil
	case 0:
		if idLike.MatchString(arg) {
			// Not seen yet (or not open): accept a full-looking id as given.
			return scope.Entry{Harness: harness, Session: arg}, nil
		}
		return scope.Entry{}, fmt.Errorf("%w %q (see `continuum sessions`; a folder works too, e.g. `continuum choose .`)", errSessionNotFound, arg)
	}
	var names []string
	for _, h := range hits {
		names = append(names, h.Harness+":"+shortID(h.ID)+" "+h.Name)
	}
	return scope.Entry{}, fmt.Errorf("%q %w: %s", arg, scope.ErrAmbiguous, strings.Join(names, ", "))
}

var errSessionNotFound = errors.New("no session matches")

// resolveSessionTargets resolves arg for removal/reset commands. For "this",
// it returns all entries (session and/or wrapper) identifying the current
// session; if no known session matches arg, it falls back to a raw session
// entry so gone sessions can still be cleaned up, while still surfacing
// "this", directory, and ambiguity errors.
func resolveSessionTargets(arg string, sessions []knownSession) ([]scope.Entry, error) {
	if isThis(arg) {
		h, err := thisSession()
		if err != nil {
			return nil, err
		}
		return h.entries(), nil
	}
	e, err := resolveEntry(arg, sessions)
	if err == nil {
		return []scope.Entry{e}, nil
	}
	if errors.Is(err, errSessionNotFound) {
		harness, session := "", arg
		if !isDirArg(arg) {
			if i := strings.IndexByte(arg, ':'); i > 0 {
				harness, session = arg[:i], arg[i+1:]
			}
		}
		return []scope.Entry{{Harness: harness, Session: session}}, nil
	}
	return nil, err
}

func cmdScope(args []string) error {
	p := core.GetPaths()
	var sc scope.Scope
	var err error
	if len(args) > 0 {
		switch args[0] {
		case scope.ModeAll, scope.ModeChosen:
			sc, err = scope.Update(p, func(cur scope.Scope) (scope.Scope, bool, error) {
				if cur.Mode == args[0] {
					return cur, false, nil
				}
				cur.Mode = args[0]
				return cur, true, nil
			})
		default:
			return errors.New("usage: continuum scope [all|chosen]")
		}
	} else {
		sc, err = scope.Load(p)
	}
	if err != nil {
		return err
	}
	if sc.Mode == scope.ModeChosen {
		fmt.Println("Scope: chosen sessions only.")
		if len(sc.Chosen) == 0 {
			fmt.Println("  Nothing is chosen yet: `continuum choose <id|name|folder>`, or start an agent with")
			fmt.Println("  `CONTINUUM_CHOOSE=1 claude` / `continuum run --choose -- codex`.")
		}
		for _, e := range sc.Chosen {
			fmt.Println("  chosen:  ", e.String())
		}
	} else {
		fmt.Println("Scope: every session.")
	}
	for _, e := range sc.Excluded {
		fmt.Println("  excluded:", e.String())
	}
	return nil
}

func cmdChoose(list string, args []string) error {
	if len(args) == 0 {
		fmt.Printf("usage: continuum %s ID|NAME|FOLDER...\n\n", map[string]string{"chosen": "choose", "excluded": "exclude"}[list])
		return cmdSessions(nil)
	}
	p := core.GetPaths()
	sessions := knownSessions()
	entries := make([]scope.Entry, len(args))
	for i, a := range args {
		e, err := resolveEntry(a, sessions)
		if err != nil {
			return err
		}
		entries[i] = e
	}
	var lines []string
	sc, err := scope.Update(p, func(cur scope.Scope) (scope.Scope, bool, error) {
		lines = lines[:0]
		dirty := false
		for _, e := range entries {
			var changed bool
			if list == "chosen" {
				cur.Chosen, changed = scope.Add(cur.Chosen, e)
				var removed int
				cur.Excluded, removed = scope.Remove(cur.Excluded, e, false)
				if removed > 0 {
					dirty = true
				}
			} else {
				cur.Excluded, changed = scope.Add(cur.Excluded, e)
			}
			if changed {
				dirty = true
			}
			verb := map[string]string{"chosen": "chosen", "excluded": "excluded"}[list]
			if !changed {
				verb = "already " + verb
			}
			lines = append(lines, fmt.Sprintf("%s: %s", verb, e.String()))
		}
		return cur, dirty, nil
	})
	if err != nil {
		return err
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	if list == "chosen" && sc.Mode != scope.ModeChosen {
		fmt.Println("Note: the scope is \"all\", so every session is resumed anyway. Run `continuum scope chosen` to limit it to chosen sessions.")
	}
	return nil
}

func cmdUnchoose(list string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: continuum %s ID|NAME|FOLDER|all", map[string]string{"chosen": "unchoose", "excluded": "include"}[list])
	}
	p := core.GetPaths()
	sessions := knownSessions()
	type target struct {
		all   bool
		entry scope.Entry
	}
	var targets []target
	for _, a := range args {
		if a == "all" {
			targets = append(targets, target{all: true})
			continue
		}
		es, err := resolveSessionTargets(a, sessions)
		if err != nil {
			return err
		}
		for _, e := range es {
			targets = append(targets, target{entry: e})
		}
	}
	total := 0
	_, err := scope.Update(p, func(cur scope.Scope) (scope.Scope, bool, error) {
		total = 0
		for _, t := range targets {
			var n int
			if list == "chosen" {
				cur.Chosen, n = scope.Remove(cur.Chosen, t.entry, t.all)
			} else {
				cur.Excluded, n = scope.Remove(cur.Excluded, t.entry, t.all)
			}
			total += n
		}
		return cur, total > 0, nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("removed %d %s entr%s\n", total, list, map[bool]string{true: "y", false: "ies"}[total == 1])
	return nil
}

func cmdSessions(args []string) error {
	fs := newFlagSet("sessions")
	asJSON := fs.Bool("json", false, "Print JSON (for scripts and other apps).")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	rows := sessionRows(3 * 24 * time.Hour)
	if *asJSON {
		return printRowsJSON(rows)
	}
	if len(rows) == 0 {
		fmt.Println("No open or recent agent sessions found.")
		return nil
	}
	printSessionTable(os.Stdout, rows, true)
	sc, _ := scope.Load(core.GetPaths())
	fmt.Printf("\nScope: %s. Change with `continuum scope all|chosen`, `continuum choose ID`, `continuum exclude ID`.\n", sc.Mode)
	return nil
}
