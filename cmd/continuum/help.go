package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

// helpEntry documents one command. Options are not listed here: they are
// read from the command's real flag set, so help never drifts from the code.
type helpEntry struct {
	Name     string
	Aliases  []string
	Group    string
	Args     string // usage after the command name, e.g. "[options] ID|all"
	Summary  string // one line, for the command list
	Detail   string // paragraphs
	ArgsHelp [][2]string
	Examples []string
	Hidden   bool
}

var groups = []string{"Set up", "Sessions and resumes", "All or chosen sessions", "Tuning", "Running agents", "Background daemon", "Internal"}

var helpEntries = []helpEntry{
	{
		Name: "install", Group: "Set up", Args: "[options]",
		Summary: "Hook Continuum into your agents, shell and login items",
		Detail: `Sets Continuum up so it works on its own from now on:

  1. Copies this binary to ~/.continuum/bin/continuum (and links it into
     ~/.local/bin when that folder is on your PATH).
  2. Adds hooks to ~/.claude/settings.json and ~/.codex/hooks.json so Claude
     Code and Codex report stalls and where they run. Every other setting is
     kept, and a timestamped backup is saved first.
  3. Adds a marked block to your shell startup file (~/.zshrc, ~/.bashrc, fish
     conf.d) that runs interactive agents (claude, codex, gemini, opencode, ...)
     under Continuum, so it can type into them in any terminal.
  4. Starts the daemon at login (launchd on macOS, a systemd user unit on
     Linux, a scheduled task on Windows).

Running it again updates everything in place. ` + "`continuum uninstall`" + ` reverses it.
Limits that happened before installing are never resumed.`,
		Examples: []string{"continuum install --dry-run", "continuum install", "continuum install --no-shell --only claude"},
	},
	{
		Name: "uninstall", Group: "Set up", Args: "[options]",
		Summary: "Remove the hooks, shell hook and login item",
		Detail: `Removes everything ` + "`install`" + ` added: the agent hooks (the settings files go back to
their original bytes), the shell hook block, the login item and the
~/.local/bin link, and stops the daemon. Your data in ~/.continuum is kept;
delete that folder to remove everything.`,
		Examples: []string{"continuum uninstall --dry-run", "continuum uninstall"},
	},
	{
		Name: "doctor", Group: "Set up",
		Summary: "Check agents, hooks, terminals and rules",
		Detail: `Runs a full self-check and says what to fix:
  - the rules still recognise every known limit message,
  - each installed agent: version, hooks, which resume routes it supports, and
    whether Continuum can tell which of its sessions are open,
  - multiplexers and the shell hook,
  - scope, keep-awake and the no-copy guarantee.
Run it after updating an agent. Exits 1 if something needs fixing.`,
	},
	{
		Name: "status", Group: "Set up",
		Summary: "Show the daemon, hooks and scheduled resumes",
	},
	{
		Name: "version", Aliases: []string{"--version", "-v"}, Group: "Set up",
		Summary: "Print the version",
	},
	{
		Name: "sessions", Group: "Sessions and resumes",
		Summary: "List open agent sessions and whether each is covered",
		Detail: `Lists the agent sessions that are open now (from the agents themselves) and
those seen recently, with their ids, names, state and folder, and whether the
current scope covers them. Use the ID or NAME with choose, exclude, cancel.`,
	},
	{
		Name: "list", Aliases: []string{"ls", "jobs"}, Group: "Sessions and resumes", Args: "[options]",
		Summary: "Show scheduled and recent resumes",
		Detail: `Shows every resume Continuum is waiting to do (when, and where it will type)
and those finished in the last 24 hours. States:
  pending      waiting for the reset
  delivered    resumed
  skipped      the session continued without Continuum, or moved on
  not_chosen   outside the scope (choose it to resume it)
  needs_user   billing, credits, sign-in or too many failures: needs you
  failed       gave up after max_attempts
  cancelled    you cancelled it`,
		Examples: []string{"continuum list", "continuum list --all --json"},
	},
	{
		Name: "cancel", Group: "Sessions and resumes", Args: "ID|all",
		Summary:  "Cancel scheduled resumes",
		ArgsHelp: [][2]string{{"ID", "a resume from `continuum list` (or a session id prefix)"}, {"all", "every pending resume"}},
		Examples: []string{"continuum cancel claude:4f2c1d9e", "continuum cancel all"},
	},
	{
		Name: "resume-now", Aliases: []string{"now"}, Group: "Sessions and resumes", Args: "ID|all",
		Summary:  "Resume now instead of waiting for the reset",
		Detail:   "Useful when you bought extra usage or the limit was lifted early. The usual\nsafety checks (still stalled, no menu open, never a copy) still apply.",
		ArgsHelp: [][2]string{{"ID", "a resume from `continuum list` (or a session id prefix)"}, {"all", "every pending resume"}},
	},
	{
		Name: "scope", Group: "All or chosen sessions", Args: "[all|chosen]",
		Summary: "Resume every session, or only the ones you choose",
		Detail: `Without an argument, shows the scope and the chosen and excluded entries.
  all      resume every session (the default)
  chosen   resume only sessions picked with ` + "`continuum choose`" + `
Excluded sessions are never resumed in either mode. Changes apply within
seconds, also to sessions that are already waiting.`,
		Examples: []string{"continuum scope", "continuum scope chosen", "continuum scope all"},
	},
	{
		Name: "choose", Group: "All or chosen sessions", Args: "ID|NAME|FOLDER...",
		Summary: "Choose sessions to resume (scope \"chosen\")",
		Detail: `Adds sessions to the chosen list. A session chosen while it is already
waiting on a limit is scheduled straight away. To choose a session as you
start it: CONTINUUM_CHOOSE=1 claude, or continuum run --choose -- codex.`,
		ArgsHelp: [][2]string{
			{"ID", "a session id or id prefix (see `continuum sessions`), optionally agent:ID"},
			{"NAME", "a session name, e.g. Claude's \"api-refactor\" (prefix works)"},
			{"FOLDER", "a path (., ~/work/api): every session started in it or below"},
		},
		Examples: []string{"continuum choose api-refactor", "continuum choose 4f2c1d9e ~/work/api", "continuum choose ."},
	},
	{
		Name: "unchoose", Group: "All or chosen sessions", Args: "ID|NAME|FOLDER|all",
		Summary: "Remove sessions from the chosen list",
	},
	{
		Name: "exclude", Group: "All or chosen sessions", Args: "ID|NAME|FOLDER...",
		Summary:  "Never resume these sessions, whatever the scope",
		Examples: []string{"continuum exclude ~/scratch", "continuum exclude nightly-job"},
	},
	{
		Name: "include", Group: "All or chosen sessions", Args: "ID|NAME|FOLDER|all",
		Summary: "Undo exclude",
	},
	{
		Name: "grace", Group: "Tuning", Args: "[AGENT MINUTES|off|default]",
		Summary: "How long to let an agent resume by itself first",
		Detail: `Some agents (Claude Code) try to resume on their own after a reset while
they are running. Continuum waits this long past the reset before it
continues the session itself, so the two never both continue it. The wait is
skipped when the agent says it won't resume or its process has exited.
Default: 5 minutes for Claude, 30 seconds for Codex. Applies within seconds,
also to sessions already waiting.`,
		ArgsHelp: [][2]string{{"AGENT", "claude, codex, ..."}, {"MINUTES", "minutes to wait (decimals allowed)"}, {"off", "don't wait: continue right after the reset"}, {"default", "back to the built-in wait"}},
		Examples: []string{"continuum grace", "continuum grace claude 10", "continuum grace claude off"},
	},
	{
		Name: "detect", Group: "Tuning", Args: "[options] [TEXT|-]",
		Summary:  "Show what Continuum makes of a message",
		Detail:   "Classifies TEXT (or standard input with -) the way the daemon would: which\nagent, what kind of stall, and when the limit resets. Handy for checking a\nnew agent message against the rules.",
		Examples: []string{`continuum detect "You've hit your usage limit. Try again at 5:53 PM."`, `continuum detect --harness gemini "Quota exceeded ... try again in 2h"`, "pbpaste | continuum detect -"},
	},
	{
		Name: "rules", Group: "Tuning", Args: "[options]",
		Summary: "Print the effective rules",
		Detail: `Everything Continuum knows about agents and multiplexers is data: limit
wording, menus to avoid, CLI flags, multiplexer commands. Override or extend
any of it in ~/.continuum/rules.json (merged by name; null deletes a key), no
rebuild needed. A broken override is reported and the built-in rules are used.`,
		Examples: []string{"continuum rules", "continuum rules --defaults > ~/.continuum/rules.json"},
	},
	{
		Name: "run", Group: "Running agents", Args: "[options] [--] COMMAND [ARGS...]",
		Summary: "Run an agent under Continuum's terminal wrapper",
		Detail: `Runs COMMAND in a terminal Continuum owns, passing all input and output
through unchanged (colours, mouse, resizing, paste, Ctrl+C, Ctrl+Z / fg).
Continuum watches the screen for limit messages and types the resume prompt
into this same session when the limit resets. Works in any terminal emulator,
focused or not. The shell hook from ` + "`install`" + ` does this for you automatically.`,
		Examples: []string{"continuum run -- claude", "continuum run --harness generic -- my-agent --model x", "continuum run --choose -- codex"},
	},
	{
		Name: "shell-init", Group: "Running agents", Args: "[zsh|bash|fish]",
		Summary: "Print the shell functions that wrap installed agents",
		Detail: `Prints one function per installed agent (claude, codex, gemini, opencode,
grok, ...) that runs interactive sessions under ` + "`continuum run --auto`" + `. Print
mode, --version, subcommands, pipes and redirects run the agent directly.
The block ` + "`install`" + ` adds to your shell startup file evaluates this at each shell
start, so newly installed agents are picked up automatically. Skip it once
with CONTINUUM_NO_WRAP=1 <agent> or ` + "`command <agent>`" + `. Choose the commands
with "shell_hook_agents" in config.json.`,
		Examples: []string{`eval "$(continuum shell-init zsh)"`, "continuum shell-init fish | source"},
	},
	{
		Name: "start", Group: "Background daemon",
		Summary: "Start the background daemon",
	},
	{
		Name: "stop", Group: "Background daemon",
		Summary: "Stop the background daemon",
		Detail:  "If the login item is installed, the daemon starts again at the next login.",
	},
	{
		Name: "daemon", Group: "Background daemon", Args: "[options]",
		Summary: "Run the daemon in the foreground (for debugging)",
	},
	{
		Name: "hook", Group: "Internal", Args: "AGENT",
		Summary: "Called by agent hooks (reads the hook payload on stdin)",
		Detail:  "Used by the hooks `install` adds. Always silent and always exits 0, so it can\nnever disturb the agent.",
		Hidden:  true,
	},
	{
		Name: "help", Group: "Internal", Args: "[COMMAND]",
		Summary: "Show help for Continuum or a command",
		Hidden:  true,
	},
}

