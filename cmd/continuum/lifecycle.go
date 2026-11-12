package main

// install, update and uninstall: Continuum's whole footprint outside its own
// folder, and how it is added, refreshed and removed.
//
//   outside ~/.continuum            added by       refreshed by        removed by
//   agent hooks (settings files)    install        update, the daemon  uninstall
//   shell hook block (rc files)     install        update              uninstall
//   login item (launchd/systemd/…)  install        update              uninstall
//   ~/.local/bin/continuum link     install        update              uninstall
//   backups *.continuum-backup-*    install        -                   uninstall --purge
//   ~/.continuum (binary, data)     install        -                   uninstall --purge

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/daemon"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/hooks"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/service"
)

type installOptions struct {
	Copy, Shell, Service, Dry bool
	Standalone                bool // no background daemon: `continuum watch` only
	Only                      string
	Update                    bool // re-applying a previous install
}

func cmdInstall(args []string) error {
	fs := newFlagSet("install")
	noService := fs.Bool("no-service", false, "Do not add the login item; the daemon is started now but not at the next login.")
	noCopy := fs.Bool("no-copy", false, "Use this binary where it is instead of copying it to ~/.continuum/bin.")
	dry := fs.Bool("dry-run", false, "Show what would change, and change nothing.")
	only := fs.String("only", "", "Comma-separated `AGENTS` to add hooks for, e.g. claude,codex (default: every installed agent that has hooks).")
	noShell := fs.Bool("no-shell", false, "Do not add the shell hook. Without it, agents in plain terminals (no tmux etc.) can only be resumed if started with `continuum run`.")
	standalone := fs.Bool("standalone", false, "Don't run in the background: no login item, and hooks never start a daemon. Continuum works while `continuum watch` is open in a terminal.")
	background := fs.Bool("background", false, "Run in the background, started at login (the default; skips the question).")
	yes := fs.Bool("yes", false, "Don't ask for confirmation; go ahead (for scripts). Also -y.")
	fs.BoolVar(yes, "y", false, "Same as --yes.")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *standalone && *background {
		return errors.New("choose either --standalone or --background")
	}
	o := installOptions{Copy: !*noCopy, Shell: !*noShell, Service: !*noService, Only: *only, Standalone: *standalone}
	if !*standalone && !*background && !*yes && !*dry && stdinIsTerminal() {
		s, err := askMode(os.Stdin)
		if err != nil {
			return err
		}
		o.Standalone = s
	}
	if o.Standalone {
		o.Service = false
	}
	return planAndInstall(o, *dry, *yes, "Continuum will make these changes:", "Install Continuum?")
}

// planAndInstall shows what install (or update) will do, asks, and does it.
func planAndInstall(o installOptions, dry, yes bool, title, question string) error {
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	r, _ := rules.Cached(p.Rules)
	printPlan(os.Stdout, title, installPlanItems(o, p, cfg, r), installFooter)
	if dry {
		fmt.Println("(dry run: nothing was changed)")
		return nil
	}
	if err := confirm(question, yes, os.Stdin); err != nil {
		if errors.Is(err, errDeclined) {
			fmt.Println("Cancelled. Nothing was changed.")
			return nil
		}
		return err
	}
	return doInstall(o)
}

func cmdUpdate(args []string) error {
	fs := newFlagSet("update")
	dry := fs.Bool("dry-run", false, "Show what would change, and change nothing.")
	yes := fs.Bool("yes", false, "Don't ask for confirmation; go ahead (for scripts). Also -y.")
	fs.BoolVar(yes, "y", false, "Same as --yes.")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	p := core.GetPaths()
	rec, ok := daemon.LoadInstall(p)
	if !ok {
		return errors.New("Continuum is not installed here; run `continuum install`")
	}
	o := installOptions{Copy: rec.Copy || rec.Version == "", Shell: rec.Shell || rec.Version == "", Service: (rec.Service || rec.Version == "") && !rec.Standalone, Only: rec.Only, Update: true, Standalone: rec.Standalone}
	title := fmt.Sprintf("Updating Continuum %s → %s, with the choices you made at install. These changes will be made:", orUnknown(rec.Version), core.Version)
	return planAndInstall(o, *dry, *yes, title, "Update Continuum?")
}

