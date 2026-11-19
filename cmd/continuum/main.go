// Command continuum keeps AI coding agents working through usage limits and
// transient API failures: it notices when Claude Code, Codex or another agent
// stalls, waits for the reset, and continues the session.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/daemon"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/hooks"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/resettime"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/service"
	"github.com/continuum-dev/continuum/internal/store"
	"github.com/continuum-dev/continuum/internal/wrapper"
	"golang.org/x/term"
)

func main() {
	if len(os.Args) < 2 {
		// In a terminal: the interactive menu. Otherwise (scripts, pipes):
		// the usage text.
		if stdinIsTerminal() && term.IsTerminal(int(os.Stdout.Fd())) {
			if err := cmdInteractive(nil); err != nil {
				fmt.Fprintln(os.Stderr, "continuum:", err)
				os.Exit(1)
			}
			os.Exit(0)
		}
		fmt.Print(usageText())
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch {
	case cmd == "help" || cmd == "--help" || cmd == "-h":
		if len(args) > 0 {
			helpFor(args[0])
		} else {
			fmt.Print(usageText())
		}
		os.Exit(0)
	case cmd != "hook" && cmd != "__pty-helper" && wantsHelp(cmd, args):
		helpFor(cmd)
		os.Exit(0)
	}
	var err error
	code := 0
	switch cmd {
	case "hook":
		// Must stay fast and silent, and always succeed.
		if len(args) > 0 {
			hooks.Handle(args[0], os.Stdin, os.Environ())
			ensureDaemon()
		}
		os.Exit(0)
	case "run":
		code, err = cmdRun(args)
	case "__pty-helper":
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		os.Exit(wrapper.Helper(args))
	case "daemon":
		err = cmdDaemon(args)
	case "watch":
		err = cmdWatch(args)
	case "logs", "log":
		err = cmdLogs(args)
	case "start":
		err = cmdStart()
	case "stop":
		err = cmdStop()
	case "this", "self":
		err = cmdThis(args)
	case "agent-guide":
		err = cmdAgentGuide(args)
	case "disable":
		err = cmdDisable(args)
	case "enable":
		err = cmdEnable(args)
	case "interactive", "menu", "i":
		err = cmdInteractive(args)
	case "status":
		err = cmdStatus(args)
	case "list", "ls", "jobs":
		err = cmdList(args)
	case "cancel":
		err = cmdControl("cancel", args)
	case "resume-now", "now":
		err = cmdControl("resume-now", args)
	case "install":
		err = cmdInstall(args)
	case "update":
		err = cmdUpdate(args)
	case "uninstall":
		err = cmdUninstall(args)
	case "diagnose":
		err = cmdDiagnose(args)
	case "doctor":
		code = cmdDoctor()
	case "detect":
		err = cmdDetect(args)
	case "grace":
		err = cmdGrace(args)
	case "message", "messages", "msg":
		err = cmdMessage(args)
	case "scope":
		err = cmdScope(args)
	case "choose":
		err = cmdChoose("chosen", args)
	case "exclude":
		err = cmdChoose("excluded", args)
	case "unchoose":
		err = cmdUnchoose("chosen", args)
	case "include":
		err = cmdUnchoose("excluded", args)
	case "sessions":
		err = cmdSessions(args)
	case "shell-init":
		err = cmdShellInit(args)
	case "rules":
		err = cmdRules(args)
	case "version", "--version", "-v":
		fmt.Println("continuum", core.Version, runtime.GOOS+"/"+runtime.GOARCH)
	default:
		fmt.Fprintf(os.Stderr, "continuum: unknown command %q (run `continuum -h` for the list)\n", cmd)
		os.Exit(2)
	}
	if errors.Is(err, errHelpShown) {
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "continuum:", err)
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func self() string {
	exe, err := os.Executable()
	if err != nil {
		return "continuum"
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		return r
	}
	return exe
}

// ---- daemon lifecycle --------------------------------------------------------

func daemonRunning(p core.Paths) bool {
	f, err := store.Lock(p.Lock)
	if err != nil {
		return errors.Is(err, store.ErrLocked)
	}
	f.Close()
	return false
}

func cmdDaemon(args []string) error {
	fs := newFlagSet("daemon")
	once := fs.Bool("once", false, "Run one pass (collect events, resume what is due) and exit.")
	debug := fs.Bool("debug", false, "Log every decision (events, checks, delivery routes) to daemon.log, and to this terminal if there is one.")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	return runDaemon(daemonOptions{Once: *once, Debug: *debug})
}

// warnCapabilities logs when an installed agent no longer offers a command
// Continuum relies on, so an agent update shows up in the log, not as a
// silent failure.
func warnCapabilities(d *daemon.Daemon) {
	for _, name := range d.Rules.HarnessNames() {
		h := d.Rules.Harness(name)
		if h.Binary == "" || len(h.NativeCommands) == 0 {
			continue
		}
		bin, err := deliver.Binary(d.Config, h)
		if err != nil {
			continue
		}
		okAny := false
		for _, nc := range h.NativeCommands {
			if ok, why := deliver.Supported(bin, nc); ok {
				okAny = true
			} else {
				d.Log.Infof("note: %s: %s", nc.Name, why)
			}
		}
		if !okAny {
			d.Log.Warnf("%s has none of the resume commands Continuum knows; it will rely on panes and the wrapper", h.DisplayName)
		}
	}
}

func cmdStart() error {
	p := core.GetPaths()
	if err := p.Ensure(); err != nil {
		return err
	}
	_ = os.Remove(stoppedPath(p))
	if daemonRunning(p) {
		fmt.Println("continuum daemon is already running")
		return nil
	}
	pid, err := proc.StartDetached([]string{self(), "daemon"}, p.Home, deliver.CleanEnv(os.Environ()), p.Home+"/daemon.stderr.log")
	if err != nil {
		return err
	}
	for i := 0; i < 30; i++ {
		if daemonRunning(p) {
			fmt.Printf("continuum daemon started (pid %d)\n", pid)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not start; see %s", p.Log)
}

func cmdStop() error {
	p := core.GetPaths()
	if err := p.Ensure(); err == nil {
		// Keep hooks from starting it again until `continuum start` or the
		// next login.
		_ = os.WriteFile(stoppedPath(p), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o600)
	}
	data, err := os.ReadFile(p.PID)
	if err != nil || !daemonRunning(p) {
		fmt.Println("continuum daemon is not running (it won't be started again until `continuum start`)")
		return nil
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	pr, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		err = pr.Kill()
	} else {
		err = pr.Signal(syscall.SIGTERM)
	}
	if err != nil {
		return err
	}
	for i := 0; i < 50 && daemonRunning(p); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Println("continuum daemon stopped; `continuum start` starts it again")
	if service.Installed(p) {
		fmt.Println("(it also starts again at your next login; `continuum disable` stops that)")
	}
	return nil
}

// ---- run ---------------------------------------------------------------------

func cmdRun(args []string) (int, error) {
	fs := newFlagSet("run")
	harness := fs.String("harness", "", "Treat the command as `AGENT`: claude, codex, gemini, ... or generic. Guessed from COMMAND when empty.")
	auto := fs.Bool("auto", false, "Wrap only interactive sessions; run anything else (print mode, --version, subcommands, pipes) directly. Used by the shell hook.")
	choose := fs.Bool("choose", false, "Add this session to the chosen sessions for as long as it runs (for `continuum scope chosen`). Same as CONTINUUM_CHOOSE=1.")
	message := fs.String("message", "", "Use `TEXT` as the resume message for this session only. Same as CONTINUUM_MESSAGE.")
	if err := parseFlags(fs, args); err != nil {
		return 2, err
	}
	argv := fs.Args()
	if len(argv) == 0 {
		return 2, errors.New("usage: continuum run [--harness NAME] -- COMMAND [ARGS...]")
	}
	if *auto && !wantWrap(*harness, argv) {
		return 127, proc.Exec(argv)
	}
	ensureDaemon()
	return wrapper.Run(wrapper.Options{Harness: *harness, Argv: argv, Stdin: os.Stdin, Stdout: os.Stdout, Choose: *choose, Message: *message})
}

// wantWrap decides, for the shell hook, whether an invocation is an
// interactive agent session worth wrapping.
func wantWrap(harness string, argv []string) bool {
	if os.Getenv("CONTINUUM_WRAPPER_ID") != "" || os.Getenv("CONTINUUM_NO_WRAP") != "" {
		return false
	}
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	if !cfg.IsEnabled() {
		return false
	}
	r, _ := rules.Cached(p.Rules)
	if r == nil {
		return false
	}
	if harness == "" {
		harness = wrapper.DetectHarness(r, argv)
	}
	if !cfg.HarnessEnabled(harness) {
		return false
	}
	return r.Harness(harness).Wrap.Wants(argv[1:])
}

// ---- status / list / control ---------------------------------------------------

func describeDue(j *core.Job) string {
	switch j.State {
	case core.StatePending:
		until := time.Until(j.DueAt).Round(time.Minute)
		s := j.DueAt.Local().Format("Mon 15:04")
		if until > 0 {
			s += " (in " + strings.TrimSuffix(until.String(), "0s") + ")"
		} else {
			s += " (now)"
		}
		if !j.ResetKnown {
			s += " ~retry"
		}
		return s
	default:
		r := j.Result
		if len(r) > 70 {
			r = r[:70] + "…"
		}
		return r
	}
}

func shortKey(j *core.Job) string {
	k := j.Key
	if j.SessionID != "" {
		id := j.SessionID
		if len(id) > 8 {
			id = id[:8]
		}
		k = j.Harness + ":" + id
	}
	return k
}

func jobSessionOpen(p core.Paths, j *core.Job, openSessions map[string]bool) bool {
	if j == nil {
		return false
	}
	if j.SessionID != "" {
		return openSessions[j.Harness+":"+j.SessionID]
	}
	if j.Location != nil && j.Location.Kind == "wrapper" {
		return deliver.WrapperAlive(p, j.Location.Target)
	}
	return false
}

func cmdList(args []string) error {
	fs := newFlagSet("list")
	all := fs.Bool("all", false, "Include finished resumes for closed sessions (default: active resumes and open sessions).")
	asJSON := fs.Bool("json", false, "Print JSON instead of a table (for scripts and other apps).")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	p := core.GetPaths()
	jobs, err := store.LoadJobs(p.Jobs)
	if err != nil {
		return err
	}
	list := store.SortedJobs(jobs)
	if !*all {
		openSessions := map[string]bool{}
		for _, s := range knownSessions() {
			if s.Open {
				openSessions[s.Harness+":"+s.ID] = true
			}
		}
		var keep []*core.Job
		for _, j := range list {
			if j.State == core.StatePending || j.State == core.StateNeedsUser {
				keep = append(keep, j)
				continue
			}
			if time.Since(j.UpdatedAt) < 24*time.Hour && jobSessionOpen(p, j, openSessions) {
				keep = append(keep, j)
			}
		}
		list = keep
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(list)
	}
	if len(list) == 0 {
		fmt.Println("No scheduled resumes.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATE\tKIND\tWHEN / RESULT\tWHERE\tDIR")
	for _, j := range list {
		where := j.Location.String()
		if where == "-" && j.SessionID != "" {
			where = "agent CLI"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", shortKey(j), j.State, strings.ReplaceAll(j.Kind, "_", " "), describeDue(j), where, filepath.Base(j.Cwd))
	}
	return tw.Flush()
}

func cmdControl(action string, args []string) error {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return fmt.Errorf("usage: continuum %s ID|all", action)
	}
	sel := args[0]
	sels := []string{sel}
	if isThis(sel) {
		h, err := thisSession()
		if err != nil {
			return err
		}
		sels = h.selectors()
		sel = h.selector()
	}
	if sel == "" {
		return fmt.Errorf("usage: continuum %s ID|all", action)
	}
	p := core.GetPaths()
	if err := p.Ensure(); err != nil {
		return err
	}
	if sel != "all" {
		jobs, _ := store.LoadJobs(p.Jobs)
		var keys []string
		sessions := map[string]bool{}
		matchedSel := ""
		for _, s := range sels {
			for _, j := range jobs {
				id := j.Key
				if j.SessionID != "" {
					id = j.Harness + "\x00" + j.SessionID
				}
				if daemon.SelectorMatches(j, s) {
					if matchedSel == "" {
						matchedSel = s
					}
					if !sessions[id] {
						sessions[id] = true
						keys = append(keys, shortKey(j))
					}
				}
			}
		}
		if matchedSel != "" {
			sel = matchedSel
		}
		if len(keys) > 1 {
			sort.Strings(keys)
			return fmt.Errorf("%q matches more than one session (%s); give more of the id", args[0], strings.Join(keys, ", "))
		}
		if len(keys) == 0 {
			fmt.Printf("note: no resume matches %q yet (see `continuum list`)\n", args[0])
		}
	}
	if daemonRunning(p) {
		if err := core.SpoolEvent(p, core.Event{Kind: "control", Message: action + " " + sel, Source: "cli"}); err != nil {
			return err
		}
		fmt.Printf("asked Continuum to %s %s\n", action, args[0])
		return nil
	}
	// Nothing running: apply the change to the saved state directly.
	d, err := daemon.New(p)
	if err != nil {
		return err
	}
	d.Handle(core.Event{Kind: "control", Message: action + " " + sel})
	if err := store.SaveJSON(p.Jobs, d.Jobs); err != nil {
		return err
	}
	fmt.Printf("%s %s: done\n", action, args[0])
	if action == "resume-now" {
		if rec, inst := daemon.LoadInstall(p); inst && rec.Standalone {
			fmt.Println("Continuum runs standalone: it resumes as soon as `continuum watch` is running.")
			return nil
		}
		if stoppedByUser(p) {
			fmt.Println("Continuum is stopped: it resumes once you run `continuum start` (or `continuum watch`).")
			return nil
		}
		return cmdStart()
	}
	return nil
}

func cmdStatus(args []string) error {
	fs := newFlagSet("status")
	all := fs.Bool("all", false, "Also list sessions closed in the last 3 days (default: only open sessions and pending resumes).")
	asJSON := fs.Bool("json", false, "Print the sessions as JSON (for scripts and other apps).")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	var window time.Duration
	if *all {
		window = 3 * 24 * time.Hour
	}
	rows := sessionRows(window)
	if *asJSON {
		return printRowsJSON(rows)
	}
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	running := daemonRunning(p)
	fmt.Printf("Continuum %s\n", core.Version)
	rec, installed := daemon.LoadInstall(p)
	switch {
	case installed && rec.Standalone:
		fmt.Printf("  mode:       standalone (runs while `continuum watch` is open)\n")
		fmt.Printf("  running:    %s\n", map[bool]string{true: "yes", false: "no: run `continuum watch` to resume stalled sessions"}[running])
	default:
		daemonState := "not running (`continuum start`)"
		switch {
		case running:
			daemonState = "running (`continuum logs -f` to follow it)"
		case stoppedByUser(p) && cfg.AutostartEnabled() && serviceInstalled(p):
			daemonState = "stopped by you (`continuum start`; it also starts at your next login)"
		case stoppedByUser(p):
			daemonState = "stopped by you (`continuum start`)"
		}
		fmt.Printf("  mode:       background\n")
		fmt.Printf("  daemon:     %s\n", daemonState)
		fmt.Printf("  auto-start: %s\n", autostartState(p, cfg))
	}
	for _, t := range hooks.Targets(cfg) {
		ok, detail := hooks.Installed(t)
		state := "installed"
		if !ok {
			state = "not installed (" + detail + ")"
		}
		fmt.Printf("  %-11s hooks %s\n", t.Harness+":", state)
	}
	if cfg.DryRun {
		fmt.Println("  dry run:    on (nothing is typed or run)")
	}
	if cfg.Debug() {
		fmt.Printf("  logging:    debug (%s)\n", tildePath(p.Log))
	}
	fmt.Println()
	if len(rows) == 0 {
		fmt.Println("No open agent sessions right now.")
	} else {
		fmt.Println("Sessions:")
		printSessionTable(os.Stdout, rows, false)
	}
	// Resumes not tied to a listed session (e.g. found on screen in a pane).
	jobs, _ := store.LoadJobs(p.Jobs)
	listed := map[string]bool{}
	listedWrappers := map[string]bool{}
	for _, r := range rows {
		if r.ID != "" {
			listed[r.ID] = true
		}
		if r.Wrapper != "" {
			listedWrappers[r.Wrapper] = true
		}
	}
	var extra []*core.Job
	for _, j := range store.SortedJobs(jobs) {
		if j.State != core.StatePending && j.State != core.StateNeedsUser {
			continue
		}
		if j.SessionID != "" && listed[j.SessionID] {
			continue
		}
		if j.Location != nil && j.Location.Kind == "wrapper" && listedWrappers[j.Location.Target] {
			continue
		}
		extra = append(extra, j)
	}
	if len(extra) > 0 {
		fmt.Println("\nOther resumes:")
		for _, j := range extra {
			fmt.Printf("  %s  %s  %s  %s\n", shortKey(j), j.State, describeDue(j), j.Location.String())
		}
	}
	fmt.Println("\n`continuum status --all` also lists closed sessions; `continuum list` shows resume history.")
	return nil
}

// ---- install -------------------------------------------------------------------

func copySelf(dst string) error {
	src := self()
	if filepath.Clean(src) == filepath.Clean(dst) {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".new"
	_ = os.Remove(tmp)
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		// Windows can't replace a program that is running (the daemon), but
		// it can rename it: move it aside first.
		old := dst + ".old"
		_ = os.Remove(old)
		if _, err := os.Stat(dst); err == nil {
			if err := os.Rename(dst, old); err != nil {
				return err
			}
		}
	}
	return os.Rename(tmp, dst)
}

// linkIntoPath puts a `continuum` symlink in ~/.local/bin when that directory
// is on PATH and the name is free.
func linkIntoPath(bin string, dry bool) {
	if runtime.GOOS == "windows" {
		return
	}
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".local", "bin")
	if !strings.Contains(string(os.PathListSeparator)+os.Getenv("PATH")+string(os.PathListSeparator), string(os.PathListSeparator)+dir+string(os.PathListSeparator)) {
		return
	}
	link := filepath.Join(dir, "continuum")
	if target, err := os.Readlink(link); err == nil {
		if target == bin {
			return
		}
		if !strings.Contains(target, ".continuum") {
			return // someone else's continuum
		}
		if !dry {
			os.Remove(link)
		}
	} else if _, err := os.Lstat(link); err == nil {
		return
	}
	if dry {
		fmt.Println("link:      would link", tildePath(link))
		return
	}
	// The folder is on PATH but may not exist yet.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Println("link:      not created:", err)
		return
	}
	if err := os.Symlink(bin, link); err != nil {
		fmt.Println("link:      not created:", err)
	} else {
		fmt.Println("link:     ", tildePath(link))
	}
}

// ---- detect / rules --------------------------------------------------------------

func cmdDetect(args []string) error {
	fs := newFlagSet("detect")
	harness := fs.String("harness", "", "Only check the rules of `AGENT` (claude, codex, gemini, ...).")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	var text string
	if fs.NArg() == 0 || fs.Arg(0) == "-" {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		text = string(data)
	} else {
		text = strings.Join(fs.Args(), " ")
	}
	p := core.GetPaths()
	r, err := rules.Cached(p.Rules)
	if r == nil {
		return err
	}
	if err != nil {
		fmt.Println("rules warning:", err)
	}
	names := r.HarnessNames()
	if *harness != "" {
		names = []string{*harness}
	}
	// Agents that read the message the same way are listed together.
	type verdict struct{ kind, matched, resets string }
	var order []verdict
	who := map[verdict][]string{}
	missed := 0
	for _, n := range names {
		h := r.Harness(n)
		m := h.Classify(text, r.Defaults.TailLines)
		if m == nil && h.NeverResume(text) {
			// Billing or sign-in wording without a limit phrase: also never resumed.
			m = &rules.Match{Kind: core.KindNeverResume, Text: strings.TrimSpace(text)}
		}
		if m == nil {
			missed++
			continue
		}
		v := verdict{kind: m.Kind, matched: strings.ReplaceAll(m.Text, "\n", " ⏎ "), resets: "unknown (Continuum would retry with backoff)"}
		if m.Kind == core.KindNeverResume || h.NeverResume(m.Text) {
			v.kind = core.KindNeverResume + " (needs you)"
			v.resets = "never retried: it needs you (billing, credits, sign-in)"
		} else if t, ok := resettime.Parse(m.Text, time.Now(), time.Local); ok {
			v.resets = t.Local().Format("Mon 2 Jan 15:04:05 MST") + " (" + inDuration(time.Until(t)) + ")"
		}
		if who[v] == nil {
			order = append(order, v)
		}
		who[v] = append(who[v], n)
	}
	if len(order) == 0 {
		fmt.Println("no limit message detected")
		return nil
	}
	for _, v := range order {
		agents := strings.Join(who[v], ", ")
		if len(who[v]) == len(names) && len(names) > 1 {
			agents = "every agent"
		}
		fmt.Printf("%s: %s\n", agents, v.kind)
		fmt.Printf("  matched: %s\n", v.matched)
		fmt.Printf("  resets:  %s\n", v.resets)
	}
	if missed > 0 && *harness == "" {
		fmt.Printf("(%d other agents don't read it as a limit)\n", missed)
	}
	return nil
}

func cmdRules(args []string) error {
	fs := newFlagSet("rules")
	defaults := fs.Bool("defaults", false, "Print the built-in rules, without your overrides (a starting point for ~/.continuum/rules.json).")
	agents := fs.Bool("agents", false, "List the supported agents as a table: command, how Continuum recognises it, how it resumes it.")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *defaults {
		_, err := os.Stdout.Write(rules.DefaultJSON())
		return err
	}
	p := core.GetPaths()
	r, err := rules.Load(p.Rules)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning:", err)
	}
	if *agents {
		printAgentTable(r)
		return nil
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}

// inDuration describes a wait: "in 45s", "in 2h13m", "now".
func inDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "now"
	case d < time.Minute:
		return "in " + d.Round(time.Second).String()
	}
	return "in " + strings.TrimSuffix(d.Round(time.Minute).String(), "0s")
}

// helpFor prints a command's help, including its real options.
func helpFor(name string) {
	e := findHelp(name)
	if e != nil && flagCommands[e.Name] {
		var err error
		switch e.Name {
		case "install":
			err = cmdInstall([]string{"-h"})
		case "uninstall":
			err = cmdUninstall([]string{"-h"})
		case "update":
			err = cmdUpdate([]string{"-h"})
		case "watch":
			err = cmdWatch([]string{"-h"})
		case "logs":
			err = cmdLogs([]string{"-h"})
		case "diagnose":
			err = cmdDiagnose([]string{"-h"})
		case "status":
			err = cmdStatus([]string{"-h"})
		case "sessions":
			err = cmdSessions([]string{"-h"})
		case "run":
			_, err = cmdRun([]string{"-h"})
		case "list":
			err = cmdList([]string{"-h"})
		case "detect":
			err = cmdDetect([]string{"-h"})
		case "rules":
			err = cmdRules([]string{"-h"})
		case "daemon":
			err = cmdDaemon([]string{"-h"})
		case "this":
			err = cmdThis([]string{"-h"})
		case "agent-guide":
			err = cmdAgentGuide([]string{"-h"})
		case "disable":
			err = cmdDisable([]string{"-h"})
		case "enable":
			err = cmdEnable([]string{"-h"})
		}
		if errors.Is(err, errHelpShown) {
			return
		}
	}
	printHelp(os.Stdout, name, nil)
}

// printAgentTable lists the supported agents as a Markdown table.
func printAgentTable(r *rules.Rules) {
	fmt.Println("| Agent | Command | Recognised by | Shell hook | Resumes |")
	fmt.Println("|---|---|---|---|---|")
	for _, name := range r.HarnessNames() {
		h := r.Harness(name)
		if name == "generic" {
			continue
		}
		var by []string
		switch name {
		case "claude":
			by = append(by, "hooks", "transcripts", "session list")
		case "codex":
			by = append(by, "hooks", "rollout files", "process scan")
		case "grok", "muse":
			by = append(by, "hooks", "session list")
		case "cursor":
			by = append(by, "hooks")
		}
		if len(h.ProcessNames) > 0 || len(h.ProcessArgPatterns) > 0 {
			by = append(by, "screen (wrapper, tmux)")
		}
		hook := "default"
		if !h.InShellHook() {
			hook = "opt-in"
		}
		resumes := "in place (wrapper, multiplexer pane)"
		if len(h.NativeCommands) > 0 {
			resumes += "; closed sessions by id"
		}
		cmdName := "`" + h.Binary + "`"
		for _, a := range h.CommandAliases {
			cmdName += " or `" + a.Command + "`"
		}
		if name == "rovodev" {
			cmdName = "`acli rovodev`"
		}
		disp := h.DisplayName
		if h.Homepage != "" {
			disp = "[" + disp + "](" + h.Homepage + ")"
		}
		fmt.Printf("| %s | %s | %s | %s | %s |\n", disp, cmdName, strings.Join(by, ", "), hook, resumes)
	}
	g := r.Harness("generic")
	fmt.Printf("\nAny other program in a tmux pane named %s is watched with the common patterns.\n", "`"+strings.Join(g.ProcessNames, "`, `")+"`")
}

// coverageSummary describes, in plain words, what is covered on this machine.
func coverageSummary(r *rules.Rules) []string {
	var out []string
	procs, _ := proc.List()
	for _, name := range []string{"claude", "codex"} {
		h := r.Harness(name)
		if _, err := proc.LookPath(h.Binary); err != nil {
			continue
		}
		line := h.DisplayName + " in any terminal"
		for _, a := range h.DesktopApps {
			installed := false
			for _, pr := range procs {
				if a.MatchesProcess(pr.Line) {
					installed = true
				}
			}
			if installed || appInstalled(a) {
				line += ", and in " + a.Name
			}
		}
		out = append(out, line)
	}
	var others []string
	listed := map[string]bool{}
	for _, a := range shellHookAgents() {
		// One agent may be wrapped under two commands (cursor-agent, agent).
		if a.Harness != "claude" && a.Harness != "codex" && !listed[a.Harness] {
			listed[a.Harness] = true
			others = append(others, r.Harness(a.Harness).DisplayName)
		}
	}
	if len(others) > 0 {
		out = append(out, strings.Join(others, ", ")+" in any terminal")
	}
	out = append(out, "tmux, screen, zellij, herdr, Rex and plain terminals; minimised, unfocused or locked")
	return out
}

// appInstalled reports whether a desktop app is installed (macOS): any app
// bundle its process patterns name, e.g. "/Claude\\.app/" or "/ChatGPT\\.app/".
func appInstalled(a *rules.DesktopApp) bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	re := regexp.MustCompile(`/([A-Za-z0-9 ]+)\\\\?\.app/`)
	for _, p := range a.ProcessPatterns {
		for _, m := range re.FindAllStringSubmatch(p, -1) {
			for _, dir := range []string{"/Applications", filepath.Join(os.Getenv("HOME"), "Applications")} {
				if _, err := os.Stat(filepath.Join(dir, m[1]+".app")); err == nil {
					return true
				}
			}
		}
	}
	return false
}
