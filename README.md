# Continuum

**When your AI coding agent hits its usage limit, Continuum picks the work back up
after the limit resets.**

Continuum watches Claude Code, Codex, Grok Build, Muse Code, Cursor CLI, opencode and
about 25 other coding agents. When
one stops on a usage limit or a temporary API error, it waits for the reset and
continues **the same session, in place**. It never starts a new session or a copy, and
the window doesn't need to be focused, visible or even unlocked.

```
You've hit your session limit · resets 10am
                                  ⋮   (Continuum waits)
10:01  › Your usage limit has reset. Continue the task you were working on…
```

- **Any terminal:** Terminal, iTerm2, Ghostty, VS Code, tmux, screen, zellij, Rex, and more.
- **Safe:** it types only into the session's own agent, never into a shell, an editor, a
  menu or an approval prompt, and it never retries billing or sign-in errors.
- **Your agents can control it:** ask one "don't auto-resume this session" or "when you
  resume, re-run the tests first".
- **Local:** nothing is sent anywhere. macOS, Linux and Windows.

## Queued prompts and usage resets

Some agents let you queue follow-up prompts while a turn is running. Codex CLI and
GitHub Copilot CLI queue these follow-ups for the next turn. Whether a queued prompt
waits for a usage reset depends on the client.
[Codex CLI queue](https://learn.chatgpt.com/docs/cli/reference) ·
[GitHub Copilot CLI queue](https://docs.github.com/en/copilot/how-tos/copilot-cli/use-copilot-cli/steer-agents#queue-a-prompt-to-be-processed-next)

Claude Code can wait in an eligible interactive session signed in with a claude.ai
subscription and continue the interrupted task after the reset. Keep the session
open for this to work. If you send a new prompt during the wait, Claude Code submits
it immediately and starts waiting again if it hits the limit.
[Claude Code usage-limit behavior](https://code.claude.com/docs/en/interactive-mode#wait-for-a-usage-limit-to-reset)

Continuum schedules a resume for a stalled session and skips it if the session has
already continued. For Claude Code, it gives the running agent five minutes by
default after a known reset to continue on its own. [Grace period settings](docs/cli.md#grace)

## Install

Download the file for your computer from the
[latest release](https://github.com/gmkbenjamin/continuum/releases/latest)
(`continuum-darwin-arm64` for an Apple-silicon Mac, `-darwin-amd64`, `-linux-amd64`,
`-linux-arm64`, `-windows-amd64.exe`, `-windows-arm64.exe`), then:

```sh
chmod +x continuum-darwin-arm64
xattr -d com.apple.quarantine continuum-darwin-arm64   # macOS: the download isn't notarized
./continuum-darwin-arm64 install
```

Or build it with [Go](https://go.dev/dl/) 1.26+: `git clone
https://github.com/gmkbenjamin/continuum.git && cd continuum && make build && ./continuum install`.

`install` lists everything it will change and asks first. Choose **background**
(recommended: starts at login) or **standalone** (works only while `continuum watch` is
open). Then open a new terminal tab.

## Use

There's nothing to do: when a limit resets, the session continues and you get a
notification. Run `continuum` on its own for a menu, or:

```sh
continuum status            # every session: what it's doing, where, whether it's covered
continuum list              # scheduled and recent resumes
continuum resume-now <id>   # don't wait for the reset (`all`: every waiting session)
continuum cancel <id>       # leave a session stalled
continuum doctor            # check everything
```

```sh
continuum stop              # stop until `continuum start`, or your next login
continuum start             # start again
continuum disable           # no auto-start at all (`continuum enable` undoes it)
```

`update` leaves a daemon you stopped stopped.

**Choose sessions.** Every session is covered unless you narrow it:

```sh
continuum scope chosen      # only sessions you choose…
continuum choose <name|folder>
continuum exclude <name|folder>   # …or never these
continuum scope all
```

**Resume messages.** Change what's typed, for everything or one agent, folder or
session (`{agent}` and `{folder}` are filled in):

```sh
continuum message set "Limit reset. Re-read the plan, then continue in {folder}."
continuum message set --session api-refactor "Finish the migration only, then stop."
```

## From your agent

Agents control Continuum through their shell, and `this` means their own session:

```sh
continuum this                    # this session's state and message
continuum exclude this            # don't auto-resume it
continuum message set --session this "Re-run the failing tests, then continue."
```

`install` gives Claude Code, Codex and Antigravity a Continuum skill (Grok Build, Muse
Code, the Cursor CLI and opencode read Claude's), so you can just ask. For other agents, tell them to run `continuum
agent-guide`.

## Supported agents

- **Fully:** Claude Code (CLI, IDE, Desktop), Codex (CLI, app), Grok Build, Muse Code.
- **With hooks:** Cursor CLI (`agent`), which reports its sessions; limits are read
  off its screen.
- **OpenCode:** terminal support and an optional shared server connection for the
  desktop app and CLI. [OpenCode setup](docs/agents.md#opencode-desktop-and-cli)
- **In any terminal:** Gemini CLI, Antigravity CLI, Qwen Code, Aider, GitHub
  Copilot CLI and more: [the full list](docs/agents.md).
- **Not covered:** agents inside an editor window (the Cursor, Windsurf and Antigravity
  editors' own agents, VS Code Copilot Chat). Their command-line agents work.

## Update and uninstall

```sh
./continuum-darwin-arm64 update   # a newly downloaded version, keeping your choices
continuum uninstall               # remove it all; keep your settings
continuum uninstall --purge       # and delete settings, history and backups
```

Both show what they'll change and ask first. A settings file you haven't changed since
install gets back its exact original bytes.

## Limitations

- A sleeping computer can't resume anything. Continuum keeps it awake while a resume is
  near, but closing a laptop lid on battery still sleeps it; overdue resumes run on wake.
- Continuum sends a notification when a session is still open and no delivery
  route can reach it.
- Windows, WezTerm, kitty and Ghostty support is built but not yet tested on real systems.

## More

[How it works](docs/how-it-works.md) · [Settings](docs/configuration.md) ·
[Agents](docs/agents.md) · [Commands](docs/cli.md) · [Changelog](CHANGELOG.md)

Build and test: `make build`, `make test`, `make integration`, `make dist`.