func orUnknown(v string) string {
	if v == "" {
		return "(earlier version)"
	}
	return v
}

func doInstall(o installOptions) error {
	p := core.GetPaths()
	if err := p.Ensure(); err != nil {
		return err
	}
	cfg, _ := core.LoadConfig(p.Config)
	r, _ := rules.Cached(p.Rules)
	bin := self()
	if o.Copy {
		name := "continuum"
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		dst := filepath.Join(p.Home, "bin", name)
		if !o.Dry {
			if err := copySelf(dst); err != nil {
				return fmt.Errorf("copy binary: %w", err)
			}
		}
		bin = dst
		fmt.Println("binary:   ", tildePath(bin))
		linkIntoPath(bin, o.Dry)
	}

	want := map[string]bool{}
	for _, h := range strings.Split(o.Only, ",") {
		if h = strings.TrimSpace(h); h != "" {
			want[h] = true
		}
	}
	var hooked []string
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
		before, _ := hooks.Status(t, bin)
		changed, err := hooks.Install(t, bin, o.Dry)
		if err != nil {
			fmt.Printf("%-9s %v\n", t.Harness+":", err)
			continue
		}
		hooked = append(hooked, t.Harness)
		verb := map[string]string{hooks.StateMissing: "hooks added to", hooks.StatePartial: "hooks completed in", hooks.StateOutdated: "hooks updated in"}[before]
		if !changed || verb == "" {
			verb = "hooks up to date in"
		} else if o.Dry {
			verb = "would have " + verb
		}
		fmt.Printf("%-9s %s %s\n", t.Harness+":", verb, tildePath(t.Path))
	}

	if o.Shell && runtime.GOOS != "windows" {
		for _, line := range installShell(bin, o.Dry) {
			fmt.Println(line)
		}
	}
	if !o.Dry {
		rec, _ := json.MarshalIndent(daemon.Install{
			Bin: bin, Hooks: hooked, At: time.Now(), Version: core.Version,
			Shell: o.Shell, Service: o.Service, Copy: o.Copy, Only: o.Only, Standalone: o.Standalone,
		}, "", "  ")
		_ = core.WriteFileAtomic(daemon.InstallPath(p), append(rec, '\n'), 0o600)
		// A daemon still running an older binary would keep old behaviour:
		// stop it so the new one takes over (or, in standalone mode, so
		// nothing runs in the background).
		if daemonRunning(p) {
			stopDaemon(p)
			if !o.Standalone {
				fmt.Println("daemon:    restarted with this version")
			} else {
				fmt.Println("daemon:    background daemon stopped (standalone mode)")
			}
		}
		if o.Standalone {
			_ = setConfigKey(p, "autostart_daemon", false)
			if service.Installed(p) {
				_ = service.Uninstall(p)
				fmt.Println("service:   login item removed (standalone mode)")
			}
		} else {
			_ = setConfigKey(p, "autostart_daemon", nil)
		}
	}
	if o.Standalone {
		// nothing runs in the background
	} else if o.Service {
		if o.Dry {
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
				where = "scheduled task Continuum"
			}
			fmt.Println("service:   starts at login via", tildePath(where))
		}
	} else if !o.Dry {
		_ = cmdStart()
	}
	if o.Dry {
		return nil
	}
	if o.Standalone {
		fmt.Println()
		fmt.Println("✅ Continuum is installed in standalone mode. Nothing runs in the background.")
		fmt.Println("   Run `continuum watch` in a terminal whenever you want stalled sessions")
		fmt.Println("   resumed; it shows what it does. Events are collected in the meantime.")
		return nil
	}
	fmt.Println()
	if o.Update {
		fmt.Printf("✅ Continuum is updated to %s. Covered:\n", core.Version)
	} else {
		fmt.Println("✅ Continuum is on. Covered from now on:")
	}
	for _, line := range coverageSummary(r) {
		fmt.Println("   • " + line)
	}
	fmt.Println()
	if !o.Update {
		fmt.Println("Nothing else to do. When a limit resets, the session continues by itself, in")
		fmt.Println("place. Open a new terminal tab to load the shell hook. Useful later:")
		fmt.Println("   continuum status      what's waiting      continuum doctor   full check")
		fmt.Println("   continuum scope       all or chosen        continuum -h       every command")
	}
	return nil
}

