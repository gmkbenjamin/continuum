package main

// Plans: before install, update or uninstall change anything, they list every
// file and item they will create, change or remove, where it is and what it is
// for, and ask the user to confirm.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/daemon"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/hooks"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/service"
	"golang.org/x/term"
)

// planItem is one thing a plan creates, changes, removes or keeps.
type planItem struct {
	Action  string // create, replace, change, remove, keep, none, start, stop
	What    string // short name
	Path    string
	Purpose string
	Notes   []string
}

var actionWords = map[string]string{
	"create": "will create", "replace": "will replace", "change": "will change",
	"remove": "will remove", "keep": "will keep", "none": "no change",
	"start": "will start", "stop": "will stop",
}

func printPlan(w io.Writer, title string, items []planItem, footer []string) {
	fmt.Fprintln(w, title)
	fmt.Fprintln(w)
	n := 0
	for _, it := range items {
		n++
		fmt.Fprintf(w, "%2d. %s: %s\n", n, it.What, actionWords[it.Action])
		if it.Path != "" {
			fmt.Fprintf(w, "    where:   %s\n", tildePath(it.Path))
		}
		if it.Purpose != "" {
			fmt.Fprintf(w, "    purpose: %s\n", wrapText(it.Purpose, 13, 88))
		}
		for _, note := range it.Notes {
			fmt.Fprintf(w, "             %s\n", wrapText(note, 13, 88))
		}
	}
	if len(footer) > 0 {
		fmt.Fprintln(w)
		for _, f := range footer {
			fmt.Fprintln(w, f)
		}
	}
	fmt.Fprintln(w)
}

func tildePath(p string) string {
	home, _ := os.UserHomeDir()
	if home != "" && (p == home || strings.HasPrefix(p, home+string(os.PathSeparator))) {
		return "~" + p[len(home):]
	}
	return p
}

// stdinReader is the one buffered reader of the terminal, shared by every
// prompt (menus, confirmations), so no prompt swallows another's input.
var stdinReader = bufio.NewReader(os.Stdin)

func lineReader(in io.Reader) *bufio.Reader {
	if in == os.Stdin {
		return stdinReader
	}
	if br, ok := in.(*bufio.Reader); ok {
		return br
	}
	return bufio.NewReader(in)
}

// errDeclined means the user answered no.
var errDeclined = errors.New("nothing was changed")

// stdinIsTerminal is replaceable in tests.
var stdinIsTerminal = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// confirm asks a yes/no question (default no). Without a terminal to ask in,
// it refuses unless --yes was given, so scripts never change things silently.
func confirm(question string, yes bool, in io.Reader) error {
	if yes {
		return nil
	}
	if !stdinIsTerminal() {
		return errors.New("no terminal to ask for confirmation; re-run with --yes to proceed")
	}
	fmt.Printf("%s [y/N] ", question)
	line, _ := lineReader(in).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		fmt.Println()
		return nil
	}
	return errDeclined
}

// linkPlan says what would happen to the ~/.local/bin/continuum link:
// "create", "replace", "none", or "" when no link is made.
func linkPlan(bin string) (string, string) {
	if runtime.GOOS == "windows" {
		return "", ""
	}
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".local", "bin")
	sep := string(os.PathListSeparator)
	if !strings.Contains(sep+os.Getenv("PATH")+sep, sep+dir+sep) {
		return "", ""
	}
	link := filepath.Join(dir, "continuum")
	if target, err := os.Readlink(link); err == nil {
		if target == bin {
			return link, "none"
		}
		if !strings.Contains(target, ".continuum") {
			return "", "" // someone else's continuum: left alone
		}
		return link, "replace"
	} else if _, err := os.Lstat(link); err == nil {
		return "", ""
	}
	return link, "create"
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// installTargets returns the agent settings files install would hook.
func installTargets(o installOptions, cfg core.Config, r *rules.Rules) []hooks.Target {
	want := map[string]bool{}
	for _, h := range strings.Split(o.Only, ",") {
		if h = strings.TrimSpace(h); h != "" {
			want[h] = true
		}
	}
	var out []hooks.Target
	for _, t := range hooks.Targets(cfg) {
		if len(want) > 0 && !want[t.Harness] {
			continue
		}
		_, binErr := deliver.Binary(cfg, r.Harness(t.Harness))
		_, dirErr := os.Stat(filepath.Dir(t.Path))
		if binErr != nil && dirErr != nil && len(want) == 0 {
			continue
		}
		out = append(out, t)
	}
	return out
}

func installBinPath(o installOptions, p core.Paths) string {
	if !o.Copy {
		return self()
	}
	name := "continuum"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return filepath.Join(p.Home, "bin", name)
}

