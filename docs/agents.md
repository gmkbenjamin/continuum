# Supported agents

Generated from the built-in rules by `make docs` (`continuum rules --agents`).

| Agent | Command | Recognised by | Shell hook | Resumes |
|---|---|---|---|---|
| [Aider](https://aider.chat) | `aider` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Amp](https://ampcode.com) | `amp` | screen (wrapper, tmux) | opt-in | in place (wrapper, multiplexer pane) |
| [Antigravity CLI](https://antigravity.google) | `agy` | hooks, transcripts, screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Augment (auggie)](https://augmentcode.com) | `auggie` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Claude Code](https://claude.com/product/claude-code) | `claude` | hooks, transcripts, session list, screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane); closed sessions by id |
| [Cline CLI](https://cline.bot) | `cline` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Codebuff](https://codebuff.com) | `codebuff` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Codex](https://github.com/openai/codex) | `codex` | hooks, rollout files, process scan, screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane); closed sessions by id |
| [Continue CLI](https://continue.dev) | `cn` | screen (wrapper, tmux) | opt-in | in place (wrapper, multiplexer pane) |
| [GitHub Copilot CLI](https://github.com/features/copilot/cli) | `copilot` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Crush](https://github.com/charmbracelet/crush) | `crush` | screen (wrapper, tmux) | opt-in | in place (wrapper, multiplexer pane) |
| [Cursor CLI](https://cursor.com/cli) | `cursor-agent` or `agent` | hooks, screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Factory Droid](https://factory.ai) | `droid` | screen (wrapper, tmux) | opt-in | in place (wrapper, multiplexer pane) |
| [Gemini CLI](https://github.com/google-gemini/gemini-cli) | `gemini` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Goose](https://block.github.io/goose) | `goose` | screen (wrapper, tmux) | opt-in | in place (wrapper, multiplexer pane) |
| [Grok Build](https://x.ai) | `grok` | hooks, session list, screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane); closed sessions by id |
| [Junie CLI](https://www.jetbrains.com/junie) | `junie` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Kilo Code CLI](https://kilocode.ai) | `kilo` or `kilocode` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Kimi CLI](https://github.com/MoonshotAI/kimi-cli) | `kimi` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Kiro CLI](https://kiro.dev/cli) | `kiro-cli` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Letta Code](https://letta.com) | `letta` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| Muse Code | `muse` | hooks, session list, screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [opencode](https://opencode.ai) | `opencode` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [OpenHands CLI](https://openhands.dev) | `openhands` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Pi coding agent](https://github.com/earendil-works/pi) | `pi` | screen (wrapper, tmux) | opt-in | in place (wrapper, multiplexer pane) |
| [Plandex](https://plandex.ai) | `plandex` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Qwen Code](https://github.com/QwenLM/qwen-code) | `qwen` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Rovo Dev CLI](https://www.atlassian.com/rovo) | `acli rovodev` | screen (wrapper, tmux) | opt-in | in place (wrapper, multiplexer pane) |
| [Mistral Vibe](https://mistral.ai) | `vibe` | screen (wrapper, tmux) | opt-in | in place (wrapper, multiplexer pane) |

Any other program in a tmux pane named `agent`, `forge`, `q`, `ra-aid`, `gptme`, `mentat`, `sweep`, `devin`, `warp-agent` is watched with the common patterns.

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
  to be closed (Claude Code, Codex and Grok Build, which report their sessions).
- **Full-screen programs**: for agents that draw by moving the cursor and keep
  redrawing (opencode 2), the wrapper reads the rendered screen too, where the message
  is whole. A message already shown when you last sent a prompt doesn't count again.
- **Recognition under interpreters**: agents that run under Node, Bun or Python (where
  tmux only reports `node` or `python`) are recognised by their command line.

Claude Code and Codex additionally report stalls through their hooks and session files,
so they are covered even outside a wrapper or pane, including the desktop apps. Grok
Build reports them through its hooks (Continuum adds `~/.grok/hooks/continuum.json`;
Grok runs it in folders you have trusted) and lists its open sessions, so a closed Grok
session can be continued with `grok --resume`.

Muse Code reports through hooks Continuum adds to `~/.config/muse/settings.json`
(`$XDG_CONFIG_HOME/muse`), and `muse sessions --json` tells which sessions are open.
Muse runs hooks with a cleaned environment, so Continuum reads where the session runs
(its wrapper or multiplexer pane) from the Muse process itself. A Muse session is
resumed in place; Muse has no command to continue a closed session headlessly.

The Cursor CLI reports its sessions through `~/.cursor/hooks.json`. Its `stop` hook
doesn't say which error ended a turn, so limits are still read off the screen. Cursor
now installs its command as `agent`: the shell hook wraps `agent` too, but only while
that name is a link into Cursor's own install. Cursor also runs Claude Code's hooks;
with its own hook installed, those runs are ignored rather than reported twice.
Cursor's `agent persist` manages its own terminal sessions and bypasses the shell
wrapper.

OpenCode's terminal interface is wrapped and resumed in place by default.

Antigravity CLI (`agy`) uses three layered detection sources: lifecycle hooks
Continuum adds under `"continuum"` in `~/.gemini/config/hooks.json` (`SessionStart`,
`PreInvocation`, `Stop`), its CLI transcripts under
`~/.gemini/antigravity-cli/brain/<id>/.system_generated/logs/transcript.jsonl`,
and screen detection in Continuum's wrapper or tmux pane. Only CLI sessions count
(Antigravity 2.0 and IDE hooks sharing `~/.gemini/config/hooks.json` are ignored).
Antigravity sessions are resumed in place.

Continuum's skill in `~/.claude/skills` is read by Grok Build, Muse Code, the Cursor
CLI and opencode as well as Claude Code.

Print-mode and SDK runs (`claude -p`, Agent SDK scripts, `codex exec`, Codex subagent
threads, `grok -p`, `muse exec`, `agent -p`), and agents started by another agent's tool, are left alone: nobody types into
them, and continuing one would start work nobody asked for.

## Compatibility checks

Continuum's tests exercise interactive and one-shot launch rules and replay usage
errors with simulated resets. Actual limits depend on the provider, plan, and
client version.

- Kilo's current terminal command is `kilo`. The shell hook also recognises
  `kilocode`. `kilo run` and server commands run directly.
  [Kilo CLI reference](https://kilo.ai/docs/code-with-ai/platforms/cli)
- Cline's bare command and `cline --tui` start interactive sessions. A positional
  prompt alone runs once. OpenHands can seed an interactive session with `--task`
  or `--file`. [Cline CLI source guide](https://github.com/cline/cline/blob/main/apps/cli/README.md) ·
  [OpenHands CLI reference](https://github.com/OpenHands/docs/blob/main/openhands/usage/cli/command-reference.mdx)
- Qwen Code can report token-plan reset times as `MM-DD HH:MM:SS UTC`. Continuum
  uses that date and time when scheduling a resume.
  [Qwen Code quota handling](https://github.com/QwenLM/qwen-code/blob/main/packages/core/src/utils/quotaErrorDetection.ts)

## OpenCode desktop and CLI

Continuum can watch and resume sessions through the OpenCode v2 server shared by
the desktop app and terminal. The API behavior was checked against OpenCode 2.3.2.

1. Start a local server with `opencode serve --hostname 127.0.0.1 --port 4096`.
2. In the shell where you connect, export `OPENCODE_PASSWORD` with the server
   password. For the CLI, run `opencode --server http://127.0.0.1:4096`. For the
   desktop app, run `opencode api --server http://127.0.0.1:4096 server.pair` and
   paste `http://127.0.0.1:4096/auth/connect/<code>` into the app's server address
   field, using the returned `code`.
3. Set `"opencode_server": "http://127.0.0.1:4096"` in Continuum's
   `config.json`. The daemon needs the server password in `OPENCODE_PASSWORD`.
   To run it in the foreground, stop any existing Continuum daemon and export
   the password before running `continuum watch`.

Server mode replaces OpenCode's screen-based detection. It watches the 20 most
recent top-level sessions on that server, including top-level sessions created by
scripts. Continuum's scope and exclusions apply. Delivery uses the `native`
strategy with `allow_headless` enabled. Continuum lets OpenCode finish any built-in
retries and checks for activity and queued work before sending a resume prompt to
the existing session. The check and submission are separate API calls, so
messages arriving concurrently can also enter the queue.
[OpenCode server setup](https://opencode.ai/docs/cli/web) ·
[OpenCode session API](https://opencode.ai/docs/api#session)

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

Agents that live inside an editor or desktop app window (the Cursor and Windsurf editors'
own agents, VS Code Copilot Chat, Cline and Roo in VS Code, Zed's agent, the Antigravity
editor, Claude and ChatGPT desktop chats) have no terminal Continuum can type into. Their
command-line agents are covered: Cursor CLI (`agent`, `cursor-agent`), Antigravity CLI (`agy`),
and any agent you run in an editor's built-in terminal. Claude Code and Codex sessions in their
desktop apps and OpenCode's shared server are supported. Codex app sessions continue
inside the app through its local server. Claude Desktop sessions continue as soon
as Desktop lets go of them, never copied while it may still hold them. See
[How it works](how-it-works.md#desktop-apps).