// stopDaemon stops a running daemon quietly.
func stopDaemon(p core.Paths) {
	data, err := os.ReadFile(p.PID)
	if err != nil {
		return
	}
	var pid int
	fmt.Sscan(strings.TrimSpace(string(data)), &pid)
	if pr, err := os.FindProcess(pid); err == nil && pid > 0 {
		if runtime.GOOS == "windows" {
			_ = pr.Kill()
		} else {
			_ = pr.Signal(os.Interrupt)
		}
	}
	for i := 0; i < 50 && daemonRunning(p); i++ {
		time.Sleep(100 * time.Millisecond)
	}
}

func cmdUninstall(args []string) error {
	fs := newFlagSet("uninstall")
	dry := fs.Bool("dry-run", false, "Show what would change, and change nothing.")
	purge := fs.Bool("purge", false, "Also delete ~/.continuum (the binary, settings, scope, history, logs) and the backups Continuum saved of your settings files. Without it, those are kept so a later install picks up where you left off.")
	yes := fs.Bool("yes", false, "Don't ask for confirmation; go ahead (for scripts). Also -y.")
	fs.BoolVar(yes, "y", false, "Same as --yes.")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	items := uninstallPlanItems(p, cfg, *purge)
	changes := 0
	for _, it := range items {
		if it.Action != "keep" && it.Action != "none" {
			changes++
		}
	}
	if changes == 0 {
		fmt.Println("Continuum isn't installed here: there is nothing to remove.")
		return nil
	}
	printPlan(os.Stdout, "Continuum will make these changes:", items, []string{"Agent sessions running right now keep working normally; they just won't be resumed after a limit any more."})
	if *dry {
		fmt.Println("(dry run: nothing was changed)")
		return nil
	}
	if err := confirm("Uninstall Continuum?", *yes, os.Stdin); err != nil {
		if errors.Is(err, errDeclined) {
			fmt.Println("Cancelled. Nothing was changed.")
			return nil
		}
		return err
	}
	say := func(what, msg string) { fmt.Printf("%-9s %s\n", what+":", msg) }
	would := func(s string) string {
		if *dry {
			return "would " + s
		}
		return s
	}

	// 1. Stop the daemon and remove the login item first, so nothing re-adds
	// what is removed next.
	if !*dry {
		if service.Installed(p) {
			if err := service.Uninstall(p); err != nil {
				say("service", err.Error())
			} else {
				say("service", "login item removed")
			}
		}
		if daemonRunning(p) {
			stopDaemon(p)
			say("daemon", "stopped")
		}
	}

	// 2. Agent hooks: every Continuum hook, in every event, any version.
	for _, t := range hooks.Targets(cfg) {
		changed, err := hooks.Uninstall(t, *dry)
		switch {
		case err != nil:
			say(t.Harness, err.Error())
		case changed:
			say(t.Harness, would("remove its hooks from ")+tildePath(t.Path))
		}
	}
	// 3. Shell hook blocks, in every shell's startup file.
	for _, line := range uninstallShell(*dry) {
		fmt.Println(line)
	}
	// 4. The ~/.local/bin link.
	home, _ := os.UserHomeDir()
	link := filepath.Join(home, ".local", "bin", "continuum")
	if target, err := os.Readlink(link); err == nil && strings.Contains(target, ".continuum") {
		if !*dry {
			os.Remove(link)
		}
		say("link", would("remove ")+tildePath(link))
	}
	if !*dry {
		os.Remove(daemon.InstallPath(p))
	}

	// 5. --purge: backups and Continuum's own folder.
	if *purge {
		var backups []string
		for _, t := range hooks.Targets(cfg) {
			backups = append(backups, hooks.Backups(t.Path)...)
		}
		for _, rc := range allRCs() {
			m, _ := filepath.Glob(rc.Path + ".continuum-backup-*")
			backups = append(backups, m...)
		}
		for _, b := range backups {
			if !*dry {
				os.Remove(b)
			}
		}
		if len(backups) > 0 {
			say("backups", would(fmt.Sprintf("delete %d backup(s) of your settings files", len(backups))))
		}
		if err := safeToPurge(p.Home); err != nil {
			say("data", "not deleted: "+err.Error())
		} else {
			if !*dry {
				if err := os.RemoveAll(p.Home); err != nil {
					say("data", err.Error())
				}
			}
			say("data", would("delete ")+tildePath(p.Home))
		}
	}
	if *dry {
		return nil
	}

	// 6. Check nothing is left behind.
	left := leftovers(p, cfg, *purge)
	fmt.Println()
	if len(left) == 0 {
		if *purge {
			fmt.Println("✅ Continuum is completely removed. Agent sessions still running under it keep")
			fmt.Println("   working normally; they just won't be resumed after a limit any more.")
		} else {
			fmt.Printf("✅ Continuum is uninstalled. Your settings and history are kept in %s\n", p.Home)
			fmt.Println("   (`continuum uninstall --purge` deletes them too).")
		}
		return nil
	}
	fmt.Println("Uninstalled, but these are still there:")
	for _, l := range left {
		fmt.Println("   • " + l)
	}
	return errors.New("some parts could not be removed")
}

