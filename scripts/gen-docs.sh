#!/usr/bin/env bash
# Regenerates docs/cli.md and docs/agents.md from the binary itself, so the
# documentation always matches the code. Run with `make docs`.
set -eu
ROOT=$(cd "$(dirname "$0")/.." && pwd)
BIN=$(mktemp -d)/continuum
(cd "$ROOT" && go build -o "$BIN" ./cmd/continuum)
export CONTINUUM_HOME=$(mktemp -d)

cmds=$("$BIN" -h | awk '/^Usage:/ {g=0; next} /^[A-Z][a-z].*:$/ {g=1; next} g && /^  [a-z]/ {print $1} /^Getting started/ {exit}')
{
  echo "# Command reference"
  echo
  echo "Generated from \`continuum <command> -h\` by \`make docs\`. Every command also"
  echo "prints this help itself with \`-h\` or \`--help\`."
  echo
  echo '```text'
  "$BIN" -h
  echo '```'
  for c in $cmds; do
    echo
    echo "## $c"
    echo
    echo '```text'
    "$BIN" "$c" -h
    echo '```'
  done
} > "$ROOT/docs/cli.md"

{
  echo "# Supported agents"
  echo
  echo "Generated from the built-in rules by \`make docs\` (\`continuum rules --agents\`)."
  echo
  "$BIN" rules --agents
  cat <<'MD'

## What "supported" means

Every agent above gets:

- **Stall detection from its screen**: the wrapper (`continuum run`, which the shell
  hook applies automatically) and the tmux pane scanner read the bottom of the screen
  for limit and outage messages: the agent's own wording plus the common wording most
  agents use ("quota exceeded", "rate limit reached", "try again in 2 hours",
  `RESOURCE_EXHAUSTED`, ...). Billing, credit, sign-in and API-key errors are never
  retried.
- **Reset-time parsing**: "try again at 5:53 PM", "resets 10am (Australia/Sydney)",
  "in 2 hours 13 minutes", "in 30s", dates, weekdays, epoch times.
- **In-place resume**: the prompt is typed into the same session, in the wrapper or in
  its multiplexer pane, after checking that the agent still shows its input box and no
  menu or approval prompt is open.
- **No copies**: an agent's own CLI is only used to continue a session that is known
  to be closed (Claude Code and Codex, which report their sessions).
- **Recognition under interpreters**: agents that run under Node, Bun or Python (where
  tmux only reports `node` or `python`) are recognised by their command line.

Claude Code and Codex additionally report stalls through their hooks and session files,
so they are covered even outside a wrapper or pane, including the desktop apps.

## Shell hook: "default" and "opt-in"

The shell hook wraps the commands marked *default* when they're installed. Commands marked
*opt-in* share their name with unrelated tools (`amp`, `goose`, `pi`, `cn`, ...), so
they're only wrapped if you list them:

```json
{ "shell_hook_agents": ["claude", "codex", "gemini", "amp", "goose"] }
```

in `~/.continuum/config.json` (this list replaces the default one).

## Adding or correcting an agent

Agents are data. Add or override one in `~/.continuum/rules.json`, no rebuild needed:

```json
{
  "harnesses": {
    "myagent": {
      "display_name": "My Agent",
      "binary": "myagent",
      "process_names": ["myagent"],
      "process_arg_patterns": ["@me/myagent"],
      "use_common_patterns": true,
      "patterns": [{ "kind": "usage_limit", "regex": "(?i)credits exhausted until" }],
      "ui_markers": ["(?i)esc to cancel"],
      "wrap": { "passthrough_flags": ["-p", "--print", "--version", "-h", "--help"],
                "passthrough_subcommands": ["login", "mcp"] },
      "live_sessions": { "scan_processes": true },
      "native_commands": []
    }
  }
}
```

Check it with `continuum detect --harness myagent "the message it prints"` and
`continuum doctor`. If you get the wording for a popular agent right, please send it
upstream.

## Not covered

Agents that live inside an editor or desktop app window (Cursor and Windsurf editors,
VS Code Copilot Chat, Cline and Roo in VS Code, Zed's agent, Claude and ChatGPT desktop
chats) have no terminal Continuum can type into. Claude Code and Codex sessions in their
desktop apps are the exception. Codex app sessions are continued inside the app,
through its local server. Claude Desktop sessions are continued by themselves as soon
as Desktop lets go of them, never copied while it may still hold them. See "Desktop
apps" in the README.
MD
} > "$ROOT/docs/agents.md"
echo "wrote docs/cli.md and docs/agents.md"
