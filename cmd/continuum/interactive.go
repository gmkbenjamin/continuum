package main

// Interactive mode: `continuum` with no arguments in a terminal (or
// `continuum interactive`) opens a menu for everything Continuum does.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
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

type tui struct {
	in  *bufio.Reader
	out io.Writer
	eof bool
}

// ask prints prompt and returns the trimmed answer ("" on Enter or EOF).
func (t *tui) ask(prompt string) string {
	if t.eof {
		return "q"
	}
	fmt.Fprint(t.out, prompt)
	line, err := t.in.ReadString('\n')
	if err != nil && line == "" {
		t.eof = true
		fmt.Fprintln(t.out)
		return "q"
	}
	return strings.TrimSpace(line)
}

func (t *tui) pause() {
	if !t.eof {
		t.ask("\nPress Enter to go back. ")
	}
}

func (t *tui) report(err error) {
	if err != nil && err != errHelpShown {
		fmt.Fprintln(t.out, "  ✗", err)
	}
}

func cmdInteractive(args []string) error {
	return runInteractive(lineReader(os.Stdin), os.Stdout)
}

func runInteractive(in *bufio.Reader, out io.Writer) error {
	t := &tui{in: in, out: out}
	for {
		p := core.GetPaths()
		sc, _ := scope.Load(p)
		fmt.Fprintln(out)
		fmt.Fprintln(out, header(p))
		fmt.Fprintln(out)
		scopeNow := "every session"
		if sc.Mode == scope.ModeChosen {
			scopeNow = "chosen sessions only"
		}
		items := []string{
			"1  Status and scheduled resumes",
			"2  Sessions: choose, exclude, set a message, resume now",
			"3  Resume messages",
			"4  Scope: which sessions are resumed (now: " + scopeNow + ")",
			"5  Wait for an agent's own resume (grace)",
			"6  Watch live in this terminal",
			"7  Start, stop or turn off auto-start (now: " + daemonSummary(p) + ")",
			"8  Check everything (doctor)",
			"9  Recent log",
			"10 Install, update or uninstall",
			"q  Quit",
		}
		for _, it := range items {
			fmt.Fprintln(out, "  "+it)
		}
		switch strings.ToLower(t.ask("\nChoose: ")) {
		case "1":
			t.report(cmdStatus(nil))
			t.pause()
		case "2":
			t.sessions()
		case "3":
			t.messages()
		case "4":
			t.scope()
		case "5":
			t.grace()
		case "6":
			fmt.Fprintln(out, "\nWatching. Press Ctrl+C to come back to the menu.")
			t.report(cmdWatch(nil))
		case "7":
			t.daemonMenu()
		case "8":
			cmdDoctor()
			t.pause()
		case "9":
			t.report(cmdLogs([]string{"-n", "40"}))
			t.pause()
		case "10":
			t.lifecycle()
		case "q", "quit", "exit", "0":
			return nil
		case "":
		default:
			fmt.Fprintln(out, "  Please choose a number from the list.")
		}
	}
}

func header(p core.Paths) string {
	mode := "not installed"
	if rec, ok := daemon.LoadInstall(p); ok {
		mode = "background mode"
		if rec.Standalone {
			mode = "standalone mode"
		}
	}
	run := "not running"
	if daemonRunning(p) {
		run = "running"
	}
	waiting := "nothing waiting"
	if jobs, err := store.LoadJobs(p.Jobs); err == nil {
		n, next := 0, time.Time{}
		for _, j := range jobs {
			if j.State == core.StatePending {
				n++
				if next.IsZero() || j.DueAt.Before(next) {
					next = j.DueAt
				}
			}
		}
		if n > 0 {
			waiting = fmt.Sprintf("%d resume(s) waiting, next at %s", n, next.Local().Format("15:04"))
		}
	}
	return fmt.Sprintf("Continuum %s · %s · %s · %s", core.Version, mode, run, waiting)
}

// ---- sessions ----------------------------------------------------------------

func (t *tui) sessions() {
	for {
		p := core.GetPaths()
		cfg, _ := core.LoadConfig(p.Config)
		r, _ := rules.Cached(p.Rules)
		sc, _ := scope.Load(p)
		m, _ := messages.Load(p)
		jobs, _ := store.LoadJobs(p.Jobs)
		list := knownSessions()
		if len(list) == 0 {
			fmt.Fprintln(t.out, "\nNo open or recent agent sessions found.")
			t.pause()
			return
		}
		fmt.Fprintln(t.out, "\nSessions:")
		tw := tabwriter.NewWriter(t.out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "  #\tAGENT\tNAME\tFOLDER\tCOVERED\tMESSAGE\tRESUME")
		for i, s := range list {
			j := &core.Job{Harness: s.Harness, SessionID: s.ID, Cwd: s.Cwd}
			ok, _ := sc.Covers(j)
			name := s.Name
			if name == "" {
				name = shortID(s.ID)
			}
			fmt.Fprintf(tw, "  %d\t%s\t%s\t%s\t%s\t%s\t%s\n", i+1, s.Harness, name, filepath.Base(s.Cwd),
				map[bool]string{true: "yes", false: "no"}[ok], m.Resolve(j, false, cfg, r).Source, resumeState(jobs, s))
		}
		tw.Flush()
		ans := t.ask("\nNumber to manage a session, Enter to go back: ")
		if ans == "" || ans == "q" {
			return
		}
		n, err := strconv.Atoi(ans)
		if err != nil || n < 1 || n > len(list) {
			fmt.Fprintln(t.out, "  Please choose a number from the list.")
			continue
		}
		t.session(list[n-1])
	}
}

