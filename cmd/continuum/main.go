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
	"os/signal"
	"path/filepath"
	"runtime"
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
)

func main() {
	if len(os.Args) < 2 {
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
	case "start":
		err = cmdStart()
	case "stop":
		err = cmdStop()
	case "status":
		err = cmdStatus()
	case "list", "ls", "jobs":
		err = cmdList(args)
	case "cancel":
		err = cmdControl("cancel", args)
	case "resume-now", "now":
		err = cmdControl("resume-now", args)
	case "install":
		err = cmdInstall(args)
	case "uninstall":
		err = cmdUninstall(args)
	case "doctor":
		code = cmdDoctor()
	case "detect":
		err = cmdDetect(args)
	case "grace":
		err = cmdGrace(args)
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

func ensureDaemon() {
	defer func() { _ = recover() }()
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	if !cfg.AutostartEnabled() || daemonRunning(p) {
		return
	}
	_, _ = proc.StartDetached([]string{self(), "daemon"}, p.Home, deliver.CleanEnv(os.Environ()), p.Log)
}

func rotateLog(path string) {
	if st, err := os.Stat(path); err == nil && st.Size() > 5<<20 {
		_ = os.Rename(path, path+".1")
	}
}

func cmdDaemon(args []string) error {
	fs := newFlagSet("daemon")
	once := fs.Bool("once", false, "Run one pass (collect events, resume what is due) and exit.")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	p := core.GetPaths()
	if err := p.Ensure(); err != nil {
		return err
	}
	lock, err := store.Lock(p.Lock)
	if err != nil {
		if errors.Is(err, store.ErrLocked) {
			fmt.Println("continuum daemon is already running")
			return nil
		}
		return err
	}
	defer lock.Close()
	rotateLog(p.Log)
	_ = os.WriteFile(p.PID, []byte(strconv.Itoa(os.Getpid())), 0o600)
	defer os.Remove(p.PID)
	_ = os.WriteFile(p.BinRecord, []byte(self()), 0o600)

	d, err := daemon.New(p)
	if err != nil {
		return err
	}
	if *once {
		d.Tick()
		return nil
	}
	warnCapabilities(d)
	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; close(stop) }()
	d.Run(stop)
	return nil
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
				d.Logger.Printf("note: %s: %s", nc.Name, why)
			}
		}
		if !okAny {
			d.Logger.Printf("warning: %s has none of the resume commands Continuum knows; it will rely on panes and the wrapper", h.DisplayName)
		}
	}
}