func findHelp(name string) *helpEntry {
	for i := range helpEntries {
		e := &helpEntries[i]
		if e.Name == name {
			return e
		}
		for _, a := range e.Aliases {
			if a == name {
				return e
			}
		}
	}
	return nil
}

// usageText is the top-level help.
func usageText() string {
	var b strings.Builder
	b.WriteString(`continuum - work never loses its momentum

Keeps AI coding agents (Claude Code, Codex, Gemini CLI, opencode, Aider, Cursor,
Copilot and more) working through usage limits and API outages. When an agent
stalls, Continuum waits for the limit to reset and continues the same session,
in place: never a copy, never a new session. Works in any terminal, with the
window unfocused or the screen locked.

Usage:
  continuum <command> [options] [arguments]
  continuum <command> -h        help for a command

`)
	byGroup := map[string][]helpEntry{}
	for _, e := range helpEntries {
		if !e.Hidden {
			byGroup[e.Group] = append(byGroup[e.Group], e)
		}
	}
	for _, g := range groups {
		list := byGroup[g]
		if len(list) == 0 {
			continue
		}
		b.WriteString(g + ":\n")
		for _, e := range list {
			fmt.Fprintf(&b, "  %-12s %s\n", e.Name, e.Summary)
		}
		b.WriteString("\n")
	}
	b.WriteString(`Getting started:
  continuum install --dry-run     see what install would change
  continuum install               set everything up
  continuum doctor                check it

Files live in ~/.continuum (set CONTINUUM_HOME to move them): config.json,
scope.json, rules.json (overrides), history.jsonl, daemon.log.
Documentation: README.md, and docs/ in the source repository.
`)
	return b.String()
}

