# Command reference

Generated from `continuum <command> -h` by `make docs`. Every command also
prints this help itself with `-h` or `--help`.

```text
continuum - work never loses its momentum

Keeps AI coding agents (Claude Code, Codex, Gemini CLI, opencode, Aider, Cursor,
Copilot and more) working through usage limits and API outages. When an agent
stalls, Continuum waits for the limit to reset and continues the same session,
in place: never a copy, never a new session. Works in any terminal, with the
window unfocused or the screen locked.

Usage:
  continuum <command> [options] [arguments]
  continuum <command> -h        help for a command

Set up:
  install      Hook Continuum into your agents, shell and login items
  update       Apply this version: refresh hooks, shell hook and login item
  uninstall    Remove everything Continuum added
  doctor       Check agents, hooks, terminals and rules
  diagnose     Write a report for troubleshooting or a bug report
  status       Show the daemon, hooks and scheduled resumes
  version      Print the version

Sessions and resumes:
  sessions     List open agent sessions and whether each is covered
  list         Show scheduled and recent resumes
  cancel       Cancel scheduled resumes
  resume-now   Resume now instead of waiting for the reset

All or chosen sessions:
  scope        Resume every session, or only the ones you choose
  choose       Choose sessions to resume (scope "chosen")
  unchoose     Remove sessions from the chosen list
  exclude      Never resume these sessions, whatever the scope
  include      Undo exclude

Tuning:
  grace        How long to let an agent resume by itself first
  detect       Show what Continuum makes of a message
  rules        Print the effective rules

Running agents:
  run          Run an agent under Continuum's terminal wrapper
  shell-init   Print the shell functions that wrap installed agents

Background daemon:
  watch        Run Continuum in this terminal, showing what it does
  logs         Show or follow what the background daemon is doing
  start        Start the background daemon
  stop         Stop the background daemon
  daemon       Run the daemon in the foreground (for debugging)

Getting started:
  continuum install --dry-run     see what install would change
  continuum install               set everything up
  continuum doctor                check it

Files live in ~/.continuum (set CONTINUUM_HOME to move them): config.json,
scope.json, rules.json (overrides), history.jsonl, daemon.log.
Documentation: README.md, and docs/ in the source repository.
```

## install

```text
continuum install - Hook Continuum into your agents, shell and login items

Usage:
  continuum install [options]

  First shows every file and item it will create or change, where it is and
  what it is for, and asks you to confirm (default: no). --dry-run shows the
  list and stops; --yes skips the question (needed when there is no terminal to
  ask in, e.g. in scripts).

  Sets Continuum up so it works on its own from now on:

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

  Running it again updates everything in place. `continuum uninstall` reverses it.
  Limits that happened before installing are never resumed.

Options:
  --background           Run in the background, started at login (the default;
                         skips the question).
  --dry-run              Show what would change, and change nothing.
  --no-copy              Use this binary where it is instead of copying it to
                         ~/.continuum/bin.
  --no-service           Do not add the login item; the daemon is started now
                         but not at the next login.
  --no-shell             Do not add the shell hook. Without it, agents in plain
                         terminals (no tmux etc.) can only be resumed if started
                         with `continuum run`.
  --only AGENTS          Comma-separated AGENTS to add hooks for, e.g.
                         claude,codex (default: every installed agent that has
                         hooks).
  --standalone           Don't run in the background: no login item, and hooks
                         never start a daemon. Continuum works while `continuum
                         watch` is open in a terminal.
  -y                     Same as --yes.
  --yes                  Don't ask for confirmation; go ahead (for scripts).
                         Also -y.
  -h, --help             Show this help.

Examples:
  continuum install --dry-run
  continuum install
  continuum install --no-shell --only claude
  continuum install --yes
```

## update

