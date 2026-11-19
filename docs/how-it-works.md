# How Continuum works

The details behind the [README](../README.md): how stalls are found, what is checked before anything is typed, and how Continuum stays safe and compatible.

## How it works

```
  observers                                   daemon                     delivery (first that works)
  ─────────                                   ──────                     ───────────────────────────
  Claude Code hooks (StopFailure, Stop, …)  ┐                            1. continuum run wrapper → PTY
  Claude transcripts  ~/.claude/projects    │   one job per stalled      2. multiplexer pane
  Codex rollouts      ~/.codex/sessions     ├─▶ session: scope, reset ─▶    tmux · screen · zellij · herdr
  Codex hooks (SessionStart, Stop)          │   time, "is it open?",        Rex · WezTerm · kitty
  Grok Build hooks, active_sessions.json    │
  Muse Code hooks, muse sessions --json     │
  Cursor CLI hooks                          │
  wrapper (screen of any wrapped agent)     │   activity check, retries  3. agent commands, same session:
  tmux pane scan (any agent)                ┘                               open background session → claude attach
                                                                            closed session → claude --bg --resume <id>
                                                                                             codex queue --thread <id> --resume
```

**Detection.** Several independent sources each report a stall on their own, so losing
one doesn't stop Continuum:

- Claude Code writes the error into its transcript (`isApiErrorMessage`,
  `error: "rate_limit"`) and fires a `StopFailure` hook.
- Claude Code's grace stop: on reaching the limit it may instead let the turn finish
  on a short allowance, with a note in the transcript (`usageLimitNote: "wrap_up"`)
  and no error. The end of that turn is the stall; the work it does meanwhile, and its
  `Stop` hook, aren't the session moving on.
- Codex records `usage_limit_exceeded`, plus the exact `resets_at` of the exhausted
  window, in its rollout file.
- Grok Build fires the same hooks (`StopFailure` with `rate_limit`, `server_error`, …),
  from `~/.grok/hooks/continuum.json`, and lists its open sessions in
  `~/.grok/active_sessions.json`. Grok also runs Claude Code's hooks; Continuum tells
  the two apart by the `GROK_*` variables Grok sets, so a Grok session is never taken
  for a Claude one.
- Muse Code fires Claude Code-style hooks (`StopFailure` with the error and its
  details) from `~/.config/muse/settings.json`, and `muse sessions --json` lists its
  open sessions. Muse runs hooks with a cleaned environment, so the hook reads the
  wrapper id or multiplexer pane from the Muse process itself.
- The Cursor CLI reports sessions starting and ending, and its replies, from
  `~/.cursor/hooks.json`; its limits are read off the screen. Cursor runs Claude
  Code's hooks too; those runs are ignored while Cursor's own is installed (the same
  goes for Grok Build).
- Hooks from runs nobody types into (`grok -p`, `muse exec`, `agent -p`) are ignored:
  the agent's own command line is judged by the same rules the shell hook uses.
- The wrapper and the tmux pane scanner read the bottom of the agent's screen. That's
  how every other agent is covered. A turn that ends normally with a reply that only
  *mentions* a limit ("the client now prints 'usage limit reached'"), anywhere in the
  reply, is not a stall: the `Stop` hook reports the reply, and limit text on screen
  that is part of it is ignored.
- Print-mode and SDK runs (`claude -p`, Agent SDK scripts, `codex exec`, Codex subagent
  threads) and agents started by another agent's tool are left alone. They inherit the
  pane of the session that started them, and nobody types into them.
- The desktop apps write the same transcript and rollout files, so Claude Desktop and
  Codex app sessions are seen too.

**Reset times** come from the exact time when the agent gives one (Codex), or from the
wording: "resets 10am (Australia/Sydney)", "try again at 5:53 PM", "in 2 hours 13
minutes", "in 30s", "resets Nov 14, 6pm", ISO times and weekdays. If no time can be
found, a limit is the account's: Continuum uses the reset another session of the same
account (same config folder) reported for the same limit, or, if none has, learns it
from the next one that does. Failing that, it retries with backoff. Transient errors (529 overloaded, 5xx, dropped
streams) get a short backoff.

