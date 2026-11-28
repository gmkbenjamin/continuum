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

// option is one menu entry: its number, and its label with "&" before the
// letter that also picks it (shown as "[s]").
type option struct {
	num   string
	label string
}

// letter is the shortcut letter marked with "&" in the label.
func (o option) letter() string {
	if i := strings.IndexByte(o.label, '&'); i >= 0 && i+1 < len(o.label) {
		return strings.ToLower(o.label[i+1 : i+2])
	}
	return ""
}

// text is the label with its shortcut letter shown in brackets.
func (o option) text() string {
	i := strings.IndexByte(o.label, '&')
	if i < 0 || i+1 >= len(o.label) {
		return o.label
	}
	return o.label[:i] + "[" + o.label[i+1:i+2] + "]" + o.label[i+2:]
}

// showMenu prints the options in rows (one or two per row, aligned).
func (t *tui) showMenu(rows ...[]option) {
	tw := tabwriter.NewWriter(t.out, 0, 2, 3, ' ', 0)
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, o := range row {
			cells[i] = fmt.Sprintf("%-2s %s", o.num, o.text())
		}
		fmt.Fprintln(tw, "  "+strings.Join(cells, "\t"))
	}
	tw.Flush()
}

// choose asks with prompt and returns the number of the option picked by
// number or letter; anything else comes back lowercased as typed.
func (t *tui) choose(prompt string, rows ...[]option) string {
	ans := strings.ToLower(t.ask(prompt))
	for _, row := range rows {
		for _, o := range row {
			if ans == o.num || (ans != "" && ans == o.letter()) {
				return o.num
			}
		}
	}
	return ans
}

const pickHint = "  Please choose a number or letter from the list."

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
		menu := [][]option{
			{{"1", "&Status and scheduled resumes"}},
			{{"2", "S&essions: choose, exclude, set a message, resume now"}},
			{{"3", "Resume &messages"}},
			{{"4", "S&cope: which sessions are resumed (now: " + scopeNow + ")"}},
			{{"5", "Wait for an agent's own resume (&grace)"}},
			{{"6", "&Watch live in this terminal"}},
			{{"7", "Start, stop or turn off &auto-start (now: " + daemonSummary(p) + ")"}},
			{{"8", "Check everything (&doctor)"}},
			{{"9", "Recent &log"}},
			{{"10", "&Install, update or uninstall"}},
			{{"q", "Quit"}},
		}
		t.showMenu(menu...)
		switch t.choose("\nChoose: ", menu...) {
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
			fmt.Fprintln(out, pickHint)
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
		if !cfg.ClosedSessionsTracked() {
			var open []knownSession
			for _, s := range list {
				if s.Open {
					open = append(open, s)
				}
			}
			list = open
		}
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
		menu := [][]option{
			{{"1", "&Choose it (for scope \"chosen\")"}, {"2", "Stop c&hoosing it"}},
			{{"3", "&Exclude it (never resume)"}, {"4", "Stop e&xcluding it"}},
			{{"5", "&Set its resume message"}, {"6", "C&lear its message"}},
			{{"7", "Sho&w the message it would get"}},
			{{"8", "&Resume it now"}, {"9", "C&ancel its scheduled resume"}},
		}
		t.showMenu(menu...)
		switch t.choose("Choose (Enter to go back): ", menu...) {
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
			fmt.Fprintln(t.out, pickHint)
		}
	}
}

// ---- messages, scope, grace, install ---------------------------------------------

func (t *tui) messages() {
	for {
		fmt.Fprintln(t.out)
		t.report(showMessages())
		fmt.Fprintln(t.out)
		menu := [][]option{
			{{"1", "Set the &default message"}, {"2", "Set the default &outage message"}},
			{{"3", "Set a message for an &agent"}, {"4", "Set a message for a &folder"}},
			{{"5", "&Reset one message"}, {"6", "Reset a&ll messages"}},
		}
		t.showMenu(menu...)
		fmt.Fprintln(t.out, "  (Per-session messages: Sessions, 2 or e in the main menu.)")
		switch t.choose("Choose (Enter to go back): ", menu...) {
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
			fmt.Fprintln(t.out, pickHint)
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
	case isDirArg(which):
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
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	fmt.Fprintln(t.out)
	t.report(cmdScope(nil))
	t.report(cmdClosed(nil))
	t.report(cmdDraft(nil))
	closedOpt := option{"3", "Stop tracking c&losed sessions"}
	if !cfg.ClosedSessionsTracked() {
		closedOpt = option{"3", "Track c&losed sessions"}
	}
	draftOpt := option{"4", "Submit existing &draft text before resuming"}
	if cfg.DraftMode() == core.DraftSubmit {
		draftOpt = option{"4", "Restore existing &draft text after resuming"}
	}
	menu := []option{{"1", "Resume &every session"}, {"2", "Resume &chosen sessions only"}, closedOpt, draftOpt}
	fmt.Fprintln(t.out)
	t.showMenu(menu[:2], menu[2:])
	fmt.Fprintln(t.out, "  (Choose or exclude sessions in Sessions, 2 or e in the main menu.)")
	switch t.choose("Choose (Enter to go back): ", menu) {
	case "1":
		t.report(cmdScope([]string{"all"}))
		t.pause()
	case "2":
		t.report(cmdScope([]string{"chosen"}))
		t.pause()
	case "3":
		if cfg.ClosedSessionsTracked() {
			t.report(cmdClosed([]string{"off"}))
		} else {
			t.report(cmdClosed([]string{"on"}))
		}
		t.pause()
	case "4":
		if cfg.DraftMode() == core.DraftSubmit {
			t.report(cmdDraft([]string{"restore"}))
		} else {
			t.report(cmdDraft([]string{"submit"}))
		}
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
		first := option{"1", "&Start it"}
		if running {
			first = option{"1", "&Stop it (until you start it again or log in)"}
		}
		auto := cfg.AutostartEnabled()
		if rec, ok := daemon.LoadInstall(p); ok && rec.Standalone {
			auto = false
		}
		second := option{"2", "Turn &auto-start on (at login, and whenever an agent needs it)"}
		if auto {
			second = option{"2", "Turn &auto-start off (nothing starts it by itself; also stops it)"}
		}
		menu := [][]option{{first}, {second}}
		t.showMenu(menu...)
		switch t.choose("Choose (Enter to go back): ", menu...) {
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
	menu := [][]option{
		{{"1", "&Install (shows what will change, then asks)"}},
		{{"2", "&Update to this version"}},
		{{"3", "U&ninstall (keeps your settings)"}},
		{{"4", "Uninstall and delete everything (--&purge)"}},
	}
	fmt.Fprintln(t.out)
	t.showMenu(menu...)
	switch t.choose("Choose (Enter to go back): ", menu...) {
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
