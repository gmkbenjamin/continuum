# Continuum

**Work never loses its momentum.**

Continuum keeps AI coding agents working through usage limits and API outages. When
Claude Code, Codex, Gemini CLI or another agent stalls on a limit, Continuum notes when
the limit resets, waits, checks the session is still stalled, and continues it.

- **The same session, in place.** It types into the session that stalled. It never
  starts a new session or a second copy.
- **Any terminal, any state.** Terminal, iTerm2, Ghostty, VS Code, tmux, screen,
  zellij, herdr, Rex and more. It works with the window minimised, hidden or unfocused,
  over SSH, and with the screen locked.
- **About 25 agents.** Claude Code, Codex, Gemini CLI, Qwen Code, Aider, opencode, Grok,
  Cursor CLI, GitHub Copilot CLI, Amp, Goose, Factory Droid, Kiro, Auggie, Cline,
  Continue, Crush, Kimi, OpenHands, Mistral Vibe, Kilo Code, Letta, Plandex, Codebuff,
  Junie, Pi, Rovo Dev. Each is a few lines of rules, so new ones are easy to add.
- **All sessions, or only the ones you choose.**
- **macOS, Linux and Windows.** One static binary with no runtime dependencies.

```
You've hit your session limit · resets 10am (Australia/Sydney)
                                  ⋮   (Continuum waits)
10:01  › Your usage limit has reset. Continue the task you were working on…
```

## Contents