func resumeState(jobs map[string]*core.Job, s knownSession) string {
	if j := jobs[s.Harness+":"+s.ID]; j != nil {
		switch j.State {
		case core.StatePending:
			return "at " + j.DueAt.Local().Format("15:04")
		case core.StateDelivered:
			return "resumed " + j.DeliveredAt.Local().Format("15:04")
		default:
			return strings.ReplaceAll(j.State, "_", " ")
		}
	}
	return "-"
}

func (t *tui) session(s knownSession) {
	id := s.ID
	for {
		label := s.Name
		if label == "" {
			label = shortID(s.ID)
		}
		fmt.Fprintf(t.out, "\n%s %s · %s · %s\n", s.Harness, shortID(s.ID), label, tildePath(s.Cwd))
		fmt.Fprintln(t.out, "  1  Choose it (for scope \"chosen\")     2  Stop choosing it")
		fmt.Fprintln(t.out, "  3  Exclude it (never resume)         4  Stop excluding it")
		fmt.Fprintln(t.out, "  5  Set its resume message            6  Clear its message")
		fmt.Fprintln(t.out, "  7  Show the message it would get")
		fmt.Fprintln(t.out, "  8  Resume it now                     9  Cancel its scheduled resume")
		switch t.ask("Choose (Enter to go back): ") {
		case "1":
			t.report(cmdChoose("chosen", []string{id}))
		case "2":
			t.report(cmdUnchoose("chosen", []string{id}))
		case "3":
			t.report(cmdChoose("excluded", []string{id}))
		case "4":
			t.report(cmdUnchoose("excluded", []string{id}))
		case "5":
			if text := t.ask("Message for this session ({agent} and {folder} work): "); text != "" && text != "q" {
				t.report(setMessage([]string{"--session", id, text}))
			}
		case "6":
			t.report(resetMessage([]string{"--session", id}))
		case "7":
			t.report(previewMessage([]string{id}))
		case "8":
			t.report(cmdControl("resume-now", []string{id}))
		case "9":
			t.report(cmdControl("cancel", []string{id}))
		case "", "q":
			return
		default:
			fmt.Fprintln(t.out, "  Please choose a number from the list.")
		}
	}
}

// ---- messages, scope, grace, install ---------------------------------------------

func (t *tui) messages() {
	for {
		fmt.Fprintln(t.out)
		t.report(showMessages())
		fmt.Fprintln(t.out)
		fmt.Fprintln(t.out, "  1  Set the default message        2  Set the default outage message")
		fmt.Fprintln(t.out, "  3  Set a message for an agent     4  Set a message for a folder")
		fmt.Fprintln(t.out, "  5  Reset one message              6  Reset all messages")
		fmt.Fprintln(t.out, "  (Per-session messages: menu 2, Sessions.)")
		switch t.ask("Choose (Enter to go back): ") {
		case "1":
			if text := t.ask("Default message: "); text != "" && text != "q" {
				t.report(setMessage([]string{text}))
			}
		case "2":
			if text := t.ask("Message after a temporary API error: "); text != "" && text != "q" {
				t.report(setMessage([]string{"--outage", text}))
			}
		case "3":
			agent := t.pickAgent()
			if agent == "" {
				continue
			}
			if text := t.ask("Message for " + agent + ": "); text != "" && text != "q" {
				t.report(setMessage([]string{"--agent", agent, text}))
			}
		case "4":
			folder := t.ask("Folder (e.g. ~/work/api or .): ")
			if folder == "" || folder == "q" {
				continue
			}
			if text := t.ask("Message for sessions in " + folder + ": "); text != "" && text != "q" {
				t.report(setMessage([]string{"--folder", folder, text}))
			}
		case "5":
			which := t.ask("Reset which? (default, an agent name, a folder path, or a session name/id): ")
			t.report(resetOne(which))
		case "6":
			if strings.HasPrefix(strings.ToLower(t.ask("Reset every message to the built-in one? [y/N] ")), "y") {
				t.report(resetMessage([]string{"--all"}))
			}
		case "", "q":
			return
		default:
			fmt.Fprintln(t.out, "  Please choose a number from the list.")
		}
	}
}