```text
continuum update - Apply this version: refresh hooks, shell hook and login item

Usage:
  continuum update [options]

  Re-applies your install with the choices you made then (shell hook, login
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
  you.

Options:
  --dry-run              Show what would change, and change nothing.
  -y                     Same as --yes.
  --yes                  Don't ask for confirmation; go ahead (for scripts).
                         Also -y.
  -h, --help             Show this help.

Examples:
  ./continuum update --dry-run
  ./continuum update
  ./continuum update --yes
```

## uninstall

```text
continuum uninstall - Remove everything Continuum added

Usage:
  continuum uninstall [options]

  First lists everything it will remove, change or keep, where it is and
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
  harmless: they do nothing, silently, until removed.

Options:
  --dry-run              Show what would change, and change nothing.
  --purge                Also delete ~/.continuum (the binary, settings, scope,
                         history, logs) and the backups Continuum saved of your
                         settings files. Without it, those are kept so a later
                         install picks up where you left off.
  -y                     Same as --yes.
  --yes                  Don't ask for confirmation; go ahead (for scripts).
                         Also -y.
  -h, --help             Show this help.

Examples:
  continuum uninstall --dry-run
  continuum uninstall
  continuum uninstall --purge
  continuum uninstall --purge --yes
```

## doctor

```text
continuum doctor - Check agents, hooks, terminals and rules

Usage:
  continuum doctor

  Runs a full self-check and says what to fix:
    - the rules still recognise every known limit message,
    - each installed agent: version, hooks, which resume routes it supports, and
      whether Continuum can tell which of its sessions are open,
    - multiplexers and the shell hook,
    - scope, keep-awake and the no-copy guarantee.
  Run it after updating an agent. Exits 1 if something needs fixing.

Options:
  -h, --help             Show this help.
```

## diagnose

```text
continuum diagnose - Write a report for troubleshooting or a bug report

Usage:
  continuum diagnose [options]

  Collects what is needed to understand a problem: version and OS, install and
  mode, settings, each agent's version, hooks and resume commands, desktop and
  terminal apps, update checks, recent resumes and the end of the logs.

  Personal details are removed: your home folder becomes ~, your user name
  <user>, session ids their first 8 characters; custom prompts and limit
  messages are left out. Read it before sharing.

  For more detail in the logs first, turn on debug logging
  (`"log_level": "debug"` in config.json, or `continuum watch --debug`), reproduce
  the problem, then run diagnose.

Options:
  --lines LINES          How many of the last log LINES to include. (default
                         150)
  -o FILE                Write the report to FILE instead of printing it.
  -h, --help             Show this help.

Examples:
  continuum diagnose
  continuum diagnose -o continuum-report.md
```

## status

```text
continuum status - Show the daemon, hooks and scheduled resumes

Usage:
  continuum status

Options:
  -h, --help             Show this help.
```

## version

```text
continuum version - Print the version

Usage:
  continuum version

Options:
  -h, --help             Show this help.
```

## sessions

```text
continuum sessions - List open agent sessions and whether each is covered

Usage:
  continuum sessions

  Lists the agent sessions that are open now (from the agents themselves) and
  those seen recently, with their ids, names, state and folder, and whether the
  current scope covers them. Use the ID or NAME with choose, exclude, cancel.

Options:
  -h, --help             Show this help.
```

## list

```text
continuum list - Show scheduled and recent resumes

Usage:
  continuum list [options]
  (also: ls, jobs)

  Shows every resume Continuum is waiting to do (when, and where it will type)
  and those finished in the last 24 hours. States:
    pending      waiting for the reset
    delivered    resumed
    skipped      the session continued without Continuum, or moved on
    not_chosen   outside the scope (choose it to resume it)
    needs_user   billing, credits, sign-in or too many failures: needs you
    failed       gave up after max_attempts
    cancelled    you cancelled it

Options:
  --all                  Include all finished resumes (default: pending ones and
                         the last 24 hours).
  --json                 Print JSON instead of a table (for scripts and other
                         apps).
  -h, --help             Show this help.

Examples:
  continuum list
  continuum list --all --json
```

