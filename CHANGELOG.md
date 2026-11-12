# Changelog

All notable changes to Continuum. Versions follow [Semantic Versioning](https://semver.org).

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
  "Claude Code · api · 9a010a4e · usage limit · resumes 10:55 (in 2h)".
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

[0.7.1]: ../../releases/tag/v0.7.1
[0.7.0]: ../../releases/tag/v0.7.0
[0.6.0]: ../../releases/tag/v0.6.0
[0.5.0]: ../../releases/tag/v0.5.0
[0.4.0]: ../../releases/tag/v0.4.0
[0.3.0]: ../../commit/ba6abd6
[0.2.0]: ../../commit/f559807
[0.1.0]: ../../commit/097e190
