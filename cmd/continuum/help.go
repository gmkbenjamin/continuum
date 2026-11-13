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

var groups = []string{"Set up", "Sessions and resumes", "All or chosen sessions", "Tuning", "Running agents", "Background daemon", "For agents", "Internal"}

var helpEntries = []helpEntry{
	{
		Name: "install", Group: "Set up", Args: "[options]",
		Summary: "Hook Continuum into your agents, shell and login items",
		Detail: `First shows every file and item it will create or change, where it is and
what it is for, and asks you to confirm (default: no). --dry-run shows the
list and stops; --yes skips the question (needed when there is no terminal to
ask in, e.g. in scripts).

Sets Continuum up so it works on its own from now on:

  1. Copies this binary to ~/.continuum/bin/continuum (and links it into
     ~/.local/bin when that folder is on your PATH).
  2. Adds hooks to ~/.claude/settings.json, ~/.codex/hooks.json and
     ~/.grok/hooks/continuum.json so Claude Code, Codex and Grok Build report
     stalls and where they run. Every other setting is kept, and a timestamped
     backup is saved first.
  3. Adds a marked block to your shell startup file (~/.zshrc, ~/.bashrc, fish
     conf.d) that runs interactive agents (claude, codex, gemini, opencode, ...)
     under Continuum, so it can type into them in any terminal.
  4. Starts the daemon at login (launchd on macOS, a systemd user unit on
     Linux, a scheduled task on Windows).

Running it again updates everything in place. ` + "`continuum uninstall`" + ` reverses it.
Limits that happened before installing are never resumed.`,
		Examples: []string{"continuum install --dry-run", "continuum install", "continuum install --no-shell --only claude", "continuum install --yes"},
	},
	{
		Name: "update", Group: "Set up", Args: "[options]",
		Summary: "Apply this version: refresh hooks, shell hook and login item",
		Detail: `Re-applies your install with the choices you made then (shell hook, login
item, which agents), using the binary you run it from. Like install, it first
lists everything it will change and asks you to confirm (--yes to skip). Use it after building
or downloading a new version:

  git pull && make build && ./continuum update

It copies the new binary into place, rewrites Continuum's hooks in the
agents' settings files in the current format (removing any an older version
added that this one no longer uses), refreshes the shell hook and login item,
and restarts the daemon on the new binary. Your settings, scope and history
are kept.

The daemon also keeps the hooks current by itself: if an agent update
removes them, or they point at an old location, it rewrites them and tells
you.`,
		Examples: []string{"./continuum update --dry-run", "./continuum update", "./continuum update --yes"},
	},
	{
		Name: "uninstall", Group: "Set up", Args: "[options]",
		Summary: "Remove everything Continuum added",
		Detail: `First lists everything it will remove, change or keep, where it is and
what it was for, and asks you to confirm (default: no; --yes to skip).

Removes, in this order: the login item and the running daemon; Continuum's
hooks from every event in the agents' settings files (any version's, keeping
all of yours; the files go back to their original bytes); the shell hook
block from every shell startup file; the ~/.local/bin link. Then it checks
that nothing is left and says so.

Without --purge, ~/.continuum is kept (settings, scope, history), so a later
install picks up where you left off. With --purge it is deleted too, along
with the backups Continuum saved of your settings files.

Agent sessions running under Continuum at the time keep working normally.
And if Continuum is ever deleted without uninstalling, its hooks stay
harmless: they do nothing, silently, until removed.`,
		Examples: []string{"continuum uninstall --dry-run", "continuum uninstall", "continuum uninstall --purge", "continuum uninstall --purge --yes"},
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
		Name: "diagnose", Group: "Set up", Args: "[options]",
		Summary: "Write a report for troubleshooting or a bug report",
		Detail: `Collects what is needed to understand a problem: version and OS, install and
mode, settings, each agent's version, hooks and resume commands, desktop and
terminal apps, update checks, recent resumes and the end of the logs.

Personal details are removed: your home folder becomes ~, your user name
<user>, session ids their first 8 characters; custom prompts and limit
messages are left out. Read it before sharing.

For more detail in the logs first, turn on debug logging
(` + "`\"log_level\": \"debug\"`" + ` in config.json, or ` + "`continuum watch --debug`" + `), reproduce
the problem, then run diagnose.`,
		Examples: []string{"continuum diagnose", "continuum diagnose -o continuum-report.md"},
	},
	{
		Name: "interactive", Aliases: []string{"menu", "i"}, Group: "Set up",
		Summary: "A menu for everything Continuum does",
		Detail: `Opens a menu: status, sessions (choose, exclude, set a resume message,
resume now, cancel), resume messages, scope, grace, watching live, doctor,
the log, and install, update or uninstall. Running ` + "`continuum`" + ` with no
arguments in a terminal opens it too.`,
	},
	{
		Name: "status", Group: "Set up", Args: "[options]",
		Summary: "Show the daemon, hooks, and every session and its state",
		Detail: `Shows whether Continuum is running and its mode, the hooks, and every open
session (plus those active in the last 12 hours) with:

  STATE    what it is doing: working, idle, waiting for you (a menu or
           prompt is open), usage limit · resumes 10:55 (in 2h),
           API error · retrying, resumed 10:55 by Continuum, needs you
           (billing, sign-in), not resuming (excluded or not chosen),
           closed
  WHERE    where it runs: tmux, Rex, iTerm2, Claude Desktop, background...
           "(via Continuum)": through Continuum's wrapper
  COVERED  whether Continuum will resume it (see ` + "`continuum scope`" + `)

New sessions appear by themselves within seconds of starting; nothing needs
registering. Agents that report their own state (Claude Code) show working
or idle; for others the state comes from their recent activity.`,
		Examples: []string{"continuum status", "continuum status --all", "continuum status --json"},
	},
	{
		Name: "version", Aliases: []string{"--version", "-v"}, Group: "Set up",
		Summary: "Print the version",
	},
	{
		Name: "sessions", Group: "Sessions and resumes", Args: "[options]",
		Summary: "List sessions with their state, and which message each gets",
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
		ArgsHelp: [][2]string{{"ID", "a resume from `continuum list`, a session id, or its first 4+ characters (it must name one session)"}, {"this", "the agent session running this command (for agents)"}, {"all", "every pending resume"}},
		Examples: []string{"continuum cancel claude:4f2c1d9e", "continuum cancel this", "continuum cancel all"},
	},
	{
		Name: "resume-now", Aliases: []string{"now"}, Group: "Sessions and resumes", Args: "ID|all",
		Summary:  "Resume now instead of waiting for the reset",
		Detail:   "Useful when you bought extra usage or the limit was lifted early. The usual\nsafety checks (still stalled, no menu open, never a copy) still apply. Naming a\nsession also retries one that finished (resumed, skipped, cancelled, failed);\n`all` covers pending resumes only. A billing or sign-in stop is never resumed.",
		ArgsHelp: [][2]string{{"ID", "a resume from `continuum list`, a session id, or its first 4+ characters (it must name one session)"}, {"this", "the agent session running this command (for agents)"}, {"all", "every pending resume"}},
	},
	{
		Name: "this", Aliases: []string{"self"}, Group: "For agents", Args: "[options]",
		Summary:  "The agent session running this command: its state and settings",
		Detail:   "For agents, through their shell tool. Wherever a command takes a session\n(`exclude`, `include`, `choose`, `unchoose`, `message --session`, `cancel`,\n`resume-now`), `this` means the agent's own session. Continuum finds it from the\nagent's environment, its process, or the wrapper or pane it runs in.",
		Examples: []string{"continuum this", "continuum this --json", "continuum exclude this", "continuum message set --session this \"Re-run the tests, then continue.\""},
	},
	{
		Name: "agent-guide", Group: "For agents",
		Summary: "Print a short guide for agents on controlling Continuum",
		Detail:  "The same guide install gives Claude Code, Codex and Antigravity as a skill. Tell\nany other agent to run it.",
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
		Name: "message", Aliases: []string{"messages", "msg"}, Group: "Tuning", Args: "[show | set | reset | preview] [options] [TEXT]",
		Summary: "What Continuum types to continue a session",
		Detail: `Shows or changes the resume message. The most specific one is used:

  1. a message for that session
  2. a message for the folder it runs in (the deepest folder wins)
  3. a message for the agent (claude, codex, ...)
  4. your default message
  5. the built-in message

Each can have a separate message for temporary API errors (--outage).
{agent} and {folder} are replaced by the agent's name and the session's
folder. Messages are typed as one line: line breaks become spaces.

To give a session its own message as you start it:
  CONTINUUM_MESSAGE="Only fix the failing test, then stop." claude
  continuum run --message "..." -- codex

  continuum message                     show every message, and what each open
                                        session would get
  continuum message set "TEXT"          your default message
  continuum message set --agent A "TEXT"
  continuum message set --session ID|NAME "TEXT"
  continuum message set --folder PATH "TEXT"
  continuum message set --outage ...    the message after a temporary API error
  continuum message reset [--agent A | --session S | --folder F | --all] [--outage]
  continuum message preview ID|NAME     the exact text a session would get`,
		Examples: []string{`continuum message set "Limit reset. Re-read the plan, then continue in {folder}."`, `continuum message set --session api-refactor "Finish the migration only."`, `continuum message set --agent codex --outage "Retry the last step."`, "continuum message reset --all"},
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
		Name: "watch", Group: "Background daemon", Args: "[options]",
		Summary: "Run Continuum in this terminal, showing what it does",
		Detail: `The standalone mode: Continuum runs in the foreground and prints what it
notices and does (stalls, scheduled resumes, resumes, skips), until you press
Ctrl+C. Nothing runs in the background. Hooks and the shell hook keep
collecting events while it isn't running; it catches up on them (up to
catch_up_hours back) when it next runs.

Only one Continuum runs at a time. If the background daemon is running,
` + "`watch`" + ` says so; --takeover stops it and watches here instead.

Install in standalone mode with ` + "`continuum install --standalone`" + `.`,
		Examples: []string{"continuum watch", "continuum watch --debug", "continuum watch --takeover"},
	},
	{
		Name: "logs", Aliases: []string{"log"}, Group: "Background daemon", Args: "[options]",
		Summary: "Show or follow what the background daemon is doing",
		Detail: `Prints the end of ~/.continuum/daemon.log. Levels: info (default) records what
Continuum did; debug (` + "`\"log_level\": \"debug\"`" + ` in config.json, ` + "`watch --debug`" + `,
` + "`daemon --debug`" + `, or CONTINUUM_DEBUG=1) also records why: every event, check and
delivery route. The log rotates to daemon.log.1 at 5 MB.`,
		Examples: []string{"continuum logs", "continuum logs -f", "continuum logs -n 200", "continuum logs --hooks"},
	},
	{
		Name: "start", Group: "Background daemon",
		Summary: "Start the background daemon",
	},
	{
		Name: "stop", Group: "Background daemon",
		Summary: "Stop the background daemon",
		Detail:  "It stays stopped (hooks don't start it again) until `continuum start`, or until\nyour next login if auto-start is on. To keep it from starting at all, use\n`continuum disable`.",
	},
	{
		Name: "disable", Group: "Background daemon", Args: "[options]",
		Summary:  "Turn auto-start off: nothing starts the daemon by itself",
		Detail:   "Removes the login item and stops hooks from starting the daemon, and stops it now\n(unless --keep-running). Hooks still note stalls, so limits that reset meanwhile are\ncaught up the next time Continuum runs: `continuum start` (until you stop it or log\nout) or `continuum watch`. `update` keeps this choice. Undo with `continuum enable`.",
		Examples: []string{"continuum disable", "continuum disable --keep-running"},
	},
	{
		Name: "enable", Group: "Background daemon",
		Summary: "Turn auto-start back on (at login, and whenever an agent needs it)",
		Detail:  "Adds the login item back, lets hooks start the daemon, and starts it now. From\nstandalone mode, this switches to background mode.",
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

Keeps AI coding agents (Claude Code, Codex, Grok Build, Gemini CLI, Antigravity,
Cursor, opencode, Aider and more) working through usage limits and API outages. When an agent
stalls, Continuum waits for the limit to reset and continues the same session,
in place: never a copy, never a new session. Works in any terminal, with the
window unfocused or the screen locked.

Usage:
  continuum                     the interactive menu (in a terminal)
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
			if len(f.Name) == 1 {
				left = "-" + f.Name
			}
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
var flagCommands = map[string]bool{"this": true, "agent-guide": true, "disable": true, "enable": true, "status": true, "sessions": true, "diagnose": true, "watch": true, "logs": true, "install": true, "update": true, "uninstall": true, "run": true, "list": true, "detect": true, "rules": true, "daemon": true}

// subcommandCommands have subcommands with their own options; their help
// text documents every option.
var subcommandCommands = map[string]bool{"message": true}

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