## cancel

```text
continuum cancel - Cancel scheduled resumes

Usage:
  continuum cancel ID|all

Arguments:
  ID         a resume from `continuum list` (or a session id prefix)
  all        every pending resume

Options:
  -h, --help             Show this help.

Examples:
  continuum cancel claude:4f2c1d9e
  continuum cancel all
```

## resume-now

```text
continuum resume-now - Resume now instead of waiting for the reset

Usage:
  continuum resume-now ID|all
  (also: now)

  Useful when you bought extra usage or the limit was lifted early. The usual
  safety checks (still stalled, no menu open, never a copy) still apply.

Arguments:
  ID         a resume from `continuum list` (or a session id prefix)
  all        every pending resume

Options:
  -h, --help             Show this help.
```

## scope

```text
continuum scope - Resume every session, or only the ones you choose

Usage:
  continuum scope [all|chosen]

  Without an argument, shows the scope and the chosen and excluded entries.
    all      resume every session (the default)
    chosen   resume only sessions picked with `continuum choose`
  Excluded sessions are never resumed in either mode. Changes apply within
  seconds, also to sessions that are already waiting.

Options:
  -h, --help             Show this help.

Examples:
  continuum scope
  continuum scope chosen
  continuum scope all
```

## choose

```text
continuum choose - Choose sessions to resume (scope "chosen")

Usage:
  continuum choose ID|NAME|FOLDER...

  Adds sessions to the chosen list. A session chosen while it is already
  waiting on a limit is scheduled straight away. To choose a session as you
  start it: CONTINUUM_CHOOSE=1 claude, or continuum run --choose -- codex.

Arguments:
  ID         a session id or id prefix (see `continuum sessions`), optionally agent:ID
  NAME       a session name, e.g. Claude's "api-refactor" (prefix works)
  FOLDER     a path (., ~/work/api): every session started in it or below

Options:
  -h, --help             Show this help.

Examples:
  continuum choose api-refactor
  continuum choose 4f2c1d9e ~/work/api
  continuum choose .
```

## unchoose

```text
continuum unchoose - Remove sessions from the chosen list

Usage:
  continuum unchoose ID|NAME|FOLDER|all

Options:
  -h, --help             Show this help.
```

## exclude

```text
continuum exclude - Never resume these sessions, whatever the scope

Usage:
  continuum exclude ID|NAME|FOLDER...

Options:
  -h, --help             Show this help.

Examples:
  continuum exclude ~/scratch
  continuum exclude nightly-job
```

## include

```text
continuum include - Undo exclude

Usage:
  continuum include ID|NAME|FOLDER|all

Options:
  -h, --help             Show this help.
```

## grace

```text
continuum grace - How long to let an agent resume by itself first

Usage:
  continuum grace [AGENT MINUTES|off|default]

  Some agents (Claude Code) try to resume on their own after a reset while
  they are running. Continuum waits this long past the reset before it
  continues the session itself, so the two never both continue it. The wait is
  skipped when the agent says it won't resume or its process has exited.
  Default: 5 minutes for Claude, 30 seconds for Codex. Applies within seconds,
  also to sessions already waiting.

Arguments:
  AGENT      claude, codex, ...
  MINUTES    minutes to wait (decimals allowed)
  off        don't wait: continue right after the reset
  default    back to the built-in wait

Options:
  -h, --help             Show this help.

Examples:
  continuum grace
  continuum grace claude 10
  continuum grace claude off
```

## detect

```text
continuum detect - Show what Continuum makes of a message

Usage:
  continuum detect [options] [TEXT|-]

  Classifies TEXT (or standard input with -) the way the daemon would: which
  agent, what kind of stall, and when the limit resets. Handy for checking a
  new agent message against the rules.

Options:
  --harness AGENT        Only check the rules of AGENT (claude, codex, gemini,
                         ...).
  -h, --help             Show this help.

Examples:
  continuum detect "You've hit your usage limit. Try again at 5:53 PM."
  continuum detect --harness gemini "Quota exceeded ... try again in 2h"
  pbpaste | continuum detect -
```

