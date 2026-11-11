# Changelog

All notable changes to Continuum. Versions follow [Semantic Versioning](https://semver.org).

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

[0.4.0]: ../../releases/tag/v0.4.0
[0.3.0]: ../../commit/ba6abd6
[0.2.0]: ../../commit/f559807
[0.1.0]: ../../commit/097e190
