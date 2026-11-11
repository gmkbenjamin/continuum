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

`install` first lists everything it will create or change, where it is and what it's
for, and asks before doing anything. Run `continuum doctor` afterwards to check that
everything works. Open a new terminal tab to start using it.

## Using it

There's nothing to do: when a limit resets, the session continues by itself, and you
get a notification. When you want to look:

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