## rules

```text
continuum rules - Print the effective rules

Usage:
  continuum rules [options]

  Everything Continuum knows about agents and multiplexers is data: limit
  wording, menus to avoid, CLI flags, multiplexer commands. Override or extend
  any of it in ~/.continuum/rules.json (merged by name; null deletes a key), no
  rebuild needed. A broken override is reported and the built-in rules are used.

Options:
  --agents               List the supported agents as a table: command, how
                         Continuum recognises it, how it resumes it.
  --defaults             Print the built-in rules, without your overrides (a
                         starting point for ~/.continuum/rules.json).
  -h, --help             Show this help.

Examples:
  continuum rules
  continuum rules --defaults > ~/.continuum/rules.json
```

## run

```text
continuum run - Run an agent under Continuum's terminal wrapper

Usage:
  continuum run [options] [--] COMMAND [ARGS...]

  Runs COMMAND in a terminal Continuum owns, passing all input and output
  through unchanged (colours, mouse, resizing, paste, Ctrl+C, Ctrl+Z / fg).
  Continuum watches the screen for limit messages and types the resume prompt
  into this same session when the limit resets. Works in any terminal emulator,
  focused or not. The shell hook from `install` does this for you automatically.

Options:
  --auto                 Wrap only interactive sessions; run anything else
                         (print mode, --version, subcommands, pipes) directly.
                         Used by the shell hook.
  --choose               Add this session to the chosen sessions for as long as
                         it runs (for `continuum scope chosen`). Same as
                         CONTINUUM_CHOOSE=1.
  --harness AGENT        Treat the command as AGENT: claude, codex, gemini, ...
                         or generic. Guessed from COMMAND when empty.
  -h, --help             Show this help.

Examples:
  continuum run -- claude
  continuum run --harness generic -- my-agent --model x
  continuum run --choose -- codex
```

## shell-init

```text
continuum shell-init - Print the shell functions that wrap installed agents

Usage:
  continuum shell-init [zsh|bash|fish]

  Prints one function per installed agent (claude, codex, gemini, opencode,
  grok, ...) that runs interactive sessions under `continuum run --auto`. Print
  mode, --version, subcommands, pipes and redirects run the agent directly.
  The block `install` adds to your shell startup file evaluates this at each shell
  start, so newly installed agents are picked up automatically. Skip it once
  with CONTINUUM_NO_WRAP=1 <agent> or `command <agent>`. Choose the commands
  with "shell_hook_agents" in config.json.

Options:
  -h, --help             Show this help.

Examples:
  eval "$(continuum shell-init zsh)"
  continuum shell-init fish | source
```

## watch

```text
continuum watch - Run Continuum in this terminal, showing what it does

Usage:
  continuum watch [options]

  The standalone mode: Continuum runs in the foreground and prints what it
  notices and does (stalls, scheduled resumes, resumes, skips), until you press
  Ctrl+C. Nothing runs in the background. Hooks and the shell hook keep
  collecting events while it isn't running; it catches up on them (up to
  catch_up_hours back) when it next runs.

  Only one Continuum runs at a time. If the background daemon is running,
  `watch` says so; --takeover stops it and watches here instead.

  Install in standalone mode with `continuum install --standalone`.

Options:
  --debug                Also show why: every event, check and delivery route.
  --takeover             If Continuum is already running in the background, stop
                         it and watch here instead.
  -h, --help             Show this help.

Examples:
  continuum watch
  continuum watch --debug
  continuum watch --takeover
```

## logs