// installPlanItems lists everything install (or update) will touch.
func installPlanItems(o installOptions, p core.Paths, cfg core.Config, r *rules.Rules) []planItem {
	bin := installBinPath(o, p)
	var items []planItem

	if o.Copy {
		act := "create"
		if exists(bin) {
			act = "replace"
		}
		items = append(items, planItem{Action: act, What: "Continuum program", Path: bin,
			Purpose: "the Continuum executable that the hooks, shell hook and login item run",
			Notes:   []string{"copied from: " + tildePath(self())}})
	}
	if link, act := linkPlan(bin); link != "" {
		items = append(items, planItem{Action: act, What: "command link", Path: link,
			Purpose: "a link to the program, so `continuum` works in any terminal (that folder is on your PATH)"})
	}
	act := "create"
	if exists(p.Home) {
		act = "keep"
	}
	items = append(items, planItem{Action: act, What: "Continuum folder", Path: p.Home,
		Purpose: "Continuum's own files: settings (config.json, scope.json), the schedule of resumes, history.jsonl, daemon.log, and install.json (what install set up, so `update` and `uninstall` know)"})

	for _, t := range installTargets(o, cfg, r) {
		h := r.Harness(t.Harness)
		st, _ := hooks.Status(t, bin)
		it := planItem{What: h.DisplayName + " hooks", Path: t.Path}
		purpose := map[string]string{
			"claude": "lets Claude Code (terminal, IDE and Desktop) tell Continuum when a session stalls on a limit, and where it runs",
			"codex":  "lets Codex tell Continuum when a session starts and stops, and where it runs",
			"grok":   "lets Grok Build tell Continuum when a session stalls on a limit, and where it runs (a file of Continuum's own; Grok asks you to trust a folder before its hooks run there)",
		}[t.Harness]
		if purpose == "" {
			purpose = "lets " + h.DisplayName + " tell Continuum about its sessions"
		}
		it.Purpose = purpose
		switch {
		case st == hooks.StateCurrent:
			it.Action = "none"
			it.Notes = []string{"already has the current hooks"}
		case !exists(t.Path):
			it.Action = "create"
			it.Notes = []string{"adds a hook to: " + strings.Join(t.Events, ", ")}
		default:
			it.Action = "change"
			n := "adds a hook to: " + strings.Join(t.Events, ", ") + "; every other setting and hook in it stays as it is"
			if st == hooks.StateOutdated || st == hooks.StatePartial {
				n = "updates Continuum's hooks to this version (" + strings.Join(t.Events, ", ") + "); every other setting and hook stays as it is"
			}
			it.Notes = []string{n, "a backup is saved first, next to it: " + filepath.Base(t.Path) + ".continuum-backup-<date-time>"}
		}
		items = append(items, it)
	}

	if o.Shell && runtime.GOOS != "windows" {
		for _, rc := range shellRCs() {
			snip, err := shellBlock(rc.Shell, bin)
			if err != nil {
				continue
			}
			old, _ := os.ReadFile(rc.Path)
			it := planItem{What: rc.Shell + " shell hook", Path: rc.Path,
				Purpose: "a marked block (# >>> continuum >>> … # <<< continuum <<<) that runs interactive agents (" + strings.Join(hookCommandNames(), ", ") + ") under Continuum, so it can type into them in any terminal"}
			switch {
			case replaceBlock(string(old), snip) == string(old):
				it.Action = "none"
				it.Notes = []string{"already has the current block"}
			case len(old) == 0:
				it.Action = "create"
			default:
				it.Action = "change"
				it.Notes = []string{"the rest of the file stays as it is; a backup is saved first: " + filepath.Base(rc.Path) + ".continuum-backup-<date-time>"}
			}
			items = append(items, it)
		}
	}

	if o.Service {
		plan, err := service.MakePlan(bin, p)
		switch {
		case err != nil:
			items = append(items, planItem{Action: "none", What: "login item", Purpose: err.Error()})
		case plan.File != "":
			act := "create"
			if exists(plan.File) {
				act = "replace"
			}
			items = append(items, planItem{Action: act, What: "login item", Path: plan.File,
				Purpose: "starts Continuum's background daemon when you log in, so resumes happen without you starting anything"})
		default:
			items = append(items, planItem{Action: "create", What: "login item", Path: "scheduled task \"Continuum\"",
				Purpose: "starts Continuum's background daemon when you log in"})
		}
	}
	if o.Standalone {
		items = append(items, planItem{Action: "change", What: "standalone mode", Path: filepath.Join(p.Home, "config.json"),
			Purpose: "sets autostart_daemon to false, so hooks and the shell hook never start a background daemon",
			Notes:   []string{"nothing runs in the background; Continuum works while `continuum watch` is open in a terminal"}})
		if service.Installed(p) {
			items = append(items, planItem{Action: "remove", What: "login item", Purpose: "the login item from a previous background install"})
		}
		return items
	}
	items = append(items, planItem{Action: "start", What: "background daemon", Path: bin + " daemon",
		Purpose: "watches agent sessions and continues them when their limits reset",
		Notes:   []string{"while a resume is due soon it asks the OS to postpone idle sleep (released as soon as nothing is pending)"}})
	return items
}

