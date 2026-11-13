# Changelog

All notable changes to Continuum. Versions follow [Semantic Versioning](https://semver.org).

## [0.10.0] - 2026-11-13

### Added
- **Agents can control Continuum.** Wherever a command takes a session, `this` means
  the agent session running the command: `continuum exclude this`,
  `continuum message set --session this "…"`, `continuum cancel this`,
  `continuum resume-now this`. `continuum this` shows the session's state, where it
  runs, whether it's covered and the message it would get (`--json` for scripts).
  Continuum finds the session from the variable the agent sets (Claude Code, Grok
  Build, Antigravity), else from the agent's process, wrapper or pane.
- `install` gives Claude Code, Codex and Antigravity a short Continuum skill (Grok
  Build reads Claude's), so you can just ask your agent, for example "don't
  auto-resume this session". `--no-skill` leaves it out; uninstall removes it.
  `continuum agent-guide` prints the same guide for any other agent.

### Changed
- `update` leaves a daemon you stopped with `continuum stop` stopped. The login item
  is refreshed without starting it; it starts at your next login, or with
  `continuum start`.
- The README is short and for users; install details, what runs in the background,
  troubleshooting and agent control moved to [How it works](docs/how-it-works.md).
- `continuum detect` lists agents that read a message the same way together, and
  says when wording means "needs you" (billing, credits, sign-in) even without a
  limit phrase.

### Fixed
Found by testing the whole app by hand, in a sandbox, with real Claude Code, Grok
Build and Antigravity CLI sessions:
- Installing the skill reported "no such file or directory" although it was written.
- The `~/.local/bin/continuum` link the install plan listed wasn't made when that
  folder (on your PATH) didn't exist yet.
- `doctor` listed Grok Build twice.
- `uninstall` said "remove its hooks from …" after removing them, and left an empty
  `skills` folder it had made.
- `continuum message` showed folders as full paths instead of `~/…`.
- Install, update and uninstall output now lines up.

## [0.9.1] - 2026-11-13

A second independent review found the issues fixed below.

### Fixed
- **Typing into the wrong pane.** A pane id is only unique within its server or
  session: tmux's `%3` exists on every tmux server, screen's window 0 and zellij's pane
  0 in every session. The check before typing now also compares the server or session,
  so an agent in the same-numbered pane elsewhere no longer passes for the session's
  own. And a pane taken from Claude Code's session list keeps the tmux server the hooks
  recorded, instead of the default server's pane of the same number.
- A stall seen by both the tmux pane scanner and an agent's hook (Grok Build, and
  others in tmux) could be resumed twice: the scanner recorded the pane without its
  server, so the two never matched. For the same reason a reply that only mentioned a
  limit, or a session you had continued, didn't cancel a resume the scanner scheduled.
- The pane check's "started after the stall" test never worked in locales that write
  dates differently ("Fri 13 Nov" in Australia, German, French, …): `ps` is now asked
  in the C locale.
- A reply that mentioned a limit above its last 8 lines was still taken for a stall
  when the wrapper read it on screen (it reads the last 40). The whole reply counts
  now. A finished turn whose reply mentions limits also counts as activity again, so a
  resume you no longer need is dropped.
- Menus recognisable only across lines ("❯ Yes" above "No", or the cursor on a later
  option) were never detected: the rule for them could not match, as lines were
  checked one at a time. It now matches, and needs a selection marker so a reply that
  answers questions with "Yes" and "No" isn't taken for a menu.
- `continuum disable --keep-running` stopped the daemon anyway when the login item had
  started it (removing the login item stops its job). It is started again on its own.
- On Linux, `continuum stop` lasted past the next login: the start by the systemd user
  unit wasn't recognised as a login.
- On Windows the process tree was empty, so an agent started by another agent wasn't
  recognised, and a session that only inherited Grok's or Cursor's variables was
  reported as theirs.
- Installed from a shell that sets `CLAUDE_CONFIG_DIR`, `CODEX_HOME` or `GROK_HOME`,
  the daemon (started at login without those variables) watched the default folders,
  ran the agents' commands there, and could put hooks into a settings file you don't
  use. `install` and `update` now note the folders in `config.json` (`grok_home` is
  new), and the daemon passes them to the agents' commands.
- `continuum message reset` on a `messages.json` with a typo saved an empty file over
  all your messages. It now leaves the file alone and says so.
- `continuum message set "TEXT" --agent codex` set the default message to "TEXT --agent
  codex". Options after the message are refused.
- `continuum include 1` (or `unchoose`) removed every entry whose session id starts
  with what was typed, and `message reset --session 1` a message of such a session.
  An id prefix needs 4 characters, as elsewhere.
- Dropping a finished `continuum run --choose` from the scope could undo a `continuum
  choose` made at the same moment.
- A daemon starting at the instant `status` or a hook checked for one could exit as if
  another one ran.
- `continuum status` said a stopped daemon starts again at the next login even when
  there is no login item.
- With auto-start off, `update`'s list of changes said it would start the daemon, which
  it doesn't (it only restarts one that is running), and left out removing a login
  item.
- `install -h` didn't mention Grok Build's hook file.

## [0.9.0] - 2026-11-13

### Added
- `continuum disable` turns auto-start off: the login item is removed, hooks no longer
  start the daemon, and the running daemon stops (`--keep-running` leaves it running).
  `update` keeps the choice. `continuum enable` turns auto-start back on (and from
  standalone mode, switches to background mode).
- The interactive menu has "Start, stop or turn off auto-start", showing whether the
  daemon runs and whether it starts by itself.

### Changed
- `continuum status` shows `auto-start` (on at login and when an agent needs it, on
  when an agent needs it, or off) instead of `at login`, and says when you stopped the
  daemon.

## [0.8.0] - 2026-11-13

An independent review of the whole project found the issues fixed below.

### Added
- **Grok Build**: Continuum adds its own hook file (`~/.grok/hooks/continuum.json`),
  reads Grok's list of open sessions, knows Grok's limit and error messages, and
  continues a closed Grok session with `grok --resume`.
- **Antigravity CLI** (`agy`): recognised, wrapped by the shell hook, and its quota
  and capacity messages understood.
- **Cursor CLI**: more of its limit wording, and its billing messages are never retried.
- `continuum status` shows where each session really runs: `Rex (via Continuum)`,
  `iTerm2`, `Ghostty`, … instead of "terminal". Agents with no session list of their own
  (Antigravity, Cursor, Gemini, …) are listed while they run in Continuum's wrapper.
- `scan_tmux_socket` setting: which tmux server the pane scanner watches.

### Fixed
- **Typing into the wrong place.** Before typing into a pane, Continuum now finds the
  agent's own process in it (from the pane id every process there inherits) and checks
  that it's the session's process, in the foreground, and not suspended. If the agent
  quit and a shell, an editor or a new session has the pane, nothing is typed. A
  session's pane is forgotten when the session ends.
- A turn that ended normally with a reply that only *mentioned* a limit or an API error
  could be taken for a stall, and "continue" typed into the session later.
- Grok Build runs Claude Code's hooks, so Grok sessions were recorded as Claude Code
  sessions. Cursor, which can do the same, is recognised too.
- Print-mode and SDK runs (`claude -p`, Agent SDK scripts, `codex exec`, Codex subagent
  threads) and agents started by another agent's tool inherited their parent's pane.
  They're now left alone.
- `resume-now all` also resumed finished jobs: billing stops, cancelled resumes, and
  sessions that had continued by themselves. `all` now means pending resumes only, a
  billing or sign-in stop is never resumed, and a selector must name one session (an
  empty one, or a prefix shorter than 4 characters, matches nothing).
- `continuum stop` didn't hold: the next hook started the daemon again. It now stays
  stopped until `continuum start` or the next login. In standalone mode, `resume-now`
  and `cancel` no longer start a background daemon.
- Running `/usage` or another local command after a limit made Continuum think the
  session had continued, so it was never resumed.
- A weekday reset ("resets Mon 6pm") seen later the same day resolved to the past.
- Two Codex sessions in the same folder could be matched to each other's wrapper.
- The wrapper's menu check could miss a menu that later redraws pushed off the bottom
  of the screen. It now also checks everything shown since you last typed.
- Install, update and uninstall replaced a symlinked `settings.json` or `.zshrc`
  (dotfiles managers) with a regular file. They now write through the link, and keep
  the file's permissions.
- Uninstall gives a shell startup file back byte for byte, even without a final
  newline, and leaves alone a file whose Continuum block is damaged instead of deleting
  lines past it.
- `--purge` deletes only Continuum's own files, and keeps your settings backups if any
  file couldn't be restored.
- Choosing or excluding a folder through a symlink (or `/tmp` on macOS) never matched.
- On Windows: a session Claude Desktop or the Codex app may hold could be copied, and
  `update` failed while the daemon ran.
- Windows notifications could break on a curly quote in the message.
- Smaller fixes: messages from `config.json` or a hand-edited `messages.json` are typed
  as one line too; a message starting with `-` isn't read as an option; Ctrl+C works
  in the interactive menu after "Watch live"; a session waiting on a desktop app no
  longer keeps the computer awake; `diagnose` reports leave custom messages out even at
  debug level; `status` no longer reports another Continuum folder's login item; the
  systemd unit survives paths with spaces or `%`; screen captures stay private; the
  history and leftover wrapper folders no longer grow without bound; a settings backup
  isn't saved again when nothing changed.

### Changed
- Tests no longer run the real `claude` and `codex`. The integration tests use stand-in
  agent programs and their own tmux server, and include the case of an agent that quit.
- `docs/cli.md` no longer ends with three empty sections.

## [0.7.1] - 2026-11-12

### Fixed
- Sessions started before Continuum was installed showed "terminal" as where they
  run, even when they were running in Rex, tmux or another multiplexer. Continuum now
  reads the pane from the agent process's own environment (`REX_BLOCK`, `TMUX_PANE`,
  and so on).