// printHelp writes a command's help. fs, if given, supplies the options.
func printHelp(w io.Writer, name string, fs *flag.FlagSet) {
	e := findHelp(name)
	if e == nil {
		fmt.Fprintf(w, "continuum: no help for %q\n\n%s", name, usageText())
		return
	}
	fmt.Fprintf(w, "continuum %s - %s\n\nUsage:\n  continuum %s", e.Name, e.Summary, e.Name)
	if e.Args != "" {
		fmt.Fprint(w, " "+e.Args)
	}
	fmt.Fprintln(w)
	if len(e.Aliases) > 0 {
		var al []string
		for _, a := range e.Aliases {
			if !strings.HasPrefix(a, "-") {
				al = append(al, a)
			}
		}
		if len(al) > 0 {
			fmt.Fprintf(w, "  (also: %s)\n", strings.Join(al, ", "))
		}
	}
	if e.Detail != "" {
		fmt.Fprintf(w, "\n%s\n", indent(e.Detail, "  "))
	}
	if len(e.ArgsHelp) > 0 {
		fmt.Fprintln(w, "\nArguments:")
		for _, a := range e.ArgsHelp {
			fmt.Fprintf(w, "  %-10s %s\n", a[0], a[1])
		}
	}
	fmt.Fprintln(w, "\nOptions:")
	if fs != nil {
		var names []string
		fs.VisitAll(func(f *flag.Flag) { names = append(names, f.Name) })
		sort.Strings(names)
		for _, n := range names {
			f := fs.Lookup(n)
			left := "--" + f.Name
			if !isBoolFlag(f) {
				left += " " + valueName(f)
			}
			desc := f.Usage
			if !isBoolFlag(f) {
				_, desc = flag.UnquoteUsage(f)
			}
			if !isBoolFlag(f) && f.DefValue != "" {
				desc += " (default " + f.DefValue + ")"
			}
			fmt.Fprintf(w, "  %-22s %s\n", left, wrapText(desc, 25, 80))
		}
	}
	fmt.Fprintf(w, "  %-22s %s\n", "-h, --help", "Show this help.")
	if len(e.Examples) > 0 {
		fmt.Fprintln(w, "\nExamples:")
		for _, x := range e.Examples {
			fmt.Fprintln(w, "  "+x)
		}
	}
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func valueName(f *flag.Flag) string {
	if name, _ := flag.UnquoteUsage(f); name != "" {
		return strings.ToUpper(name)
	}
	return "VALUE"
}

func indent(s, pre string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pre + l
		}
	}
	return strings.Join(lines, "\n")
}

