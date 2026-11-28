# Changelog

All notable changes to Continuum. Versions follow [Semantic Versioning](https://semver.org).

## [Unreleased]

## [0.17.2] - 2026-11-28

### Added

- **Existing chat input handling (`draft_handling` / `continuum draft`).**
  Prevents Continuum from inserting its resume prompt into the middle of
  unsubmitted text in a wrapped agent's input box. Run `continuum draft restore`
  (default) to clear the draft, resume, and restore it afterwards, or
  `continuum draft submit` to press Enter on the existing text first and then
  send the resume prompt.

### Fixed

- **Closed non-headless sessions no longer retry endlessly after reset.** When
  a wrapper exits and clears a job's dead route under `track_closed_sessions`,
  agents without headless resume commands (such as Antigravity CLI) now fail
  cleanly with `ErrGone` at due time instead of retrying with backoff.
- **Antigravity CLI prompt transcripts no longer cancel pending resumes.**
  `USER_INPUT` lines in Antigravity `transcript.jsonl` now emit `TurnStart`
  activity (matching `PreInvocation` hooks) and preserve preceding completed
  `PLANNER_RESPONSE` answers, so sending a prompt that immediately hits quota
  no longer marks the session as continued.
- **Stale wrapper redraws no longer corrupt or duplicate pending resumes.**
  Wrapper deduplication now lasts for the full turn (resetting on Enter,
  daemon resume, or stall clearance) and ignores leading spinner lines above
  the matched limit line; daemon twin matching and merging now ensure
  authoritative hook/transcript reset times and limit timestamps always beat
  screen scrapes without clobbering retry backoff.

## [0.17.1] - 2026-11-28

### Fixed

- **Windows PowerShell profile directory resolution.** Scoped
  `FOLDERID_Documents` lookups to the active user home directory so overridden
  `USERPROFILE` environments cleanly resolve `<USERPROFILE>\Documents` while
  still supporting OneDrive Known Folder Move under the user's home folder, and
  updated shell round-trip tests for Windows PowerShell profiles.

## [0.17.0] - 2026-11-28

### Added

- **Cross-platform stale process cleanup (`continuum cleanup`).** On-demand
  scanner and terminator for macOS, Linux, and Windows:
  - `continuum cleanup scan` saves confirmed leftover findings to
    `~/.continuum/cleanup-scan.json` for review; `continuum cleanup terminate SCAN_ID PID...`
    (or `--all SCAN_ID`) revalidates boot session, start time, ownership,
    process-group membership, and live evidence before sending `SIGTERM` (Unix)
    or handle-verified termination (Windows).
  - `continuum cleanup scan --auto` scans and stops confirmed findings in one
    run without overwriting a saved review scan.
  - Excludes the running Continuum daemon, current shell ancestry, launchd
    services (macOS), systemd service units (Linux), and other users' processes.
- **Closed-session tracking option (`track_closed_sessions` / `continuum closed`).**
  Run `continuum closed off` to track and resume only open sessions: pending
  resumes drop as soon as their session closes, closed transcripts are not
  scheduled, and `status`, `list`, and `sessions` omit closed sessions unless
  `--all` is passed.
- **Windows feature parity with macOS and Linux.**
  - Native Windows process inspection in `internal/proc`: `StartTime`
    (`GetProcessTimes`), `Args`, `Cwd`, `Environ` (`NtQueryInformationProcess` +
    PEB `RTL_USER_PROCESS_PARAMETERS` / `ProcessCommandLineInformation`), and
    full command lines in `List()`.
  - Windows ConPTY attached-terminal delivery (`internal/deliver/pty_windows.go`)
    so interactive `use_pty` resume commands (such as `claude-attach`) work on
    Windows.
  - PowerShell shell hook integration (`continuum shell-init pwsh|powershell`,
    installed into PowerShell 7 / Windows PowerShell profiles on Windows).
- **Codex hooks trust check in `continuum doctor`.** Flags `~/.codex/config.toml`
  `[hooks.state]` entries that lack `enabled = true` so users know why Codex is
  relying on rollout tailing.

### Changed

- **First-class wrapper instance model (`instances.json`) and daemon snapshot (`state.json`).**
  Separates running terminal/wrapper instances (`store.Instance`) from
  conversations (`store.Session`), emits explicit `wrapper_start` / `wrapper_exit`
  lifecycle events, recovers cleanly from crashed wrappers, and publishes one
  canonical live `Snapshot` from the daemon for CLI commands (`status`,
  `sessions`, `choose`) to eliminate daemon/CLI split-brain.
- **Per-agent drivers, explicit `JobPhase` state machine, and modular daemon.**
  Isolated agent-specific logic into dedicated drivers across `daemon`, `hooks`,
  and `deliver`, replaced overlapping job status booleans with `JobPhase` and
  atomic transitions (`internal/daemon/jobstate.go`), and split the monolithic
  `internal/daemon/daemon.go` into focused modules (`daemon.go`, `ingest.go`,
  `schedule.go`, `delivery.go`, `maintenance.go`, `snapshot.go`).

### Fixed

- **Codex open sessions no longer duplicated or falsely marked `closed`.**
  Unambiguous live Codex wrappers now bind to their rollout-discovered
  conversation (by argv resume, tracked `Instance.ActiveSessionID`, or unique
  directory wrapper), and ambiguous non-wrapped process scans leave `Checked`
  unset instead of reporting an active session as `closed`.
- **Antigravity CLI subagent conversations ignored.** Filters internal subagent
  transcripts (step 0 `SYSTEM`/`SYSTEM_MESSAGE` with `sender=`) across tailing,
  hooks, startup job reconciliation, and status views, and stops keep-awake from
  holding sleep open for closed non-headless jobs.

## [0.16.0] - 2026-11-27

### Added

- **Antigravity CLI (`agy`) 3-layer detection.** Continuum now combines three
  detection sources for Antigravity CLI so they back each other up and merge
  into a single scheduled resume:
  - **Lifecycle hooks** in `~/.gemini/config/hooks.json` under the `"continuum"`
    named hook (`SessionStart`, `PreInvocation`, `Stop`), returning the JSON
    `{}` response Antigravity requires and ignoring Antigravity 2.0 desktop app
    and IDE hooks that share the same file.
  - **Transcript tailing** of
    `~/.gemini/antigravity-cli/brain/<id>/.system_generated/logs/transcript.jsonl`
    as a structured backup (filtering out `GENERIC` tool outputs and recording
    assistant replies mentioning limits as `reply` events).
  - **Wrapper PTY and multiplexer screen detection** as fallback, merging with
    hook/transcript events to upgrade reset times and fuller error messages.

### Fixed

- **Third-party and general quota-reached wording.** Generalized Antigravity's
  `individual quota reached` pattern and the common quota pattern to match
  `Third-party quota reached` and other `quota reached` messages (e.g.,
  `⚠ Third-party quota reached. ... Resets in 2h30m50s.`).
- **Model-safe early-lift for third-party quotas.** Answering with a Gemini
  model in another Antigravity session no longer falsely lifts a third-party
  quota limit early.
- **Process-scan safety and session status accuracy.** Generalized the
  ambiguous process-scan guard beyond Codex so closed sessions are never routed
  into a different live process in the same folder, ensured live wrapped
  sessions idle past 12 hours still appear in `continuum status`, and stripped
  `ANTIGRAVITY_*` variables in `CleanEnv` so non-CLI env vars cannot leak into
  the daemon or resumed children.

## [0.15.11] - 2026-11-26

### Fixed

- On Windows (and other systems without sysctl or /proc), a hook reads the
  process table once instead of once per ancestor.
- Leftover payload files from 0.15.9's hook hand-off are removed.
- The hook tests cover every agent's SessionStart, and that the ancestry
  follows the whole chain of parents.
- Docs: hooks run no `ps`; the only program one may start is the daemon,
  detached, when it isn't running.

## [0.15.10] - 2026-11-26

### Fixed

- Hooks start no program at all: the hook's ancestors, and an agent's name,
  arguments and environment, are read from the kernel (macOS sysctl, Linux
  /proc) instead of `ps`. Every hook returns in milliseconds even on a busy
  or monitored computer, and does its work before the agent continues again.
  This replaces 0.15.9's hand-off to a detached process: that one checked the
  process tree only after the hook had returned, by when the agent could have
  quit (its event dropped or misjudged), and its events could arrive after
  the agent's next ones (a resume wrongly reported as failed, a Cursor limit
  taken for the old one).