- The same lookup lets Continuum type into those older sessions in place, instead of
  notifying you that it can't reach them.

## [0.7.0] - 2026-11-12

### Added
- `continuum status` lists every open session (and those active in the last 12
  hours), not only stalled ones. For each it shows its state (working, idle, waiting
  for you, usage limit with the resume time, API error, resumed, needs you, not
  resuming, closed), where it runs (tmux, Rex, a terminal, Claude Desktop,
  background), and whether Continuum covers it. `--all` adds closed sessions, and
  `--json` gives the same for scripts.
- `continuum sessions` shows the same table, plus which message each session would
  get. `--json` is supported.

### Changed
- Where a session runs is filled in from the hooks, even when the agent's own
  session list doesn't say.
- The interactive menu's status uses the new view.

## [0.6.0] - 2026-11-12

### Added
- Custom resume messages with `continuum message`: a default, per agent, per folder,
  and per session. The most specific one is used. Each can have a separate message
  for temporary API errors (`--outage`). `{agent}` and `{folder}` are replaced with
  the agent's name and the session's folder. `continuum message preview` shows the
  exact text a session would get.
- Set a session's message as you start it: `CONTINUUM_MESSAGE="…" claude`, or
  `continuum run --message "…"`.
- Interactive mode: `continuum` with no arguments in a terminal (or
  `continuum interactive`) opens a menu for status, sessions (choose, exclude, set a
  message, resume now, cancel), messages, scope, grace, watching live, doctor, logs,
  and install, update or uninstall.