func cmdStart() error {
	p := core.GetPaths()
	if err := p.Ensure(); err != nil {
		return err
	}
	if daemonRunning(p) {
		fmt.Println("continuum daemon is already running")
		return nil
	}
	pid, err := proc.StartDetached([]string{self(), "daemon"}, p.Home, deliver.CleanEnv(os.Environ()), p.Log)
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
	data, err := os.ReadFile(p.PID)
	if err != nil || !daemonRunning(p) {
		fmt.Println("continuum daemon is not running")
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
	fmt.Println("continuum daemon stopped")
	if service.Installed(p) {
		fmt.Println("(it starts again at login; `continuum uninstall` removes that)")
	}
	return nil
}

// ---- run ---------------------------------------------------------------------

func cmdRun(args []string) (int, error) {
	fs := newFlagSet("run")
	harness := fs.String("harness", "", "Treat the command as `AGENT`: claude, codex, gemini, ... or generic. Guessed from COMMAND when empty.")
	auto := fs.Bool("auto", false, "Wrap only interactive sessions; run anything else (print mode, --version, subcommands, pipes) directly. Used by the shell hook.")
	choose := fs.Bool("choose", false, "Add this session to the chosen sessions for as long as it runs (for `continuum scope chosen`). Same as CONTINUUM_CHOOSE=1.")
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
	return wrapper.Run(wrapper.Options{Harness: *harness, Argv: argv, Stdin: os.Stdin, Stdout: os.Stdout, Choose: *choose})
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

func cmdList(args []string) error {
	fs := newFlagSet("list")
	all := fs.Bool("all", false, "Include all finished resumes (default: pending ones and the last 24 hours).")
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
		var keep []*core.Job
		for _, j := range list {
			if j.State == core.StatePending || j.State == core.StateNeedsUser || time.Since(j.UpdatedAt) < 24*time.Hour {
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
	if len(args) != 1 {
		return fmt.Errorf("usage: continuum %s ID|all", action)
	}
	sel := args[0]
	if i := strings.IndexByte(sel, ':'); i >= 0 && !strings.Contains(sel, "@") {
		sel = sel[i+1:] // accept the harness:id form shown by list
	}
	p := core.GetPaths()
	if err := p.Ensure(); err != nil {
		return err
	}
	if daemonRunning(p) {
		if err := core.SpoolEvent(p, core.Event{Kind: "control", Message: action + " " + sel, Source: "cli"}); err != nil {
			return err
		}
		fmt.Printf("asked the daemon to %s %s\n", action, args[0])
		return nil
	}
	// No daemon: apply directly and start one so resume-now happens.
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
		return cmdStart()
	}
	return nil
}

func cmdStatus() error {
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	running := daemonRunning(p)
	fmt.Printf("Continuum %s\n", core.Version)
	fmt.Printf("  daemon:   %s\n", map[bool]string{true: "running", false: "not running (`continuum start`)"}[running])
	fmt.Printf("  at login: %s\n", map[bool]string{true: "yes", false: "no"}[service.Installed(p)])
	for _, t := range hooks.Targets(cfg) {
		ok, detail := hooks.Installed(t)
		state := "installed"
		if !ok {
			state = "not installed (" + detail + ")"
		}
		fmt.Printf("  %-8s  hooks %s\n", t.Harness+":", state)
	}
	if cfg.DryRun {
		fmt.Println("  dry run:  on (nothing is typed or run)")
	}
	fmt.Println()
	return cmdList(nil)
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
	return os.Rename(tmp, dst)
}

func cmdInstall(args []string) error {
	fs := newFlagSet("install")
	noService := fs.Bool("no-service", false, "Do not add the login item; the daemon is started now but not at the next login.")
	noCopy := fs.Bool("no-copy", false, "Use this binary where it is instead of copying it to ~/.continuum/bin.")
	dry := fs.Bool("dry-run", false, "Show what would change, and change nothing.")
	only := fs.String("only", "", "Comma-separated `AGENTS` to add hooks for, e.g. claude,codex (default: every installed agent that has hooks).")
	noShell := fs.Bool("no-shell", false, "Do not add the shell hook. Without it, agents in plain terminals (no tmux etc.) can only be resumed if started with `continuum run`.")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	p := core.GetPaths()
	if err := p.Ensure(); err != nil {
		return err
	}
	cfg, _ := core.LoadConfig(p.Config)
	bin := self()
	if !*noCopy {
		name := "continuum"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		dst := filepath.Join(p.Home, "bin", name)
		if !*dry {
			if err := copySelf(dst); err != nil {
				return fmt.Errorf("copy binary: %w", err)
			}
		}
		bin = dst
		fmt.Println("binary:   ", bin)
		linkIntoPath(bin, *dry)
	}
	want := map[string]bool{}
	for _, h := range strings.Split(*only, ",") {
		if h = strings.TrimSpace(h); h != "" {
			want[h] = true
		}
	}
	r, _ := rules.Cached(p.Rules)
	for _, t := range hooks.Targets(cfg) {
		if len(want) > 0 && !want[t.Harness] {
			continue
		}
		_, binErr := deliver.Binary(cfg, r.Harness(t.Harness))
		_, dirErr := os.Stat(filepath.Dir(t.Path))
		if binErr != nil && dirErr != nil && len(want) == 0 {
			fmt.Printf("%-9s not found, skipped\n", t.Harness+":")
			continue
		}
		changed, err := hooks.Install(t, bin, *dry)
		if err != nil {
			fmt.Printf("%-9s %v\n", t.Harness+":", err)
			continue
		}
		verb := "hooks already up to date in"
		if changed {
			verb = "hooks added to"
			if *dry {
				verb = "would add hooks to"
			}
		}
		fmt.Printf("%-9s %s %s\n", t.Harness+":", verb, t.Path)
	}
	if !*noShell && runtime.GOOS != "windows" {
		for _, line := range installShell(bin, *dry) {
			fmt.Println(line)
		}
	}
	if !*noService {
		if *dry {
			plan, err := service.MakePlan(bin, p)
			if err != nil {
				fmt.Println("service:  ", err)
			} else if plan.File != "" {
				fmt.Println("service:   would write", plan.File)
			} else {
				fmt.Println("service:   would run", strings.Join(plan.Commands[0], " "))
			}
		} else if plan, err := service.Install(bin, p); err != nil {
			fmt.Println("service:   not installed:", err)
			_ = cmdStart()
		} else {
			where := plan.File
			if where == "" {
				where = "scheduled task " + "Continuum"
			}
			fmt.Println("service:   starts at login via", where)
		}
	} else if !*dry {
		_ = cmdStart()
	}
	if *dry {
		return nil
	}
	fmt.Println()
	fmt.Println("Done. Agents started from now on are covered, in any terminal (open a new")
	fmt.Println("terminal tab for the shell hook). Already-running sessions are watched through")
	fmt.Println("their transcripts. Check with `continuum doctor`.")
	return nil
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
		fmt.Println("link:      would link", link)
		return
	}
	if err := os.Symlink(bin, link); err == nil {
		fmt.Println("link:     ", link)
	}
}

func cmdUninstall(args []string) error {
	fs := newFlagSet("uninstall")
	dry := fs.Bool("dry-run", false, "Show what would change, and change nothing.")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	for _, t := range hooks.Targets(cfg) {
		changed, err := hooks.Uninstall(t, *dry)
		switch {
		case err != nil:
			fmt.Printf("%-9s %v\n", t.Harness+":", err)
		case changed:
			fmt.Printf("%-9s hooks removed from %s\n", t.Harness+":", t.Path)
		}
	}
	for _, line := range uninstallShell(*dry) {
		fmt.Println(line)
	}
	if *dry {
		return nil
	}
	if err := service.Uninstall(p); err != nil {
		fmt.Println("service:  ", err)
	}
	_ = cmdStop()
	home, _ := os.UserHomeDir()
	link := filepath.Join(home, ".local", "bin", "continuum")
	if target, err := os.Readlink(link); err == nil && strings.Contains(target, ".continuum") {
		os.Remove(link)
	}
	fmt.Printf("Continuum is uninstalled. Its data is still in %s; delete that folder to remove everything.\n", p.Home)
	return nil
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
	found := false
	for _, n := range names {
		h := r.Harness(n)
		m := h.Classify(text, r.Defaults.TailLines)
		if m == nil {
			continue
		}
		found = true
		kind := m.Kind
		if h.NeverResume(m.Text) {
			kind = core.KindNeverResume + " (needs you)"
		}
		fmt.Printf("%-8s %s\n", n+":", kind)
		fmt.Printf("         matched: %s\n", strings.ReplaceAll(m.Text, "\n", " ⏎ "))
		if t, ok := resettime.Parse(m.Text, time.Now(), time.Local); ok {
			fmt.Printf("         resets:  %s (%s)\n", t.Local().Format("Mon 2 Jan 15:04:05 MST"), inDuration(time.Until(t)))
		} else {
			fmt.Println("         resets:  unknown (Continuum would retry with backoff)")
		}
	}
	if !found {
		fmt.Println("no limit message detected")
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