## [0.15.9] - 2026-11-26

### Fixed

- Continuum's per-turn hooks (Claude Code's prompt-submit and Stop, and the
  same events of Grok Build, Muse Code and the Cursor CLI) no longer hold up
  the agent. On a busy or monitored computer reading the process tree could
  take over 10 seconds, so 0.15.8's prompt-submit hook timed out before every
  prompt. They now hand their work to a detached process and return at once;
  the event keeps the time the hook ran.

- Claude Code's "You've hit your 5-hour limit" (and other limits named with a
  hyphen or dot, like "Opus 4.5") is now recognised when read off the screen:
  the pattern only allowed letters, digits and spaces before "limit". Hooks
  and transcripts were unaffected (they use the error code).
- A limit read off the screen of an out-of-date copy's pane (no session id) is
  no longer typed into it: when it comes due, Continuum looks up which
  conversation the process in that pane holds and resumes it as that
  conversation, with the same checks (scope, print-mode runs, the copy check).
  It drops it when that conversation has its own resume (or you cancelled it
  for this stall), is open in another process where it continued, or started
  after the limit. A pane that is back on a limit right after such a resume is
  resumed again as a further attempt of the same resume (up to the attempt
  limit; with backoff when it shows the reset that was just used).
- A resumed turn that fails with an error that is neither a limit nor an
  outage (the API refused the request) is reported: a notification, and
  `continuum status` shows "resumed, but its turn failed" until a later turn
  ends normally (for up to a day). Only the resumed turn counts (within 15 minutes, before it
  ends otherwise, counting from when typing it began); resuming again starts
  afresh. Such errors used to be dropped.