// hookCommandNames lists the agent commands the shell hook will wrap now.
func hookCommandNames() []string {
	var out []string
	for _, a := range shellHookAgents() {
		out = append(out, a.Command)
	}
	if len(out) == 0 {
		return []string{"claude", "codex"}
	}
	return out
}

var installFooter = []string{
	"Continuum works entirely on this computer: it sends nothing anywhere.",
	"Everything above is undone by `continuum uninstall`.",
}

// uninstallPlanItems lists everything uninstall will remove (and keep).
func uninstallPlanItems(p core.Paths, cfg core.Config, purge bool) []planItem {
	var items []planItem
	if plan, err := service.MakePlan(self(), p); err == nil && plan.File != "" && exists(plan.File) {
		items = append(items, planItem{Action: "remove", What: "login item", Path: plan.File, Purpose: "started Continuum's daemon at login"})
	} else if service.Installed(p) {
		items = append(items, planItem{Action: "remove", What: "login item", Purpose: "started Continuum's daemon at login"})
	}
	if daemonRunning(p) {
		items = append(items, planItem{Action: "stop", What: "background daemon", Purpose: "the running Continuum daemon"})
	}
	r, _ := rules.Cached(p.Rules)
	for _, t := range hooks.Targets(cfg) {
		if st, _ := hooks.Status(t, ""); st == hooks.StateMissing {
			continue
		}
		name := t.Harness
		if r != nil {
			name = r.Harness(t.Harness).DisplayName
		}
		items = append(items, planItem{Action: "change", What: name + " hooks", Path: t.Path,
			Purpose: "removes every Continuum hook from this file",
			Notes:   []string{"your own settings and hooks stay; if nothing else changed since install, the file goes back to its exact original bytes"}})
	}
	for _, rc := range allRCs() {
		if data, err := os.ReadFile(rc.Path); err == nil && strings.Contains(string(data), blockStart) {
			items = append(items, planItem{Action: "change", What: rc.Shell + " shell hook", Path: rc.Path,
				Purpose: "removes the marked Continuum block; the rest of the file stays as it is"})
		}
	}
	home, _ := os.UserHomeDir()
	link := filepath.Join(home, ".local", "bin", "continuum")
	if target, err := os.Readlink(link); err == nil && strings.Contains(target, ".continuum") {
		items = append(items, planItem{Action: "remove", What: "command link", Path: link, Purpose: "the `continuum` command link"})
	}
	if exists(daemon.InstallPath(p)) {
		items = append(items, planItem{Action: "remove", What: "install record", Path: daemon.InstallPath(p), Purpose: "what install set up"})
	}
	if purge {
		var backups []string
		for _, t := range hooks.Targets(cfg) {
			backups = append(backups, hooks.Backups(t.Path)...)
		}
		for _, rc := range allRCs() {
			m, _ := filepath.Glob(rc.Path + ".continuum-backup-*")
			backups = append(backups, m...)
		}
		backups = dedupe(backups)
		for _, b := range backups {
			items = append(items, planItem{Action: "remove", What: "settings backup", Path: b, Purpose: "a copy Continuum saved before changing that file"})
		}
		if exists(p.Home) {
			files, size := dirStats(p.Home)
			items = append(items, planItem{Action: "remove", What: "Continuum folder", Path: p.Home,
				Purpose: fmt.Sprintf("the program, settings, scope, history and logs (%d files, %s)", files, humanBytes(size))})
		}
	} else if exists(p.Home) {
		items = append(items, planItem{Action: "keep", What: "Continuum folder", Path: p.Home,
			Purpose: "your settings, scope and history, so a later install picks up where you left off (add --purge to delete it, and the settings backups)"})
	}
	return items
}

func dedupe(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range list {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func dirStats(dir string) (int, int64) {
	n, size := 0, int64(0)
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
			if info, err := d.Info(); err == nil {
				size += info.Size()
			}
		}
		return nil
	})
	return n, size
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d bytes", n)
}
