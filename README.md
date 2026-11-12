# Continuum

**Your AI coding agent hit its usage limit? Continuum picks the work back up the moment the limit resets.**

Continuum watches Claude Code, Codex and other AI coding agents. When one stalls on a
usage limit or an API outage, it waits for the reset and continues **the same
session, in place**. It never starts a new session or a second copy, and it doesn't
need the window to be focused, visible or even unlocked.

```
You've hit your session limit · resets 10am
                                  ⋮   (Continuum waits)
10:01  › Your usage limit has reset. Continue the task you were working on…
```

## Features

- **Same session, in place.** It types into the stalled session itself, and never
  starts a duplicate.
- **Works however the session is open:** focused, minimised, hidden, in the
  background, over SSH, or with the screen locked.
- **Any terminal:** Terminal, iTerm2, Ghostty, VS Code, tmux, screen, zellij, and more.
- **About 25 agents:** Claude Code (CLI and Desktop), Codex (CLI and app), Gemini CLI,
  Aider, opencode, Cursor CLI, GitHub Copilot CLI, and more.
- **All sessions or only the ones you choose.**
- **Safe:** it never types into a menu or approval prompt, and never retries billing or
  sign-in errors.
- **Local only:** everything runs on your computer and nothing is sent anywhere.
- **macOS, Linux and Windows.** One small binary.

## Install