**Before typing**, Continuum checks four things:

1. **Nobody has continued yet.** New activity in the transcript means you or the agent
   resumed it. So does Claude reporting the session busy since after the limit hit.
   The screen changing doesn't count when the transcript or a hook reported the
   stall: Claude Code clears its limit notice by itself when the limit resets.
2. **It's still the conversation that stalled.** If the agent's window has moved on to
   another conversation (`/clear`, `/resume`), Continuum leaves the old one alone.
3. **The pane still runs this session's agent, in front.** Every process in a pane
   carries the pane's id in its environment (`TMUX_PANE`, `REX_BLOCK`, …), and the
   server or session it belongs to (a pane id like `%3` exists on every tmux server).
   Continuum finds the agent's process that way and checks that it's the session's own process
   (or, if none was recorded, one that was already running when the session stalled),
   that it's in the foreground, and that it isn't suspended. If the agent quit and a
   shell, an editor or a new session now has the pane, nothing is typed. Where
   processes can't be checked (Windows, or macOS's own programs, whose environment is
   hidden), it falls back to the pane's foreground program and the agent's own screen
   markers. After a session ends, its pane is forgotten.
4. **No menu or approval prompt is open,** at the bottom of the screen, or (in the
   wrapper) anywhere in what the agent has shown since you last typed. Enter or a typed
   letter could pick an option.
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
  Continuum continues it under its own id with `claude --bg --resume <id>`,
  `codex queue --thread <id> --resume` or `grok --resume <id> --single <message>`. It's the same conversation, and nothing else
  has it open, so nothing is copied.
- **It's open somewhere Continuum can't type**, such as an agent started in a plain
  terminal before the shell hook was installed, or a session open in a desktop app.
  Continuum doesn't start a copy. It notifies you, and keeps retrying in case the session
  becomes reachable.

Continuum learns whether a session is open from the agent itself. It uses
`claude agents --json`, the running-session files Claude keeps, Grok Build's
`active_sessions.json`, the process recorded by the hook, and a process scan for the
other agents. On Windows, which doesn't say which folder a process runs in, any running
process of the agent counts as possibly holding the session. If none of these can answer,
Continuum assumes the session is open. Set `"allow_copies": true` to allow the CLI route
anyway.

## Desktop apps

**Claude Desktop.** Claude Desktop runs Claude Code itself (it bundles its own copy).
Desktop sessions write the same transcripts, and load the same user hooks and plugins
from `~/.claude`, so `continuum install` covers them with nothing extra. That was
checked on a real machine: Desktop transcripts record Stop hooks from user-installed
plugins.

Continuum can't type into an app window, so it handles a stalled Desktop session like
this:

- **While Desktop may still hold the session,** Continuum never starts a copy. It
  notifies you once ("send any message there to continue now, or leave it"), then
  checks again every couple of minutes.
- **Once Desktop lets go,** Continuum continues the session by itself under its own id.
  You'll see the continued conversation when you next open it.
- **Knowing when Desktop has let go.** The safe default treats a Desktop session as held
  while the app is running. Continuum also watches Claude's own session list. Once that
  list has shown a Desktop session, Continuum trusts it for that version of Desktop's
  bundled Claude Code, and continues as soon as the session is no longer listed, with no
  need to quit the app. When Desktop updates its bundled Claude Code, Continuum goes
  back to the safe default until it sees a Desktop session in the list again.

**Codex app.** `codex queue` sends the resume prompt to the local Codex app server,
which the Codex app itself uses. So the session continues inside the app, with no copy
and no need to close it.

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