- [Install](#install)
- [Everyday use](#everyday-use)
- [Supported agents](#supported-agents)
- [Terminals and multiplexers](#terminals-and-multiplexers)
- [All sessions, or only chosen ones](#all-sessions-or-only-chosen-ones)
- [How it works](#how-it-works)
- [Never a copy, never a new session](#never-a-copy-never-a-new-session)
- [Waiting for the agent's own resume](#waiting-for-the-agents-own-resume)
- [Locked screen, sleep and power](#locked-screen-sleep-and-power)
- [Safety](#safety)
- [Staying compatible with agent updates](#staying-compatible-with-agent-updates)
- [Configuration](#configuration)
- [Commands](#commands)
- [Files](#files)
- [Limitations](#limitations)
- [Development](#development)

## Install

You need Go 1.23 or later to build from source.

```sh
git clone <this repo> continuum && cd continuum
make build                       # or: go build -o continuum ./cmd/continuum
./continuum install --dry-run    # see what install would change
./continuum install              # set everything up
continuum doctor                 # check it
```

`install` sets up four things, and running it again updates them in place:

1. **The binary.** It's copied to `~/.continuum/bin/continuum`, and linked into
   `~/.local/bin` when that folder is on your `PATH`.
2. **Agent hooks.** They're added to `~/.claude/settings.json` and
   `~/.codex/hooks.json`, so Claude Code and Codex report stalls and where they run.
   Every other setting and hook is kept, and a timestamped backup is saved first.
3. **A shell hook.** A marked block in `~/.zshrc`, `~/.bashrc` or fish's `conf.d` runs
   interactive agent sessions under Continuum's terminal wrapper, so it can type into
   them in any terminal. See [Terminals and multiplexers](#terminals-and-multiplexers).
4. **A login item.** The daemon starts at login: a launchd agent on macOS, a systemd
   user unit on Linux, a scheduled task on Windows.

Use `--no-shell` or `--no-service` to skip a step, and `--only claude,codex` to choose
which agents get hooks. `continuum uninstall` reverses everything and returns the
agents' settings files to their original bytes.

Continuum never resumes limits that were hit before it was installed. Agents already
running when you install are watched through their transcripts. Open a new terminal tab
so the shell hook loads for new sessions.

## Everyday use

```sh
continuum status              # daemon, hooks, scheduled resumes
continuum sessions            # open agent sessions, and whether each is covered
continuum list                # what is waiting, when, and where it will be typed
continuum resume-now <id>     # don't wait (e.g. you bought extra usage)
continuum cancel <id|all>     # leave a session stalled
continuum grace claude off    # don't wait for Claude's own resume (default: 5 min)
continuum doctor              # after updating an agent
```

You get a desktop notification when a session is paused, resumed, or needs you. Turn
these off with `"notify": false`. Every command explains itself with `-h`, for example
`continuum choose -h`. The full reference is in [docs/cli.md](docs/cli.md).

## Supported agents

| | Detects stalls from | Resumes |
|---|---|---|
| **Claude Code** (CLI, IDE, Desktop) | hooks, transcripts, its session list, the screen | in place; a closed session by its id |
| **Codex** (CLI, app) | hooks, rollout files (exact reset time), the screen | in place; a closed session by its id |
| **About 25 others** | the screen, through the wrapper or a tmux pane | in place |

[docs/agents.md](docs/agents.md) lists every agent, how each is recognised, and how to
add one. In short:

- **Agent-specific wording.** Each agent's own limit wording is recognised, as is the
  wording most agents share: "quota exceeded", "rate limit reached", "try again in 2
  hours", `RESOURCE_EXHAUSTED`, and so on.
- **Agents under an interpreter.** Agents that run under Node, Bun or Python are
  recognised by their command line, even though tmux only shows `node`.
- **Agents with common names.** Agents whose command name other tools also use (`amp`,
  `goose`, `pi`, `cn`, `droid`, `crush`, `vibe`) are opt-in for the shell hook. Add them
  with `"shell_hook_agents"`.
- **Everything is data.** Each agent is an entry in
  [`default_rules.json`](internal/rules/default_rules.json). Fix or add one in
  `~/.continuum/rules.json`, with no rebuild.

## Terminals and multiplexers

Continuum never sends keystrokes to a window. It types through one of these routes,
so focus, minimised windows and the lock screen don't matter.

- **The wrapper** (`continuum run`, applied automatically by the shell hook). It works in
  any terminal emulator: Terminal.app, iTerm2, Ghostty, Alacritty, WezTerm, kitty,
  Windows Terminal, VS Code, JetBrains, and plain SSH. The agent runs in a terminal
  Continuum owns, and all input and output pass through unchanged: colours, mouse,
  resizing, paste, Ctrl+C, and Ctrl+Z with `fg`.
- **Multiplexer panes:** tmux, GNU screen, zellij, herdr, Rex, WezTerm and kitty (the
  last two through their CLIs), including detached sessions.
- **The agent's own commands**, for sessions that are closed (see below).

The shell hook only wraps interactive sessions. Print mode (`claude -p`, `gemini -p`),
`--version`, subcommands such as `codex exec` or `opencode run`, pipes and redirects
all run untouched. To skip it once, use `CONTINUUM_NO_WRAP=1 claude` or
`command claude`. The hook picks up agents you install later without re-running
`install`.

## All sessions, or only chosen ones

By default every session is covered. To cover only some:

```sh
continuum scope chosen              # only chosen sessions from now on
continuum sessions                  # open sessions with ids and names
continuum choose api-refactor 4f2c1d9e  # by session name or id (prefix)
continuum choose ~/work/api         # every session started in this folder
CONTINUUM_CHOOSE=1 claude           # choose a session as you start it
continuum run --choose -- gemini    # same, explicitly
continuum unchoose api-refactor         # stop choosing it
continuum exclude ~/scratch         # never resume these, whatever the scope
continuum scope all                 # back to every session
```

Changes apply within seconds. Choosing a session that's already waiting on a limit
schedules its resume, and unchoosing one cancels its pending resume.

## How it works

```
  observers                                   daemon                     delivery (first that works)
  ─────────                                   ──────                     ───────────────────────────
  Claude Code hooks (StopFailure, Stop, …)  ┐                            1. continuum run wrapper → PTY
  Claude transcripts  ~/.claude/projects    │   one job per stalled      2. multiplexer pane
  Codex rollouts      ~/.codex/sessions     ├─▶ session: scope, reset ─▶    tmux · screen · zellij · herdr
  Codex hooks (SessionStart, Stop)          │   time, "is it open?",        Rex · WezTerm · kitty
  wrapper (screen of any wrapped agent)     │   activity check, retries  3. agent commands, same session:
  tmux pane scan (any agent)                ┘                               open background session → claude attach
                                                                            closed session → claude --bg --resume <id>
                                                                                             codex queue --thread <id> --resume
```

**Detection.** Several independent sources each report a stall on their own, so losing
one doesn't stop Continuum:

- Claude Code writes the error into its transcript (`isApiErrorMessage`,
  `error: "rate_limit"`) and fires a `StopFailure` hook.
- Codex records `usage_limit_exceeded`, plus the exact `resets_at` of the exhausted
  window, in its rollout file.
- The wrapper and the tmux pane scanner read the bottom of the agent's screen. That's
  how every other agent is covered.
- The desktop apps write the same transcript and rollout files, so Claude Desktop and
  Codex app sessions are seen too.

**Reset times** come from the exact time when the agent gives one (Codex), or from the
wording: "resets 10am (Australia/Sydney)", "try again at 5:53 PM", "in 2 hours 13
minutes", "in 30s", "resets Nov 14, 6pm", ISO times and weekdays. If no time can be
found, Continuum retries with backoff. Transient errors (529 overloaded, 5xx, dropped
streams) get a short backoff.

**Before typing**, Continuum checks four things:

1. **Nobody has continued yet.** New activity in the transcript means you or the agent
   resumed it. So does Claude reporting the session busy since after the limit hit.
2. **It's still the conversation that stalled.** If the agent's window has moved on to
   another conversation (`/clear`, `/resume`), Continuum leaves the old one alone.
3. **The pane still shows the agent.** It checks the foreground process, or the agent's
   own screen markers, so a prompt never lands in a shell.
4. **No menu or approval prompt is open.** Enter or a typed letter could pick an option.
   In testing, Codex showed an "Update available" menu where Enter means *Update now*.
   Grok's folder-trust screen takes a single `y`, and a Claude background session
   showed "Resume this conversation?". Continuum refuses all of these, then uses another
   route or retries later.

## Never a copy, never a new session

Continuum only ever continues the original session:

- **The session is open** in a terminal, pane or the wrapper: Continuum types into it
  there. For a Claude background session (`claude --bg`), it opens the session with
  `claude attach` in a hidden terminal, types, and closes that terminal. The session
  keeps running.
- **The session is closed** (the agent exited, or a background session isn't running):
  Continuum continues it under its own id with `claude --bg --resume <id>` or
  `codex queue --thread <id> --resume`. It's the same conversation, and nothing else
  has it open, so nothing is copied.
- **It's open somewhere Continuum can't type**, such as an agent started in a plain
  terminal before the shell hook was installed, or a session open in a desktop app.
  Continuum doesn't start a copy. It notifies you, and keeps retrying in case the session
  becomes reachable.

Continuum learns whether a session is open from the agent itself. It uses
`claude agents --json`, the running-session files Claude keeps, the process recorded by
the hook, and a process scan for the other agents. If none of these can answer,
Continuum assumes the session is open. Set `"allow_copies": true` to allow the CLI route
anyway.

## Waiting for the agent's own resume

Claude Code tries to resume by itself after a reset while it's running, so Continuum
first gives it 5 minutes past the reset. That way the two never both continue the
session. Codex gets 30 seconds. The wait is skipped when the agent says it *won't*
resume (for example "Claude Code exited during the wait…"), or its process is gone.
Change it with one command; sessions already waiting are re-planned straight away:

```sh
continuum grace                  # show the current waits
continuum grace claude 10        # wait 10 minutes
continuum grace claude off       # continue right after the reset
continuum grace claude default   # back to 5 minutes
```

Apart from this wait, Continuum acts `delay_after_reset_seconds` (default 60) after the
reset. That margin allows for rounded reset times and clock differences. A resume
that's too early hits the limit again and is retried after 15 minutes.

## Locked screen, sleep and power

Nothing Continuum does needs the display, so a locked screen doesn't stop it. A
*sleeping* computer runs nothing, though. While a resume is due within
`keep_awake_hours` (default 8), and while a resumed session is still working, Continuum
asks the OS not to idle-sleep:

- macOS: `caffeinate -i -s`
- Linux: `systemd-inhibit --what=idle:sleep`
- Windows: `SetThreadExecutionState`

It releases that as soon as nothing is pending. The login service runs as a normal,
unthrottled background process. Closing a laptop lid on battery still sleeps; resumes
that came due while asleep run as soon as the computer wakes.

## Safety

- **Nothing from before installation is revived.** After the daemon has been off, at
  most 12 hours (`catch_up_hours`) are caught up.
- **No unsafe typing.** It never types into menus, approval prompts or plain shells, and
  never starts a copy of an open session.
- **Billing stops for you.** Billing, credit, spend-cap, sign-in and API-key errors are
  marked **needs you** and never retried.
- **One stall, one resume,** even when the hook, transcript and screen all report it.
  It never types twice into the same pane for one stall.
- **Limits on retries.** It gives up after 8 attempts (`max_attempts`), and doesn't
  schedule resets further away than `max_wait_hours` (default 8 days).
- **Dry runs.** `"dry_run": true` logs what would happen without typing or running
  anything. Everything is recorded in `~/.continuum/history.jsonl`.
- **Hooks can't break an agent.** They take about 4 ms, print nothing, and always exit
  0, even on malformed input.

## Staying compatible with agent updates

Nothing can promise that a future agent release won't change something. Continuum is
built so most changes are absorbed, and the rest are visible and quick to fix:

- **Stable interfaces first.** Documented hooks, documented CLI flags (`--resume`,
  `--bg`, `claude agents --json`, `codex queue`), and the session files the agents
  already keep. Screen reading is a fallback for those two, and the main source only
  for agents that offer nothing else.
- **Wording, flags and commands are data.** Every message pattern, CLI flag, menu
  pattern and multiplexer command lives in
  [`default_rules.json`](internal/rules/default_rules.json). Override any of it in
  `~/.continuum/rules.json` without rebuilding (`continuum rules --defaults` prints a
  starting point). A broken override is reported, and the built-in rules keep working.
- **Capability probing.** Before using an agent command, Continuum checks the installed
  agent's `--help` for the flags it needs. If a release drops `--bg`, it falls back to
  the next command instead of failing.
- **Lenient parsing.** Unknown fields in hooks, transcripts and session lists are
  ignored, and missing ones are tolerated.
- **Drift shows up.** `continuum doctor` re-checks real limit messages against the
  rules, checks which resume commands the installed agents still support, and checks
  the hooks.

## Configuration

`~/.continuum/config.json`. Every key is optional.

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `true` | Turn Continuum off without uninstalling. |
| `dry_run` | `false` | Log what would happen; type and run nothing. |
| `harnesses` | all on | Turn agents on or off: `{"aider": false}`. |
| `resume_prompts` | built in | Prompt per agent: `{"claude": "Limit reset. Carry on."}`. Use `"<agent>_transient"` for API-error resumes. |
| `delay_after_reset_seconds` | `60` | Margin after the reset time. |
| `native_grace_seconds` | Claude 300, Codex 30 | Wait for the agent's own resume first. Easier to set with `continuum grace`. |
| `keep_awake_hours` | `8` | Hold off idle sleep while a resume is due this soon; `0` turns it off. |
| `allow_copies` | `false` | Let the agent CLI continue a session that's still open elsewhere (starts a copy). |
| `allow_headless` | `true` | `false`: only type into wrappers and panes, never run agent commands. |
| `strategies` | `["wrapper","multiplexer","native"]` | Delivery routes, in order. |
| `shell_hook_agents` | installed agents | Commands the shell hook wraps (replaces the default list). |
| `max_wait_hours` | `192` | Don't schedule resets further away than this. |
| `catch_up_hours` | `12` | How far back to catch up after the daemon was off. |
| `notify` | `true` | Desktop notifications. |
| `watch_transcripts` | `true` | Read Claude transcripts and Codex rollouts. |
| `scan_panes` | `true` | Scan tmux panes for other agents. |
| `poll_seconds` | `5` | How often the daemon checks. |
| `binaries` | from `PATH` | Agent executables: `{"claude": "/opt/claude"}`. |
| `claude_home`, `codex_home` | `~/.claude`, `~/.codex` | Where the agents keep their files. |
| `reset_hints_command` | none | A command printing `{"claude": "<RFC3339>"}`, used when a message gives no reset time. |
| `autostart_daemon` | `true` | Start the daemon from hooks and `run` if it isn't running. |

The scope is in `~/.continuum/scope.json` (use `continuum scope` and `choose`). Rules
overrides go in `~/.continuum/rules.json` (see [docs/agents.md](docs/agents.md)).

## Commands

| Command | What it does |
|---|---|
| `install`, `uninstall` | Set up or remove hooks, the shell hook and the login item |
| `doctor`, `status`, `version` | Check everything, show state |
| `sessions`, `list` | Open sessions; scheduled and recent resumes |
| `resume-now`, `cancel` | Resume a stalled session now, or leave it stalled |
| `scope`, `choose`, `unchoose`, `exclude`, `include` | All or chosen sessions |
| `grace` | How long to wait for an agent's own resume |
| `detect`, `rules` | Test a message against the rules; print the rules or the agent list |
| `run`, `shell-init` | Run an agent under the wrapper; print the shell hook |
| `start`, `stop`, `daemon` | Control the background daemon |

`continuum -h` lists them, and `continuum <command> -h` explains one with all its
options and examples. [docs/cli.md](docs/cli.md) has the full reference.

## Files

Everything is in `~/.continuum`; set `CONTINUUM_HOME` to move it.

| File | Contents |
|---|---|
| `config.json` | Your settings |
| `scope.json` | All or chosen sessions, chosen and excluded entries |
| `rules.json` | Your rules overrides (optional) |
| `jobs.json`, `sessions.json`, `offsets.json` | Scheduler state |
| `history.jsonl` | Every scheduled, delivered, skipped and failed resume |
| `daemon.log` | Daemon log |
| `wrappers/` | One folder per running `continuum run` |
| `bin/continuum` | The installed binary |

## Limitations

- **Sleep.** A sleeping computer runs nothing. Continuum keeps it awake while a resume
  is close, but a closed lid on battery still sleeps. Overdue resumes run when it
  wakes.
- **Sessions Continuum can't reach in place.** It can't type into a session already
  running in a plain terminal before the shell hook was installed, or one open in the
  Claude Desktop or Codex app window. It notifies you instead of starting a copy.
- **Editor-based agents.** Agents inside an editor (Cursor or Windsurf editors, VS Code
  Copilot Chat, Cline or Roo in VS Code, Zed) have no terminal to type into.
- **Confirmation menus.** If an agent shows one (for example Claude's "Resume this
  conversation?"), Continuum won't press Enter for you. It notifies you and retries.
- **Untested wording.** For agents other than Claude Code, Codex, opencode and Grok, the
  limit wording comes from their public messages and hasn't been checked against a real
  limit. The shared patterns cover most variations. Corrections go in `rules.json`.
- **Two wrapped sessions in one folder.** If an agent in the wrapper has no hooks, and
  two sessions of it stall in the same directory within 3 minutes, Continuum may
  attach a session to the wrong wrapper.
- **Platforms.** The WezTerm and kitty adapters follow their documented CLIs but weren't
  available to test. The Windows build (ConPTY wrapper, scheduled task, notifications,
  keep-awake) compiles but hasn't been run on Windows, and has no shell hook: use
  `continuum run`.

## Development

```sh
make test          # unit tests
make vet           # go vet for macOS, Linux and Windows
make integration   # real tmux, screen, zellij, herdr, Rex, a Node agent and the wrapper, all detached
make real-agents   # starts the real Claude Code and Codex TUIs in hidden tmux and types
                   # into them without pressing Enter (uses no model quota)
make docs          # regenerate docs/cli.md and docs/agents.md from the binary
make dist          # binaries for macOS, Linux, Windows (amd64 + arm64)
```

Layout:

| Folder | Purpose |
|---|---|
| `cmd/continuum` | CLI and help |
| `internal/rules` | data-driven knowledge of agents and multiplexers |
| `internal/resettime` | reset-time parsing |
| `internal/sources` | Claude transcripts and Codex rollouts |
| `internal/hooks` | agent hook handler and installer |
| `internal/daemon` | scheduler |
| `internal/deliver` | delivery routes and safety checks |
| `internal/live` | is a session open? |
| `internal/scope` | all or chosen sessions |
| `internal/awake` | keep-awake |
| `internal/mux` | multiplexer adapters |
| `internal/wrapper` | PTY / ConPTY wrapper with job control |
| `internal/service` | login items |