You need [Go](https://go.dev/dl/) 1.23 or later.

```sh
git clone https://github.com/gmkbenjamin/continuum.git
cd continuum
make build
./continuum install
```

`install` asks how Continuum should run:

- **In the background** (recommended): a small daemon starts at login, so sessions
  resume even when no terminal is open.
- **Standalone:** nothing runs in the background. Continuum works while
  `continuum watch` is open in a terminal, and prints what it does.

`--background` or `--standalone` choose without asking. Then `install` lists everything
it will create or change, where it is and what it's for, and asks before doing
anything (see [What it installs](#what-it-installs)). Run `continuum doctor` afterwards
to check that everything works, and open a new terminal tab to start using it.

## How it works

1. **Notice the stall.** When an agent hits a limit, Continuum learns about it from up
   to three places:
   - the agent's hook (Claude Code and Codex call `continuum hook` on session events);
   - the session files the agent already writes (`~/.claude/projects`,
     `~/.codex/sessions`);
   - the bottom of the agent's screen, for every other agent.

   It reads the reset time from the message, for example "resets 10am" or "try again
   in 2h".
2. **Wait.** It schedules a resume one minute after the reset. For Claude Code it waits
   5 minutes after the reset instead, in case Claude resumes on its own. Change this
   with `continuum grace`.
3. **Check.** Just before acting, it checks four things:
   - the session hasn't already continued;
   - the agent is still the program in that window;
   - no menu or approval prompt is open;
   - the session is in scope.

   If any check fails, it waits and tries again later.
4. **Continue.** It types a short "your limit has reset, continue" message into the
   same session. It does this through its own terminal wrapper or the multiplexer pane
   (tmux and others), so the window doesn't need focus. A session that has been closed
   is continued under its own id with the agent's own command, for example
   `claude --bg --resume <id>`.

More detail is in [docs/how-it-works.md](docs/how-it-works.md).

## What it installs

Everything is under your user account. Nothing needs admin rights, and nothing is sent
over the network.

| What | Where | Why |
|---|---|---|
| The program | `~/.continuum/bin/continuum` | Run by everything below. |
| Command link | `~/.local/bin/continuum` (only if that folder is on your `PATH`) | So `continuum` works in any terminal. |
| Data folder | `~/.continuum/` | Settings (`config.json`, `scope.json`), scheduled resumes, `history.jsonl`, `daemon.log`, and a record of what was installed. |
| Claude Code hooks | `~/.claude/settings.json`: `SessionStart`, `StopFailure`, `Stop`, `SessionEnd` | Claude Code reports stalls and where each session runs. This also covers Claude Desktop. |
| Codex hooks | `~/.codex/hooks.json`: `SessionStart`, `Stop` | Codex reports where each session runs. |
| Shell hook | A marked block in `~/.zshrc`, `~/.bashrc` or fish's `conf.d/continuum.fish` | Runs interactive `claude`, `codex` and other installed agents under the wrapper, so Continuum can type into them. |
| Login item (background mode only) | macOS: `~/Library/LaunchAgents/dev.continuum.daemon.plist`. Linux: `~/.config/systemd/user/continuum.service`. Windows: scheduled task "Continuum". | Starts the background daemon when you log in. |
| Backups | `<file>.continuum-backup-<date-time>` next to each file it changes | Your file as it was before Continuum touched it. |

Each hook is one line, `/bin/sh -c '"~/.continuum/bin/continuum" hook claude 2>/dev/null
|| true'`. Your other settings and hooks are left alone. `continuum uninstall` removes
all of this, and `--purge` also removes the data folder and the backups.

## What runs in the background

In standalone mode, nothing does: the work happens while `continuum watch` is open, and
hooks only queue events for it. In background mode:

- **One daemon**, `continuum daemon`, started at login. It uses about 25 MB of memory
  and well under 1% of one CPU core. It:
  - collects events from hooks every 5 seconds;
  - reads new lines of the agents' session files every 10 seconds;
  - looks at tmux panes running other agents every 15 seconds;
  - checks the hooks are intact, and whether an agent was updated, every 10 minutes.

  It types or runs anything only when a resume is due.
- **Hooks**, run by Claude Code and Codex on session events. Each takes a few
  milliseconds, drops a small event file in `~/.continuum/spool/`, and starts the
  daemon if it isn't running.
- **The wrapper**, around each agent started from your shell: two small processes,
  `continuum run` and `continuum __pty-helper`, which pass everything through. They
  exit when the agent does.
- **Keep-awake**, only while a resume is less than 8 hours away or a resumed session is
  still working. On macOS it's a `caffeinate` process, on Linux `systemd-inhibit`, and
  on Windows a system flag. It's released as soon as nothing is pending.

To check what it's doing, run `continuum status` and `continuum list`, or follow the
log with `continuum logs -f`. `continuum stop` stops the daemon until the next login.

## Troubleshooting

```sh
continuum doctor                   # checks everything and says what to fix
continuum watch --debug            # run in this terminal and show every decision
continuum logs -f                  # follow the background daemon's log
continuum diagnose -o report.md    # a report for a bug report, personal details removed
```

For detailed logs from the background daemon, add `"log_level": "debug"` to
`~/.continuum/config.json`. The daemon then records why it did each thing: every event,
every check, and every delivery route tried. Hooks also log one line per call to
`~/.continuum/hooks.log`. Logs rotate at 5 MB.

## Using it

There's nothing to do: when a limit resets, the session continues by itself, and you
get a notification. Run `continuum` on its own for a menu of everything below, or use
the commands:

```sh
continuum status            # is it running, and what is waiting
continuum sessions          # open agent sessions, and whether each is covered
continuum list              # scheduled and recent resumes
continuum resume-now <id>   # don't wait for the reset
continuum cancel <id>       # leave a session stalled
continuum -h                # every command; `continuum <command> -h` for details
```

## Choosing sessions

Every open session is covered by default. To cover only some:

```sh
continuum scope chosen           # only sessions you choose
continuum choose <name|id>       # a session (see `continuum sessions`)
continuum choose ~/work/api      # every session in a folder
CONTINUUM_CHOOSE=1 claude        # choose a session as you start it
continuum exclude ~/scratch      # never resume these
continuum scope all              # back to every session
```

## Resume messages

By default Continuum types "Your usage limit has reset. Continue the task you were
working on…". You can change that for everything, for one agent, for a folder, or for
a single session. The most specific message wins.

```sh
continuum message set "Limit reset. Re-read the plan, then continue in {folder}."
continuum message set --agent codex "Carry on with the current task."
continuum message set --session api-refactor "Finish the migration only, then stop."
continuum message set --folder ~/work/docs "Continue editing; don't commit."
continuum message                     # see every message, and what each session gets
CONTINUUM_MESSAGE="Only fix the failing test." claude    # set it as you start a session
```

`--outage` sets the message used after a temporary API error instead.

## Where it works

- **Any terminal:** a small shell hook runs your agents under Continuum's pass-through
  wrapper, so it can type into them. Colours, mouse, paste, Ctrl+C and Ctrl+Z all
  behave as usual. To skip it once, use `CONTINUUM_NO_WRAP=1 claude`.
- **Multiplexers:** tmux, screen, zellij, herdr, Rex, WezTerm and kitty, including
  detached sessions.
- **Ghostty:** agents already open in a plain Ghostty tab, found and typed into without
  focusing the window.
- **Claude Desktop:** sessions continue by themselves once Desktop releases them.
  Continuum never copies a session Desktop may still have open.
- **Codex app:** sessions continue inside the app.

## Supported agents

| Agent | Coverage |
|---|---|
| Claude Code (CLI, IDE, Desktop), Codex (CLI, app) | Full: hooks, session files and screen |
| Gemini CLI, Qwen Code, Aider, opencode, Grok CLI, Cursor CLI, GitHub Copilot CLI, Auggie, Cline, Kimi, OpenHands, Kilo Code, Letta, Plandex, Codebuff, Junie, Kiro | Screen: through the wrapper or a tmux pane |
| Amp, Goose, Factory Droid, Continue, Crush, Mistral Vibe, Pi, Rovo Dev | Screen, with the shell hook opt-in (their command names are shared with other tools) |

See [docs/agents.md](docs/agents.md) for details, and for how to add an agent or correct
one's wording without rebuilding.

## Settings

Everything works without configuration. The settings people change most are these:

```sh
continuum grace claude off   # don't wait for Claude's own auto-resume (default: 5 min)
```

```jsonc
// ~/.continuum/config.json
{
  "resume_prompts": { "claude": "Limit reset. Carry on with the plan." },
  "notify": true,            // desktop notifications
  "keep_awake_hours": 8,     // keep the computer awake while a resume is near (0 = off)
  "dry_run": false           // log what would happen, change nothing
}
```

All settings are in [docs/configuration.md](docs/configuration.md).

## Updating and uninstalling

```sh
git pull && make build && ./continuum update   # apply a new version, keeping your choices
continuum uninstall                            # remove everything; keep your settings
continuum uninstall --purge                    # remove everything, including settings and backups
```

Both commands show what they'll change and ask first. Uninstalling puts your agents'
settings files back exactly as they were. If Continuum is deleted without
uninstalling, its hooks do nothing and cause no errors.

## Limitations

- **Sleep.** A sleeping computer can't resume anything. Continuum keeps it awake while
  a resume is near, but closing a laptop lid on battery still sleeps it. Overdue
  resumes run when it wakes.
- **Editor-based agents** (Cursor or Windsurf editors, VS Code Copilot Chat, Cline in
  VS Code) have no terminal Continuum can type into.
- **Open where Continuum can't type.** If a session is open somewhere Continuum can't
  type into, such as a plain terminal started before installing, you get a
  notification instead of a duplicate session.
- **Untested.** Windows, WezTerm, kitty and Ghostty support are built but not yet
  tested on real systems. Limit wording for agents other than Claude Code, Codex,
  opencode and Grok hasn't been seen at a real limit.

## Documentation

- [How it works](docs/how-it-works.md): detection, safety checks, desktop apps, sleep,
  and staying compatible with agent updates
- [Configuration](docs/configuration.md): every setting and file
- [Supported agents](docs/agents.md): coverage per agent, and adding your own
- [Command reference](docs/cli.md): all commands and options
- [Changelog](CHANGELOG.md)

## Building from source

```sh
make build         # build for this computer (./continuum links to it)
make test          # unit tests
make integration   # end-to-end tests in real tmux, screen, zellij, herdr, Rex
make dist          # binaries for macOS, Linux and Windows, in dist/
make docs          # regenerate docs/cli.md and docs/agents.md
```