### Changed
- Messages are always typed as one line: line breaks become spaces, so a message is
  never sent half-written.
- Session ids are shortened to 8 characters in listings.

## [0.5.0] - 2026-11-12

### Added
- Standalone mode. `continuum install` asks whether to run in the background (started
  at login) or standalone, where nothing runs in the background. `--background` and
  `--standalone` choose without asking.
- `continuum watch` runs Continuum in the terminal with readable live output. Ctrl+C
  stops it. `--takeover` replaces a running background daemon.
- `continuum logs [-f] [-n LINES] [--hooks]` shows or follows the daemon's log.
- `continuum diagnose [-o FILE]` writes a report for troubleshooting, with personal
  details removed.
- Log levels (`error`, `warn`, `info`, `debug`). Turn on debug with the `"log_level"`
  setting, `--debug`, or `CONTINUUM_DEBUG=1`. At debug level every event, check and
  delivery route is logged, and hooks log to `hooks.log`. Logs rotate at 5 MB.

### Changed
- Log messages are easier to read, for example
  "Claude Code · api · 5e551011 · usage limit · resumes 10:55 (in 2h)".
- `status` and `doctor` show the mode, and whether debug logging is on.

## [0.4.0] - 2026-11-12

First public release.

### Added
- `install`, `update` and `uninstall` list every file and item they will create,
  change or remove, with its location and purpose, and ask before doing anything.
  `--dry-run` shows the list only, and `--yes` skips the question.