```text
continuum logs - Show or follow what the background daemon is doing

Usage:
  continuum logs [options]
  (also: log)

  Prints the end of ~/.continuum/daemon.log. Levels: info (default) records what
  Continuum did; debug (`"log_level": "debug"` in config.json, `watch --debug`,
  `daemon --debug`, or CONTINUUM_DEBUG=1) also records why: every event, check and
  delivery route. The log rotates to daemon.log.1 at 5 MB.

Options:
  -f                     Keep printing new lines as they are written (Ctrl+C to
                         stop).
  --hooks                Show hooks.log (one line per agent hook call, written
                         when debug logging is on) instead of daemon.log.
  -n LINES               How many of the last LINES to show. (default 40)
  -h, --help             Show this help.

Examples:
  continuum logs
  continuum logs -f
  continuum logs -n 200
  continuum logs --hooks
```

## start

```text
continuum start - Start the background daemon

Usage:
  continuum start

Options:
  -h, --help             Show this help.
```

## stop

```text
continuum stop - Stop the background daemon

Usage:
  continuum stop

  If the login item is installed, the daemon starts again at the next login.

Options:
  -h, --help             Show this help.
```

## daemon

```text
continuum daemon - Run the daemon in the foreground (for debugging)

Usage:
  continuum daemon [options]

Options:
  --debug                Log every decision (events, checks, delivery routes) to
                         daemon.log, and to this terminal if there is one.
  --once                 Run one pass (collect events, resume what is due) and
                         exit.
  -h, --help             Show this help.
```

## continuum

```text
continuum: no help for "continuum"

continuum - work never loses its momentum

Keeps AI coding agents (Claude Code, Codex, Gemini CLI, opencode, Aider, Cursor,
Copilot and more) working through usage limits and API outages. When an agent
stalls, Continuum waits for the limit to reset and continues the same session,
in place: never a copy, never a new session. Works in any terminal, with the
window unfocused or the screen locked.

Usage:
  continuum <command> [options] [arguments]
  continuum <command> -h        help for a command

Set up:
  install      Hook Continuum into your agents, shell and login items
  update       Apply this version: refresh hooks, shell hook and login item
  uninstall    Remove everything Continuum added
  doctor       Check agents, hooks, terminals and rules
  diagnose     Write a report for troubleshooting or a bug report
  status       Show the daemon, hooks and scheduled resumes
  version      Print the version

Sessions and resumes:
  sessions     List open agent sessions and whether each is covered
  list         Show scheduled and recent resumes
  cancel       Cancel scheduled resumes
  resume-now   Resume now instead of waiting for the reset

All or chosen sessions:
  scope        Resume every session, or only the ones you choose
  choose       Choose sessions to resume (scope "chosen")
  unchoose     Remove sessions from the chosen list
  exclude      Never resume these sessions, whatever the scope
  include      Undo exclude

Tuning:
  grace        How long to let an agent resume by itself first
  detect       Show what Continuum makes of a message
  rules        Print the effective rules

Running agents:
  run          Run an agent under Continuum's terminal wrapper
  shell-init   Print the shell functions that wrap installed agents

Background daemon:
  watch        Run Continuum in this terminal, showing what it does
  logs         Show or follow what the background daemon is doing
  start        Start the background daemon
  stop         Stop the background daemon
  daemon       Run the daemon in the foreground (for debugging)

Getting started:
  continuum install --dry-run     see what install would change
  continuum install               set everything up
  continuum doctor                check it

Files live in ~/.continuum (set CONTINUUM_HOME to move them): config.json,
scope.json, rules.json (overrides), history.jsonl, daemon.log.
Documentation: README.md, and docs/ in the source repository.
```

## continuum