// wrapText wraps s to width, indenting continuation lines by col.
func wrapText(s string, col, width int) string {
	words := strings.Fields(s)
	var b strings.Builder
	n := col
	for i, w := range words {
		if i > 0 {
			if n+1+len(w) > width {
				b.WriteString("\n" + strings.Repeat(" ", col))
				n = col
			} else {
				b.WriteByte(' ')
				n++
			}
		}
		b.WriteString(w)
		n += len(w)
	}
	return b.String()
}

// errHelpShown means help was printed: exit 0 quietly.
var errHelpShown = errors.New("help shown")

// newFlagSet returns a flag set whose -h/--help prints the command's help.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

// parseFlags parses args, printing help for -h/--help.
func parseFlags(fs *flag.FlagSet, args []string) error {
	err := fs.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		printHelp(os.Stdout, fs.Name(), fs)
		return errHelpShown
	}
	if err != nil {
		return fmt.Errorf("%v (see `continuum %s -h`)", err, fs.Name())
	}
	return nil
}

// flagCommands lists the commands that parse options; their help shows them.
var flagCommands = map[string]bool{"install": true, "uninstall": true, "run": true, "list": true, "detect": true, "rules": true, "daemon": true}

// wantsHelp reports whether args ask for help: -h/--help/help before any
// "--" (and, for run, before the agent's own command).
func wantsHelp(cmd string, args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if a == "-h" || a == "--help" || a == "-help" {
			return true
		}
		if cmd == "run" && !strings.HasPrefix(a, "-") {
			return false
		}
	}
	return false
}