- **Closed sessions from before installation are never revived.** Sessions that are
  open and stalled are picked up. After the daemon has been off, at most 12 hours
  (`catch_up_hours`) are caught up, except that a session that is still open and
  still waiting on a limit is resumed however long ago it stopped (up to 8 days, past
  a weekly limit). Anything the session did since (you typed in it, cleared it, or
  closed it) means it is left alone.
- **No unsafe typing.** It types only into the session's own agent process: never into
  menus, approval prompts, shells, editors or another session, and it never starts a
  copy of an open session.
- **Billing stops for you.** Billing, credit, spend-cap, sign-in and API-key errors are
  marked **needs you** and never retried, not even by `continuum resume-now`.
- **One stall, one resume,** even when the hook, transcript and screen all report it.
  It never types twice into the same pane for one stall.
- **Limits on retries.** It gives up after 8 attempts (`max_attempts`), and doesn't
  schedule resets further away than `max_wait_hours` (default 8 days).
- **Dry runs.** `"dry_run": true` logs what would happen without typing or running
  anything. Everything is recorded in `~/.continuum/history.jsonl`.
- **Hooks can't break an agent.** They take about 4 ms, print nothing, and always exit
  0, even on malformed input.

## What install sets up

Everything is under your user account. Nothing needs admin rights, and nothing is sent
over the network.