- A limit seen only on a pane's screen is no longer merged into the pending
  resume of a conversation in the same folder that has ended: that resume
  could be typed into the other pane, and the ended conversation never
  continued.
- A limit of a resumed turn reported by the agent itself (hook, transcript,
  rollout) while a slow delivery is still returning is no longer dropped as an
  old line.
- A screen-only resume whose terminal closed, for a stall its conversation's
  own resume covers, is dropped without a "won't resume" notification.
- A StopFailure that only gives a billing or sign-in error code (such as
  `authentication_failed`) now needs you, as the rules intended.

### Changed

- The out-of-date copy checks also cover Grok Build and Muse Code, once
  Continuum has seen on this computer that the process their hooks report is
  the one their own session list names (an entry of the list itself, not a
  guess; two different processes must agree; learned again when the agent's
  executable changes; checked with a backoff). Their hooks now report the process on every event, from the
  process tree they already read.
- The prompt-submit hooks (Claude Code, Grok Build, Muse Code) show no status
  line: they run on every prompt.
- `continuum status --json`: another process holding a conversation has state
  `copy` (`stale_copy` is only an out-of-date copy); the state codes are
  documented in `continuum status -h`.
- The end-to-end test covers an out-of-date copy, both when a hook reports its
  limit and when the limit shows only on its screen.

## [0.15.8] - 2026-11-26

### Changed

- Claude Code also gets Continuum's prompt-submit hook (`UserPromptSubmit`; it
  prints nothing, so adds nothing to the conversation). Typing in one copy of a
  conversation open in two processes makes that copy the one Continuum
  resumes straight away, instead of when its turn ends. The daemon adds the
  hook to existing installs by itself.