### Changed
- `make build` now builds into `dist/` and points `./continuum` at the build.
- The README is shorter and written for users. The details moved to `docs/`.

## [0.3.0] - 2026-11-12

### Added
- `continuum update` applies a new version, keeping the choices you made at install.
- `continuum uninstall --purge` also removes Continuum's folder and the backups of
  your settings files.
- Uninstall checks that nothing is left behind, and puts settings files back exactly
  as they were.
- Sessions that are already open and waiting on a limit when Continuum starts are
  resumed.
- Ghostty support: agents in a plain Ghostty tab are typed into without focusing the
  window.

### Changed
- Hooks do nothing, and cause no errors, if Continuum is deleted without uninstalling.
- The daemon keeps Continuum's hooks up to date by itself.

### Fixed
- Two installs within the same second could overwrite the backup of the original
  settings file.
- A hook of yours whose path contained "continuum" could be mistaken for one of
  Continuum's.

## [0.2.0] - 2026-11-12

### Added
- Claude Desktop and Codex app support. Desktop sessions are never copied while the
  app may still have them open.
- Continuum notices when an agent updates, re-checks everything it relies on, repairs
  its hooks, and tells you if something changed.
- `install` ends with a summary of what's covered. `doctor` reports desktop apps and
  agent updates.

### Changed
- Waiting for a session that's open somewhere Continuum can't type into no longer
  counts as a failed attempt.

## [0.1.0] - 2026-11-12

### Added
- Resumes Claude Code, Codex and about 25 other agents after usage limits and API
  outages, in the same session, without needing window focus.
- Delivery through its own terminal wrapper (applied by a shell hook in any terminal),
  multiplexer panes (tmux, screen, zellij, herdr, Rex, WezTerm, kitty), or the agent's
  own command for closed sessions. It never starts a copy of an open session.
- Keeps the computer awake while a resume is near, so it works with the screen
  locked.
- Resumes all sessions, or only the ones you choose (`scope`, `choose`, `exclude`).
- `grace`: how long to wait for an agent's own auto-resume.
- Safety: never types into menus or approval prompts, and never retries billing or
  sign-in errors.
- Help for every command (`-h`), `doctor`, and docs in `docs/`.

[0.10.0]: ../../releases/tag/v0.10.0
[0.9.1]: ../../releases/tag/v0.9.1
[0.9.0]: ../../releases/tag/v0.9.0
[0.8.0]: ../../releases/tag/v0.8.0
[0.7.1]: ../../releases/tag/v0.7.1
[0.7.0]: ../../releases/tag/v0.7.0
[0.6.0]: ../../releases/tag/v0.6.0
[0.5.0]: ../../releases/tag/v0.5.0
[0.4.0]: ../../releases/tag/v0.4.0
[0.3.0]: ../../commit/ba6abd6
[0.2.0]: ../../commit/f559807
[0.1.0]: ../../commit/097e190
