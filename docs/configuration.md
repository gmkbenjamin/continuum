# Configuration

`~/.continuum/config.json`. Every key is optional.

| Key | Default | Meaning |
|---|---|---|
| `enabled` | `true` | Turn Continuum off without uninstalling. |
| `dry_run` | `false` | Log what would happen; type and run nothing. |
| `harnesses` | all on | Turn agents on or off: `{"aider": false}`. |
| `resume_prompts` | built in | Prompt per agent: `{"claude": "Limit reset. Carry on."}`. Use `"<agent>_transient"` for API-error resumes. Easier: `continuum message`, which also sets messages per session, per folder and as a default (kept in `messages.json`). |
| `delay_after_reset_seconds` | `60` | Margin after the reset time. |
| `native_grace_seconds` | Claude 300, Codex 30 | Wait for the agent's own resume first. Easier to set with `continuum grace`. |
| `keep_awake_hours` | `8` | Hold off idle sleep while a resume is due this soon; `0` turns it off. |
| `allow_copies` | `false` | Let the agent CLI continue a session that's still open elsewhere (starts a copy). |
| `allow_headless` | `true` | `false`: only type into wrappers and panes, never run agent commands. |
| `strategies` | `["wrapper","multiplexer","terminal-app","native"]` | Delivery routes, in order. |
| `shell_hook_agents` | installed agents | Commands the shell hook wraps (replaces the default list). |
| `max_wait_hours` | `192` | Don't schedule resets further away than this. |
| `catch_up_hours` | `12` | How far back to catch up after the daemon was off. |
| `notify` | `true` | Desktop notifications. |
| `watch_transcripts` | `true` | Read Claude transcripts and Codex rollouts. |
| `scan_panes` | `true` | Scan tmux panes for other agents. |
| `scan_tmux_socket` | your default tmux server | The tmux server (socket path) the pane scanner watches. |
| `poll_seconds` | `5` | How often the daemon checks. |
| `binaries` | from `PATH` | Agent executables: `{"claude": "/opt/claude"}`. |
| `claude_home`, `codex_home` | `~/.claude`, `~/.codex` | Where the agents keep their files. Grok Build's is `$GROK_HOME`, else `~/.grok`. |
| `reset_hints_command` | none | A command printing `{"claude": "<RFC3339>"}`, used when a message gives no reset time. |
| `autostart_daemon` | `true` | Start the daemon from hooks and `run` if it isn't running. `false` in standalone mode. |
| `log_level` | `info` | `debug` also logs why: every event, check and delivery route, and hooks write to `hooks.log`. Also: `error`, `warn`. `CONTINUUM_DEBUG=1` turns on debug for one run. |

The scope is in `~/.continuum/scope.json` (use `continuum scope` and `choose`). Rules
overrides go in `~/.continuum/rules.json` (see [docs/agents.md](agents.md)).

## Files

Everything is in `~/.continuum`; set `CONTINUUM_HOME` to move it.

| File | Contents |
|---|---|
| `config.json` | Your settings |
| `messages.json` | Resume messages you set with `continuum message`: default, per agent, folder and session |
| `scope.json` | All or chosen sessions, chosen and excluded entries |
| `rules.json` | Your rules overrides (optional) |
| `jobs.json`, `sessions.json`, `offsets.json` | Scheduler state |
| `history.jsonl` | Every scheduled, delivered, skipped and failed resume |
| `daemon.log`, `daemon.log.1` | Daemon log (rotates at 5 MB); `continuum logs -f` follows it |
| `hooks.log`, `hooks.log.1` | One line per hook call, when debug logging is on (rotates at 2 MB) |
| `history.jsonl.1` | The previous resume history, once `history.jsonl` passes 5 MB |
| `stopped` | Left by `continuum stop`, so hooks don't start the daemon again; removed by `continuum start` and at login |
| `daemon.stderr.log` | Crash output of the background daemon, if any |
| `wrappers/` | One folder per running `continuum run` |
| `install.json` | What `install` set up and your choices, for `update` and for restoring hooks |
| `compat.json` | Results of the last update check, and what was learned about desktop apps |
| `bin/continuum` | The installed binary |