```text
continuum: no help for "continuum"

continuum - work never loses its momentum

Keeps AI coding agents (Claude Code, Codex, Gemini CLI, opencode, Aider, Cursor,
Copilot and more) working through usage limits and API outages. When an agent
stalls, Continuum waits for the limit to reset and continues the same session,
in place: never a copy, never a new session. Works in any terminal, with the
window unfocused or the screen locked.

Usage:
  continuum <command> [options] [arguments]
  continuum <command> -h        help for a command

Set up:
  install      Hook Continuum into your agents, shell and login items
  update       Apply this version: refresh hooks, shell hook and login item
  uninstall    Remove everything Continuum added
  doctor       Check agents, hooks, terminals and rules
  diagnose     Write a report for troubleshooting or a bug report
  status       Show the daemon, hooks and scheduled resumes
  version      Print the version

Sessions and resumes:
  sessions     List open agent sessions and whether each is covered
  list         Show scheduled and recent resumes
  cancel       Cancel scheduled resumes
  resume-now   Resume now instead of waiting for the reset

All or chosen sessions:
  scope        Resume every session, or only the ones you choose
  choose       Choose sessions to resume (scope "chosen")
  unchoose     Remove sessions from the chosen list
  exclude      Never resume these sessions, whatever the scope
  include      Undo exclude

Tuning:
  grace        How long to let an agent resume by itself first
  detect       Show what Continuum makes of a message
  rules        Print the effective rules

Running agents:
  run          Run an agent under Continuum's terminal wrapper
  shell-init   Print the shell functions that wrap installed agents

Background daemon:
  watch        Run Continuum in this terminal, showing what it does
  logs         Show or follow what the background daemon is doing
  start        Start the background daemon
  stop         Stop the background daemon
  daemon       Run the daemon in the foreground (for debugging)

Getting started:
  continuum install --dry-run     see what install would change
  continuum install               set everything up
  continuum doctor                check it

Files live in ~/.continuum (set CONTINUUM_HOME to move them): config.json,
scope.json, rules.json (overrides), history.jsonl, daemon.log.
Documentation: README.md, and docs/ in the source repository.
```

## continuum

```text
continuum: no help for "continuum"

continuum - work never loses its momentum

Keeps AI coding agents (Claude Code, Codex, Gemini CLI, opencode, Aider, Cursor,
Copilot and more) working through usage limits and API outages. When an agent
stalls, Continuum waits for the limit to reset and continues the same session,
in place: never a copy, never a new session. Works in any terminal, with the
window unfocused or the screen locked.

Usage:
  continuum <command> [options] [arguments]
  continuum <command> -h        help for a command

Set up:
  install      Hook Continuum into your agents, shell and login items
  update       Apply this version: refresh hooks, shell hook and login item
  uninstall    Remove everything Continuum added
  doctor       Check agents, hooks, terminals and rules
  diagnose     Write a report for troubleshooting or a bug report
  status       Show the daemon, hooks and scheduled resumes
  version      Print the version

Sessions and resumes:
  sessions     List open agent sessions and whether each is covered
  list         Show scheduled and recent resumes
  cancel       Cancel scheduled resumes
  resume-now   Resume now instead of waiting for the reset

All or chosen sessions:
  scope        Resume every session, or only the ones you choose
  choose       Choose sessions to resume (scope "chosen")
  unchoose     Remove sessions from the chosen list
  exclude      Never resume these sessions, whatever the scope
  include      Undo exclude

Tuning:
  grace        How long to let an agent resume by itself first
  detect       Show what Continuum makes of a message
  rules        Print the effective rules

Running agents:
  run          Run an agent under Continuum's terminal wrapper
  shell-init   Print the shell functions that wrap installed agents

Background daemon:
  watch        Run Continuum in this terminal, showing what it does
  logs         Show or follow what the background daemon is doing
  start        Start the background daemon
  stop         Stop the background daemon
  daemon       Run the daemon in the foreground (for debugging)

Getting started:
  continuum install --dry-run     see what install would change
  continuum install               set everything up
  continuum doctor                check it

Files live in ~/.continuum (set CONTINUUM_HOME to move them): config.json,
scope.json, rules.json (overrides), history.jsonl, daemon.log.
Documentation: README.md, and docs/ in the source repository.
```