| What | Where | Why |
|---|---|---|
| The program | `~/.continuum/bin/continuum` | Run by everything below. |
| Command link | `~/.local/bin/continuum` (only if that folder is on your `PATH`) | So `continuum` works in any terminal. |
| Data folder | `~/.continuum/` | Settings (`config.json`, `scope.json`), scheduled resumes, `history.jsonl`, `daemon.log`, and a record of what was installed. |
| Claude Code hooks | `~/.claude/settings.json`: `SessionStart`, `StopFailure`, `Stop`, `SessionEnd` | Claude Code reports stalls and where each session runs. This also covers Claude Desktop. |
| Codex hooks | `~/.codex/hooks.json`: `SessionStart`, `Stop` | Codex reports where each session runs. |
| Grok Build hooks | `~/.grok/hooks/continuum.json`, a file of Continuum's own | Grok Build reports stalls and where each session runs. Grok runs hooks in folders you have trusted. |
| Muse Code hooks | `~/.config/muse/settings.json`: `SessionStart`, `StopFailure`, `Stop`, `SessionEnd` | Muse Code reports stalls and its sessions. |
| Cursor CLI hooks | `~/.cursor/hooks.json`: `sessionStart`, `afterAgentResponse`, `stop`, `sessionEnd` | The Cursor CLI reports its sessions and replies. |
| Agent skill | `~/.claude/skills/continuum`, `~/.codex/skills/continuum`, `~/.gemini/config/skills/continuum` (for the agents you have) | Lets agents check and control Continuum for their own session (see [below](#agents-controlling-continuum)). `install --no-skill` leaves it out. |
| Shell hook | A marked block in `~/.zshrc`, `~/.bashrc` or fish's `conf.d/continuum.fish` | Runs interactive `claude`, `codex` and other installed agents under the wrapper, so Continuum can type into them. |
| Login item (background mode only) | macOS: `~/Library/LaunchAgents/dev.continuum.daemon.plist`. Linux: `~/.config/systemd/user/continuum.service`. Windows: scheduled task "Continuum". | Starts the background daemon when you log in. |
| Backups | `<file>.continuum-backup-<date-time>` next to each file it changes | Your file as it was before Continuum touched it. |

Each hook is one line that runs the program by its full path, for example
`/bin/sh -c '"/Users/you/.continuum/bin/continuum" hook claude 2>/dev/null || true'`.
Your other settings and hooks are left alone. `continuum uninstall` removes
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
- **Hooks**, run by Claude Code, Codex, Grok Build, Muse Code and the Cursor CLI on session events. Each takes a few
  milliseconds, drops a small event file in `~/.continuum/spool/`, and starts the
  daemon if it isn't running.
- **The wrapper**, around each agent started from your shell: two small processes,
  `continuum run` and `continuum __pty-helper`, which pass everything through. They
  exit when the agent does.
- **Keep-awake**, only while a resume is less than 8 hours away or a resumed session is
  still working. On macOS it's a `caffeinate` process, on Linux `systemd-inhibit`, and
  on Windows a system flag. It's released as soon as nothing is pending.

Check on it with `continuum status`, `continuum list` and `continuum logs -f`.

## Agents controlling Continuum

Agents run Continuum through their shell tool. Wherever a command takes a session,
`this` means the agent's own session:

```sh
continuum this                    # this session: state, where it runs, its message
continuum exclude this            # don't auto-resume it (include this: undo)
continuum message set --session this "Re-run the failing tests, then continue."
continuum cancel this             # cancel its scheduled resume
continuum resume-now this         # resume it now
```

Continuum finds the session from the variable the agent sets for its commands
(Claude Code's `CLAUDE_CODE_SESSION_ID`, Grok Build's `GROK_SESSION_ID`,
Muse Code's `MUSE_SESSION_ID`, Antigravity's `ANTIGRAVITY_CONVERSATION_ID`), else from the agent's process, or the
wrapper or pane it runs in, matched against what its hooks reported. Run outside an
agent, `this` is an error.

`install` gives agents that read skills a short Continuum skill, so you can simply ask
("don't auto-resume this session", "when you resume, re-run the tests first"):
`~/.claude/skills/continuum` (Claude Code, and Grok Build, Muse Code, the Cursor CLI
and opencode, which read Claude's skills),
`~/.codex/skills/continuum` and `~/.gemini/config/skills/continuum` (Antigravity).
`install --no-skill` leaves them out. `continuum agent-guide` prints the same guide for
any other agent.

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
`~/.continuum/hooks.log`. `daemon.log` rotates at 5 MB and `hooks.log` at 2 MB.

## Staying compatible with agent updates

Nothing can promise that a future agent release won't change something. Continuum is
built so most changes are absorbed, and the rest are visible and quick to fix:

- **Stable interfaces first.** Documented hooks, documented CLI flags (`--resume`,
  `--bg`, `claude agents --json`, `codex queue`), and the session files the agents
  already keep. Screen reading is a fallback for those two, and the main source only
  for agents that offer nothing else.
- **Wording, flags and commands are data.** Every message pattern, CLI flag, menu
  pattern and multiplexer command lives in
  [`default_rules.json`](../internal/rules/default_rules.json). Override any of it in
  `~/.continuum/rules.json` without rebuilding (`continuum rules --defaults` prints a
  starting point). A broken override is reported, and the built-in rules keep working.
- **Capability probing.** Before using an agent command, Continuum checks the installed
  agent's `--help` for the flags it needs. If a release drops `--bg`, it falls back to
  the next command instead of failing.
- **Lenient parsing.** Unknown fields in hooks, transcripts and session lists are
  ignored, and missing ones are tolerated.
- **Updates are noticed and handled automatically.** When an agent updates (its
  executable changes), the daemon re-checks everything it relies on. That covers its
  hooks, its resume commands, and whether it still reports its open sessions. If an
  update removed Continuum's hooks, the daemon re-adds them, keeping every other
  setting, and tells you. If something can't be repaired, it switches to the safe
  behaviour (only typing into terminals, never starting a copy) and tells you once,
  in plain words.
- **Desktop app updates too.** What Continuum learns about a desktop app is tied to the
  app's version, and re-checked when the app updates.
- **Unknown means careful.** When an update removes a field Continuum reads (for example
  where a session was started), Continuum assumes the cautious answer rather than the
  convenient one.
- **Drift shows up.** `continuum doctor` re-checks real limit messages against the
  rules, checks which resume commands the installed agents still support, the hooks,
  the desktop apps, and the result of the last update check.

