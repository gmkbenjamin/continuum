package main

// "this": the agent session a command runs in. An agent controls Continuum
// through its shell tool, for its own session: `continuum this`,
// `continuum exclude this`, `continuum message set --session this "…"`.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/hooks"
	"github.com/continuum-dev/continuum/internal/messages"
	"github.com/continuum-dev/continuum/internal/mux"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/scope"
	"github.com/continuum-dev/continuum/internal/store"
)

// here is the session a command runs in.
type here struct {
	Harness string
	ID      string // the session id, when known
	Wrapper string // Continuum's wrapper the agent runs in, if it does
	PID     int    // the agent's process
	Cwd     string
}

var errNotInSession = errors.New("`this` means the agent session the command runs in, and this command isn't run by an agent (name a session instead; see `continuum sessions`)")

// What thisSession looks at. Tests replace them.
var (
	thisTree = proc.Tree
	thisEnv  = func() map[string]string { return mux.EnvMap(os.Environ()) }
	thisPPID = os.Getppid
)

func isThis(arg string) bool { return arg == "this" || arg == "self" }

// thisSession finds the agent session this command runs in: the agent among
// the command's parent processes, and which of its sessions that is.
func thisSession() (here, error) {
	p := core.GetPaths()
	r, err := rules.Cached(p.Rules)
	if err != nil || r == nil {
		return here{}, fmt.Errorf("rules: %v", err)
	}
	env := thisEnv()
	harness, pid := hooks.NearestAgent(r, thisTree(), thisPPID())
	wrapper := env["CONTINUUM_WRAPPER_ID"]
	if wh := wrapperHarness(p, wrapper); wrapper != "" && (wh == "" || harness != "" && wh != harness) {
		wrapper = "" // inherited from another agent's wrapper
	} else if harness == "" && wrapper != "" {
		harness = wh // an agent under an interpreter (node, python), in the wrapper
	}
	if harness == "" {
		return here{}, errNotInSession
	}
	h := here{Harness: harness, PID: pid, Wrapper: wrapper}
	// The agent says which session (Claude Code, Grok Build, Antigravity).
	for _, v := range r.Harness(harness).SessionEnv {
		if id := env[v]; id != "" {
			h.ID = id
			return h, nil
		}
	}
	// Continuum's records: the session running in this process, wrapper or
	// pane, as the agent's hooks reported it.
	sessions := map[string]*store.Session{}
	_ = store.LoadJSON(p.Sessions, &sessions)
	pane := mux.LocateFromEnv(r, env)
	var best *store.Session
	for _, s := range sessions {
		if s.Harness != harness || s.Ended || s.SessionID == "" || s.Location == nil && s.PID == 0 {
			continue
		}
		match := pid > 0 && s.PID == pid
		if s.Location != nil {
			switch {
			case wrapper != "":
				match = match || s.Location.Kind == "wrapper" && s.Location.Target == wrapper
			case pane != nil:
				match = match || s.Location.Kind == pane.Kind && s.Location.Target == pane.Target
			}
		}
		if match && (best == nil || s.LastSeen.After(best.LastSeen)) {
			best = s
		}
	}
	if best != nil {
		h.ID, h.Cwd = best.SessionID, best.Cwd
		return h, nil
	}
	// The agent's own list of open sessions (Claude Code, Grok Build).
	if pid > 0 {
		for _, k := range knownSessions() {
			if k.Harness == harness && k.PID == pid && k.ID != "" {
				h.ID, h.Cwd = k.ID, k.Cwd
				return h, nil
			}
		}
	}
	if wrapper != "" {
		return h, nil // known by its wrapper only
	}
	return h, fmt.Errorf("can't tell which %s session this is: Continuum hasn't seen it yet (name it instead; see `continuum sessions`)", r.Harness(harness).DisplayName)
}

func wrapperHarness(p core.Paths, id string) string {
	if id == "" {
		return ""
	}
	var meta struct {
		Harness string `json:"harness"`
	}
	data, _ := os.ReadFile(filepath.Join(p.Wrappers, id, "meta.json"))
	_ = json.Unmarshal(data, &meta)
	return meta.Harness
}

// entry is the scope and message entry for the session.
func (h here) entry() scope.Entry {
	if h.ID != "" {
		return scope.Entry{Harness: h.Harness, Session: h.ID, Label: filepath.Base(h.Cwd)}
	}
	return scope.Entry{Harness: h.Harness, Wrapper: h.Wrapper}
}

// selector names the session's resume for `cancel` and `resume-now`.
func (h here) selector() string {
	if h.ID != "" {
		return h.Harness + ":" + h.ID
	}
	return h.Harness + "@" + (&core.Location{Kind: "wrapper", Target: h.Wrapper}).String()
}

func cmdThis(args []string) error {
	fs := newFlagSet("this")
	asJSON := fs.Bool("json", false, "Print it as JSON.")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	h, err := thisSession()
	if err != nil {
		return err
	}
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	r, _ := rules.Cached(p.Rules)
	var row *sessionRow
	for _, rw := range sessionRows(3*24*time.Hour, true) {
		if h.ID != "" && rw.ID == h.ID || h.ID == "" && rw.Wrapper == h.Wrapper && h.Wrapper != "" {
			rw := rw
			row = &rw
			break
		}
	}
	if row == nil {
		row = &sessionRow{Agent: h.Harness, ID: h.ID, Folder: h.Cwd, Wrapper: h.Wrapper, State: "active", Detail: "open", Where: "-"}
		if hr := r.Harness(h.Harness); hr != nil && hr.DisplayName != "" {
			row.Agent = hr.DisplayName
		}
		sc, _ := scope.Load(p)
		row.Covered, _ = sc.Covers(h.job())
	}
	msgs, _ := messages.Load(p)
	choice := msgs.Resolve(h.job(), false, cfg, r)
	if *asJSON {
		out := struct {
			sessionRow
			MessageText string `json:"message"`
		}{*row, choice.Text}
		out.sessionRow.Message = choice.Source
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	name := row.Name
	if name == "" && h.ID != "" {
		name = shortID(h.ID)
	}
	if name == "" {
		name = "wrapper " + h.Wrapper
	}
	folder := row.Folder
	if folder == "" {
		folder = "-"
	}
	fmt.Printf("This session: %s · %s · %s\n", row.Agent, name, tildePath(folder))
	fmt.Printf("  state:    %s\n", row.Detail)
	fmt.Printf("  where:    %s\n", row.Where)
	if row.Covered {
		fmt.Println("  resumes:  yes, when a limit resets or an API error passes")
	} else {
		fmt.Println("  resumes:  no (`continuum include this`, or `continuum choose this`)")
	}
	fmt.Printf("  message:  %s: %q\n", choice.Source, choice.Text)
	fmt.Println("\nFor this session: `continuum exclude this` · `continuum include this` ·")
	fmt.Println("`continuum message set --session this \"…\"` · `continuum cancel this` · `continuum resume-now this`")
	return nil
}

// job is a stand-in job for the session, for scope and message checks.
func (h here) job() *core.Job {
	j := &core.Job{Harness: h.Harness, SessionID: h.ID, Cwd: h.Cwd}
	if h.Wrapper != "" {
		j.Location = &core.Location{Kind: "wrapper", Target: h.Wrapper}
	}
	return j
}