### Fixed

- A copy whose agent reports it working since before the other process took
  over is a stuck turn, not the copy in use: it is no longer typed into.

## [0.15.7] - 2026-11-26

### Fixed

- Waiting for an out-of-date copy (or for a session open somewhere Continuum
  can't type) gives up 24 hours after the wait began, not 24 hours after the
  limit: a weekly limit no longer gives up at once. Giving up now notifies you.
- Each reason to wait is notified when it starts, and a wait that ended no
  longer shows as one or stops the computer staying awake for retries.
- An out-of-date copy is also recognized when the process that continued the
  conversation still runs but has moved on to another conversation.
- Whether a quitting process was only a copy is decided from Claude Code's own
  list of open sessions, not the process name (which can be reused, or show as
  `node`).
- A hook event delivered after its session ended no longer reopens it or moves
  it back to its old pane.
- `continuum status` lists every session with a resume pending, including ones
  closed normally or seen more than 3 days ago; doesn't show a closed
  process's pane for a copy; and only promises to continue a closed
  conversation when it can.
- A resume dropped because its terminal closed now notifies you.

## [0.15.6] - 2026-11-26

### Fixed

- A Claude Code process is only treated as an out-of-date copy when it hasn't
  done anything since the process the hooks last heard from: one that is
  working, or whose status changed since, is the copy in use and is resumed.
  This also covers records that 0.15.4 left pointing at a copy that had quit.

## [0.15.5] - 2026-11-26

### Fixed

- Continuum no longer types a resume into an out-of-date copy of a Claude Code
  conversation: when the process that continued the conversation has closed
  and only an older copy is still open, it notifies you once and waits.
  Closing the copy lets it continue the conversation from the full transcript;
  typing in the copy carries on from there. `continuum status` shows such a
  copy as out of date.
- Closing one copy of a conversation open in two processes no longer marks
  the session closed, or drops the pane a pending resume would be typed into:
  the session's record stays with the process the hooks last heard from.
  Anything a session's process reports after it was marked closed marks it
  open again.

## [0.15.4] - 2026-11-26

### Fixed

- A conversation open in two processes (resumed again in another pane) is
  checked through the process Continuum's hooks last heard from, not whichever
  the agent lists first, and one process's status no longer overwrites the
  other's.
- `continuum status` lists such a conversation under the process Continuum
  resumes and shows the other as another copy (`"copy": true`, with its `pid`,
  in `--json`), instead of pinning the session to the stale pane and listing
  the real one as an unnamed wrapper. `continuum this` works in either copy.
- `continuum status` checks whether a session with a resume pending is still
  open, and marks a closed one as continuing in the background (or needing to
  be reopened, for agents without a resume command).
- A resume known only by a wrapper that has exited is dropped a few minutes
  later instead of staying listed until it is due.

## [0.15.3] - 2026-11-23

### Added

- `continuum recover all` checks every eligible Codex resume and retries each
  thread whose transcript shows no newer turn.

## [0.15.2] - 2026-11-23

### Added

- `continuum recover CODEX_THREAD_ID` can retry a detached Codex launch that an
  older Continuum marked delivered. It checks the exact thread's transcript
  for a newer turn before requeueing.

## [0.15.1] - 2026-11-23

### Fixed

- Continuum can resume a Codex thread already loaded by the local shared app
  server when it has no usable terminal route.
- The detached Codex fallback waits for a turn to start in the target thread
  before reporting success. An unconfirmed launch needs attention and does
  not send a second prompt automatically.

## [0.15.0] - 2026-11-22

### Added

- OpenCode Desktop and CLI can resume through an opt-in shared local server.
  Continuum checks the original session, its latest failure, and its queue
  before sending the continuation. See the [OpenCode setup guide](docs/agents.md#opencode-desktop-and-cli).

### Fixed

- If Codex queues a continuation and fails to reopen its thread, Continuum
  reports the queued message and prevents a second send.
- Terminal redraws no longer reschedule a stale usage limit. Changes to a
  reset time on a neighboring line still update the wait.
- Qwen Code's numeric UTC reset dates now retain their date and seconds.
- The shell hook recognizes the current `kilo` command and handles more
  one-shot, interactive, and administrative CLI invocations correctly.

## [0.14.2] - 2026-11-21

### Fixed
- A Codex conversation reopened with `codex resume <session-id>` appears once
  in status. Continuum sends the scheduled resume to its current wrapper
  when the old wrapper has closed.
- Codex hooks from the shared app server no longer record its inherited
  terminal as the conversation's location. Activity from a different
  conversation no longer cancels a pending resume with the same recorded
  terminal.

## [0.14.1] - 2026-11-21

### Changed
- **Continuum's hooks have a name.** Each hook entry now carries a `statusMessage`
  ("Continuum: tracks this session to resume it after usage limits") in Claude Code,
  Codex, Grok Build and Muse Code, so Codex no longer lists them as unnamed hooks. The
  daemon updates existing installs; Codex asks once to trust the changed hooks.
- **Grok Build, Muse Code and the Cursor CLI get a prompt-submit hook**
  (`UserPromptSubmit`, `beforeSubmitPrompt`). It says when a turn started, which banked
  reset detection now uses instead of assuming no answer takes over 10 minutes.
  A prompt submitted in a waiting session (`/usage`, `/model`) doesn't drop its resume.

### Fixed
- **A failed early resume waits for the reset it already knew.** When it hit the limit
  again with no reset time (a `rate_limit` StopFailure, a screen read), it retried on
  backoff until it gave up, instead of waiting for the original reset.
- **Banked resets for limits read off the screen.** A limit without a transcript (all
  of Cursor's) never matched a hook answer, which carries one, so it was never moved up.
- **A failed early resume only blocks its own limit.** It stopped answers from moving
  up every limit on the account, including other model families' and Codex limit ids.
  It now also keeps blocking until that limit's reset once the session is continued by
  hand or given up on, and only marks a resume failed when it hit the same limit.
- **Hook answers that don't show the limit lifted** no longer move resumes up: a turn
  that started before the limit (a long answer), one from Cursor's editor, and one from
  another model than the limited session's (Cursor's Auto).
- **Codex limits name the right limit.** The limit id comes from a rate-limit snapshot
  of the same turn that shows the limit full, not whatever snapshot was read last (a
  `/model` switch between them tagged the wrong limit), and so does its reset time. A
  turn that ends without an error no longer leaves a request marked in flight.
- **Cursor: an early resume that hits the limit again is seen.** Its limits only show
  on screen, and the notice after the resume was taken for the old one, so the session
  sat at the limit with nothing scheduled. A turn started there after the resume now
  makes it a new hit, which waits for the reset.
- **A session nothing can reach any more fails at once.** When its wrapper had exited
  (the terminal closed, or the computer restarted) and Continuum knew no session id or
  pane for it, it retried for hours, keeping the computer awake, until the attempts ran
  out. Seen with Antigravity CLI sessions after a restart.
- **A failed early resume is remembered until its reset,** not dropped with other
  finished resumes after 72 hours (a weekly reset can be further away).

## [0.14.0] - 2026-11-20

### Added
- **Banked resets for Codex, Grok Build, Muse Code and the Cursor CLI.** Spotting a
  limit that came back early now works beyond Claude Code:
  - **Codex** records the plan's limits with every answer. An answer to a request sent
    after another session's limit, with room left in every window of the same limit
    (`limit_id`), moves that session's resume up. Answers paid from credits leave the
    windows at 100% and don't count.
  - **Grok Build, Muse Code and the Cursor CLI**: a turn their Stop hook reports as
    ended normally, more than 10 minutes after another session's limit, does the same
    (`stop_hook_answers` in the rules).

  Checked live on Claude Code: a banked reset used during a weekly limit moved three
  waiting sessions from 114 hours away to 5 minutes, 10 seconds after the next answer.

### Changed
- **A wrong guess isn't repeated.** If a resume moved up because the limit looked
  lifted hits the limit again within 15 minutes, answers in the account's other
  sessions no longer move it up before its reset: they were paid some other way. A reset
  the agent reports used still does.

### Fixed
- **Codex sessions from different days are one account.** Codex keeps rollouts in
  dated folders, so a session started on another day wasn't matched to the same
  account, for example to learn the reset time another session had reported.

## [0.13.0] - 2026-11-20

### Added
- **Banked limit resets.** When you spend a limit reset (claude.ai Settings > Usage,
  the Desktop app, or Claude Code's `/limit-reset`), the account's limits come back
  before the reset Claude Code announced. Continuum used to wait for that original reset,
  which can be days away for a weekly limit. It now moves the account's waiting resumes
  up when Claude Code reports a reset used (`Reset used · 1 left`), or when another
  session of the same account gets an answer on the plan to a request sent after the
  limit hit. Answers paid from usage credits, Claude Code's own messages and grace-stop
  turns don't count, and a limit on one model family is lifted only by an answer from
  that family. A wrong guess costs one resume that hits the limit again and then waits
  for its reset. The old notice still on screen isn't taken for a new limit. The wording
  is data (`limit_reset_patterns` in the rules). If no other session is running,
  `continuum resume-now all` (now in the agent skill too) continues every waiting
  session.

## [0.12.1] - 2026-11-20

### Fixed
- **An agent wrapped under two commands is listed once.** `install` and `update`
  listed the Cursor CLI twice in their summary of what is covered (its `agent` and
  `cursor-agent` commands are both wrapped).

## [0.12.0] - 2026-11-20

### Added
- **Muse Code.** Meta's terminal agent (`muse`, running as `muse-bin-<version>`) is
  wrapped by the shell hook and recognised in tmux panes. `install` adds hooks to
  `~/.config/muse/settings.json` (`$XDG_CONFIG_HOME/muse`): Muse reports sessions,
  stalls (`StopFailure` with the error and its details) and replies the way Claude
  Code does. `muse sessions --json` tells Continuum which sessions are open. Muse
  runs hooks with a cleaned environment, so the hook reads the session's wrapper or
  pane from the Muse process itself. Muse reads Continuum's skill from
  `~/.claude/skills`. Continuum's checks run the launcher with
  `MUSE_NO_AUTO_UPDATE=1`, so they never start Muse's self-update.
- **Cursor CLI hooks and its `agent` command.** Cursor now installs its CLI as
  `agent`; the shell hook wraps that name too, but only while it is a link into
  Cursor's own install (`command_aliases` in the rules). `install` adds
  `~/.cursor/hooks.json` (`sessionStart`, `afterAgentResponse`, `stop`,
  `sessionEnd`), so Cursor sessions are tracked by their conversation id and folder,
  and a reply that only mentions a limit isn't taken for one. The Cursor editor and
  Grok Build read the same file: only runs by the Cursor CLI itself count, and the
  hooks are added only when the CLI is installed. The CLI runs under node, started as
  `agent`, and is recognised by its command line. `agent ls` (a picker
  that opens a session) is now wrapped; `models`, `worker`, `plugin` and others are
  passed through.
- **Limits on full-screen programs.** opencode 2 draws by moving the cursor and
  redraws its title after an error, so its limit message reached the wrapper only in
  pieces and was never detected. The wrapper now keeps a rendered copy of the screen
  for agents marked `screen_detection` in the rules (opencode), and a limit message
  shown since the last prompt or resume counts, even the same wording again. A menu
  or approval prompt drawn the same way is seen on the rendered screen too, so
  nothing is typed into it.

### Fixed
- **Grok Build stalls were reported twice.** Grok runs Claude Code's hooks as well as
  its own; the check for Continuum's own Grok hook searched the file as text and
  missed the escaped quotes in it, so the borrowed run reported every event again.
  The file is now parsed (the same check covers Cursor).
- **Print-mode runs of other agents are ignored.** Hooks from `grok -p`, `muse exec`,
  `agent -p` and other runs nobody types into no longer create sessions: the agent's
  command line is judged by the same rules the shell hook uses.
- **The test suite passes on Windows.** Tests there shared the real home folder
  (Windows takes it from `USERPROFILE`), wrote Windows paths unescaped into JSON, and
  expected Unix paths, file modes and links.

## [0.11.0] - 2026-11-19

### Fixed
- **Claude Code's grace stop is resumed.** On reaching the usage limit, Claude Code
  can now let the turn finish on a short allowance instead of failing it, so no
  limit error or `StopFailure` hook is written: only a note in the transcript
  (`usageLimitNote: "wrap_up"`), and a normal turn end. Continuum took the turn's own
  work for the session moving on and never resumed it. The end of such a turn is now
  a usage-limit stall. Its reset time, which the note doesn't give, is taken from
  another session of the same account that hit the same limit (or filled in when
  one does), with the unknown-reset backoff as the fallback. The wrapper seeing the
  screen change no longer cancels a resume that the transcript or a hook reported:
  Claude Code clears its "Usage limit reached" notice by itself at the reset, which
  dropped the resume minutes before it was due.
- **Sessions that hit a limit while the daemon was stopped are resumed when it
  starts.** A limit older than `catch_up_hours` (12h) was silently dropped, even when
  the session was still open and waiting on it and its reset hadn't come yet, so a
  weekly limit hit during a long stop was never resumed. Now an old limit is kept
  when its session is still open and hasn't moved on (later activity still cancels
  it), and the first scan after a start looks back 8 days for open sessions whose
  transcript ends on a limit (agents without hooks). A dropped old limit is logged
  at debug level.

### Added
- **Letter shortcuts in the interactive menu.** Every menu item can be picked by a
  letter from its label, shown in brackets (`[S]tatus`, `S[c]ope`, `Resume [m]essages`),
  as well as by its number. Letters are case-insensitive and unique within each menu.

## [0.10.2] - 2026-11-14

### Fixed
- **Compact reset durations (`Resets in 3h32m34s.`).** Antigravity CLI and Go-style
  compact durations without spaces between units now parse accurately instead of
  falling back to the 15m/30m unknown-reset backoff.
- **False-positive limit detection on user prompts and status lines.** `Classify`
  ignores interactive user input lines (`> ...`, `❯ ...`, `› ...`) and Continuum's
  own `⏸`/`▶` status log lines; the PTY wrapper clears its screen ring when Enter
  is pressed, checks the bottom 12 lines, and emits activity when a stalled session
  settles with non-limit output.
- **Open and active sessions in `status` and `list`.** `continuum status` and
  `continuum list` now show only currently open sessions and active resumes by
  default (`--all` includes closed sessions and finished resumes). Dormant
  background sessions from `claude agents --json` no longer pretend to be active
  now, wrapped sessions in Rex/multiplexers are no longer listed twice or repeated
  under `Other resumes:`, wrapper rows show their short wrapper ID in the `SESSION`
  column, and recycled OS PIDs no longer mark closed sessions as open.
- **Diagnostic logging and code review hardening.** Added `INFO`/`DEBUG` schedule
  and reconciliation logs, atomic lockfile creation across `scope`/`messages`,
  Windows PTY and process tree fixes, and safe binary replacement during `update`.

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

[0.16.0]: ../../releases/tag/v0.16.0
[0.15.0]: ../../releases/tag/v0.15.0
[0.14.2]: ../../releases/tag/v0.14.2
[0.11.0]: ../../releases/tag/v0.11.0
[0.10.2]: ../../releases/tag/v0.10.2
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