// safeToPurge refuses to delete anything that doesn't look like Continuum's
// own folder.
func safeToPurge(dir string) error {
	clean := filepath.Clean(dir)
	home, _ := os.UserHomeDir()
	if clean == "/" || clean == filepath.Clean(home) || clean == "." || len(clean) < 4 {
		return fmt.Errorf("%s is not a Continuum folder", dir)
	}
	if _, err := os.Stat(clean); os.IsNotExist(err) {
		return nil
	}
	if filepath.Base(clean) == ".continuum" {
		return nil
	}
	for _, marker := range []string{"jobs.json", "offsets.json", "install.json", "config.json", "daemon.log", "spool"} {
		if _, err := os.Stat(filepath.Join(clean, marker)); err == nil {
			return nil
		}
	}
	return fmt.Errorf("%s doesn't contain Continuum's files", dir)
}

// leftovers lists anything of Continuum's still present after uninstalling.
func leftovers(p core.Paths, cfg core.Config, purged bool) []string {
	var out []string
	for _, t := range hooks.Targets(cfg) {
		if st, _ := hooks.Status(t, ""); st != hooks.StateMissing {
			out = append(out, "hooks in "+t.Path)
		}
	}
	for _, rc := range allRCs() {
		if data, err := os.ReadFile(rc.Path); err == nil && strings.Contains(string(data), blockStart) {
			out = append(out, "shell hook in "+rc.Path)
		}
	}
	if service.Installed(p) {
		out = append(out, "login item")
	}
	if daemonRunning(p) {
		out = append(out, "a running daemon")
	}
	home, _ := os.UserHomeDir()
	if target, err := os.Readlink(filepath.Join(home, ".local", "bin", "continuum")); err == nil && strings.Contains(target, ".continuum") {
		out = append(out, "~/.local/bin/continuum")
	}
	if purged {
		if _, err := os.Stat(p.Home); err == nil {
			out = append(out, p.Home)
		}
		for _, t := range hooks.Targets(cfg) {
			for _, b := range hooks.Backups(t.Path) {
				out = append(out, b)
			}
		}
	}
	return out
}

// askMode asks how Continuum should run. It returns true for standalone.
func askMode(in io.Reader) (bool, error) {
	fmt.Println("How should Continuum run?")
	fmt.Println("  1. In the background, started at login (recommended): sessions resume even")
	fmt.Println("     when no terminal is open.")
	fmt.Println("  2. Standalone: only while `continuum watch` is open in a terminal, showing")
	fmt.Println("     what it does. Nothing runs in the background.")
	fmt.Print("Choose 1 or 2 [1]: ")
	line, _ := bufio.NewReader(in).ReadString('\n')
	fmt.Println()
	switch strings.TrimSpace(line) {
	case "", "1":
		return false, nil
	case "2":
		return true, nil
	}
	return false, fmt.Errorf("please answer 1 or 2")
}