func resetOne(which string) error {
	p := core.GetPaths()
	r, _ := rules.Cached(p.Rules)
	switch {
	case which == "" || which == "q":
		return nil
	case which == "default":
		return resetMessage(nil)
	case r != nil && r.Harnesses[which] != nil:
		return resetMessage([]string{"--agent", which})
	case strings.HasPrefix(which, "/") || strings.HasPrefix(which, "~") || strings.HasPrefix(which, "."):
		return resetMessage([]string{"--folder", which})
	}
	return resetMessage([]string{"--session", which})
}

func (t *tui) pickAgent() string {
	p := core.GetPaths()
	r, _ := rules.Cached(p.Rules)
	names := []string{"claude", "codex"}
	for _, a := range shellHookAgents() {
		if a.Harness != "claude" && a.Harness != "codex" {
			names = append(names, a.Harness)
		}
	}
	for i, n := range names {
		fmt.Fprintf(t.out, "  %d  %s\n", i+1, r.Harness(n).DisplayName)
	}
	fmt.Fprintln(t.out, "  (or type any agent name, see `continuum rules --agents`)")
	ans := t.ask("Agent: ")
	if n, err := strconv.Atoi(ans); err == nil && n >= 1 && n <= len(names) {
		return names[n-1]
	}
	if ans == "q" {
		return ""
	}
	return ans
}

func (t *tui) scope() {
	fmt.Fprintln(t.out)
	t.report(cmdScope(nil))
	fmt.Fprintln(t.out, "\n  1  Resume every session   2  Resume chosen sessions only")
	fmt.Fprintln(t.out, "  (Choose or exclude sessions in menu 2, Sessions.)")
	switch t.ask("Choose (Enter to go back): ") {
	case "1":
		t.report(cmdScope([]string{"all"}))
		t.pause()
	case "2":
		t.report(cmdScope([]string{"chosen"}))
		t.pause()
	}
}

func (t *tui) grace() {
	fmt.Fprintln(t.out)
	t.report(cmdGrace(nil))
	ans := t.ask("\nChange it: type an agent and minutes, \"off\" or \"default\" (e.g. \"claude 10\"), or Enter to go back: ")
	if f := strings.Fields(ans); len(f) == 2 {
		t.report(cmdGrace(f))
		t.pause()
	} else if ans != "" && ans != "q" {
		fmt.Fprintln(t.out, "  Expected an agent and a value, e.g. \"claude off\".")
		t.pause()
	}
}

// daemonSummary: running or not, and whether it starts by itself.
func daemonSummary(p core.Paths) string {
	cfg, _ := core.LoadConfig(p.Config)
	run := "not running"
	if daemonRunning(p) {
		run = "running"
	}
	if rec, ok := daemon.LoadInstall(p); ok && rec.Standalone {
		return run + ", standalone"
	}
	if cfg.AutostartEnabled() {
		return run + ", auto-start on"
	}
	return run + ", auto-start off"
}

func (t *tui) daemonMenu() {
	for {
		p := core.GetPaths()
		cfg, _ := core.LoadConfig(p.Config)
		running := daemonRunning(p)
		fmt.Fprintf(t.out, "\n  Daemon: %s\n  Auto-start: %s\n\n", map[bool]string{true: "running", false: "not running"}[running], autostartState(p, cfg))
		if running {
			fmt.Fprintln(t.out, "  1  Stop it (until you start it again or log in)")
		} else {
			fmt.Fprintln(t.out, "  1  Start it")
		}
		auto := cfg.AutostartEnabled()
		if rec, ok := daemon.LoadInstall(p); ok && rec.Standalone {
			auto = false
		}
		if auto {
			fmt.Fprintln(t.out, "  2  Turn auto-start off (nothing starts it by itself; also stops it)")
		} else {
			fmt.Fprintln(t.out, "  2  Turn auto-start on (at login, and whenever an agent needs it)")
		}
		switch t.ask("Choose (Enter to go back): ") {
		case "1":
			if running {
				t.report(cmdStop())
			} else {
				t.report(cmdStart())
			}
		case "2":
			if auto {
				t.report(cmdDisable(nil))
			} else {
				t.report(cmdEnable(nil))
			}
		default:
			return
		}
	}
}

func (t *tui) lifecycle() {
	fmt.Fprintln(t.out, "\n  1  Install (shows what will change, then asks)")
	fmt.Fprintln(t.out, "  2  Update to this version")
	fmt.Fprintln(t.out, "  3  Uninstall (keeps your settings)")
	fmt.Fprintln(t.out, "  4  Uninstall and delete everything (--purge)")
	switch t.ask("Choose (Enter to go back): ") {
	case "1":
		t.report(cmdInstall(nil))
	case "2":
		t.report(cmdUpdate(nil))
	case "3":
		t.report(cmdUninstall(nil))
	case "4":
		t.report(cmdUninstall([]string{"--purge"}))
	default:
		return
	}
	t.pause()
}
