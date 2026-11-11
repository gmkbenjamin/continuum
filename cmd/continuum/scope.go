package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/scope"
	"github.com/continuum-dev/continuum/internal/store"
)

// knownSession is a session the user can choose.
type knownSession struct {
	Harness, ID, Short, Name, Cwd, Where string
	Open                                 bool
	LastSeen                             time.Time
}

// knownSessions lists open sessions (from the agents) and recently seen ones
// (from Continuum's own records), newest first.
func knownSessions() []knownSession {
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	r, _ := rules.Cached(p.Rules)
	var out []knownSession
	seen := map[string]bool{}
	for _, name := range r.HarnessNames() {
		h := r.Harness(name)
		if h.LiveSessions == nil || !cfg.HarnessEnabled(name) {
			continue
		}
		bin, _ := deliver.Binary(cfg, h)
		list, _ := live.Default().List(h, live.Query{Bin: bin, Homes: map[string]string{"claude_home": cfg.ClaudeDir(), "codex_home": cfg.CodexDir()}})
		for _, s := range list {
			if s.SessionID == "" || seen[name+s.SessionID] {
				continue
			}
			seen[name+s.SessionID] = true
			where := s.Kind
			switch {
			case s.PID <= 0 && s.Kind == "background":
				where += ", not running"
			case s.WaitingFor != "":
				where += ", waiting: " + s.WaitingFor
			case s.Status != "":
				where += ", " + s.Status
			}
			out = append(out, knownSession{Harness: name, ID: s.SessionID, Short: s.ShortID, Name: s.Name, Cwd: s.Cwd, Where: where, Open: true, LastSeen: time.Now()})
		}
	}
	sessions := map[string]*store.Session{}
	_ = store.LoadJSON(p.Sessions, &sessions)
	for _, s := range sessions {
		if s.SessionID == "" || seen[s.Harness+s.SessionID] || time.Since(s.LastSeen) > 3*24*time.Hour || s.Ended {
			continue
		}
		seen[s.Harness+s.SessionID] = true
		out = append(out, knownSession{Harness: s.Harness, ID: s.SessionID, Cwd: s.Cwd, Where: "seen " + ago(s.LastSeen), LastSeen: s.LastSeen})
	}
	// Open sessions first, then by recency.
	for i := 1; i < len(out); i++ {
		for k := i; k > 0 && less(out[k], out[k-1]); k-- {
			out[k], out[k-1] = out[k-1], out[k]
		}
	}
	return out
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

// resolveEntry turns an argument into a scope entry: a directory, or a
// session found by id, id prefix, short id or name.
func resolveEntry(arg string, sessions []knownSession) (scope.Entry, error) {
	if arg == "." || strings.HasPrefix(arg, "/") || strings.HasPrefix(arg, "~") || strings.HasPrefix(arg, "./") || strings.HasPrefix(arg, "../") {
		path := arg
		if strings.HasPrefix(path, "~") {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
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
	var hits []knownSession
	for _, s := range sessions {
		if harness != "" && s.Harness != harness {
			continue
		}
		if s.ID == arg || s.Short == arg || strings.EqualFold(s.Name, arg) {
			hits = []knownSession{s}
			break
		}
		if (len(arg) >= 4 && strings.HasPrefix(s.ID, arg)) || (s.Name != "" && strings.HasPrefix(strings.ToLower(s.Name), strings.ToLower(arg))) {
			hits = append(hits, s)
		}
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
		return scope.Entry{}, fmt.Errorf("no session matches %q (see `continuum sessions`; a folder works too, e.g. `continuum choose .`)", arg)
	}
	var names []string
	for _, h := range hits {
		names = append(names, h.Harness+":"+shortID(h.ID)+" "+h.Name)
	}
	return scope.Entry{}, fmt.Errorf("%q %w: %s", arg, scope.ErrAmbiguous, strings.Join(names, ", "))
}

func cmdScope(args []string) error {
	p := core.GetPaths()
	sc, err := scope.Load(p)
	if err != nil {
		return err
	}
	if len(args) > 0 {
		switch args[0] {
		case scope.ModeAll, scope.ModeChosen:
			sc.Mode = args[0]
			if err := sc.Save(p); err != nil {
				return err
			}
		default:
			return errors.New("usage: continuum scope [all|chosen]")
		}
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
	sc, err := scope.Load(p)
	if err != nil {
		return err
	}
	sessions := knownSessions()
	for _, a := range args {
		e, err := resolveEntry(a, sessions)
		if err != nil {
			return err
		}
		var changed bool
		if list == "chosen" {
			sc.Chosen, changed = scope.Add(sc.Chosen, e)
			sc.Excluded, _ = scope.Remove(sc.Excluded, e, false)
		} else {
			sc.Excluded, changed = scope.Add(sc.Excluded, e)
		}
		verb := map[string]string{"chosen": "chosen", "excluded": "excluded"}[list]
		if !changed {
			verb = "already " + verb
		}
		fmt.Printf("%s: %s\n", verb, e.String())
	}
	if err := sc.Save(p); err != nil {
		return err
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
	sc, err := scope.Load(p)
	if err != nil {
		return err
	}
	sessions := knownSessions()
	total := 0
	for _, a := range args {
		var e scope.Entry
		if a != "all" {
			if e, err = resolveEntry(a, sessions); err != nil {
				// Fall back to the raw text: it may name an entry for a
				// session that is gone.
				e = scope.Entry{Session: a}
			}
		}
		var n int
		if list == "chosen" {
			sc.Chosen, n = scope.Remove(sc.Chosen, e, a == "all")
		} else {
			sc.Excluded, n = scope.Remove(sc.Excluded, e, a == "all")
		}
		total += n
	}
	if err := sc.Save(p); err != nil {
		return err
	}
	fmt.Printf("removed %d %s entr%s\n", total, list, map[bool]string{true: "y", false: "ies"}[total == 1])
	return nil
}

func cmdSessions(args []string) error {
	p := core.GetPaths()
	sc, _ := scope.Load(p)
	list := knownSessions()
	if len(list) == 0 {
		fmt.Println("No open or recent agent sessions found.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "COVERED\tAGENT\tID\tNAME\tSTATE\tDIR")
	for _, s := range list {
		ok, _ := sc.Covers(&core.Job{Harness: s.Harness, SessionID: s.ID, Cwd: s.Cwd})
		mark := "yes"
		if !ok {
			mark = "no"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", mark, s.Harness, shortID(s.ID), s.Name, s.Where, s.Cwd)
	}
	tw.Flush()
	fmt.Printf("\nScope: %s. Change with `continuum scope all|chosen`, `continuum choose ID`, `continuum exclude ID`.\n", sc.Mode)
	return nil
}
