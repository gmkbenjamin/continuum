# Supported agents

Generated from the built-in rules by `make docs` (`continuum rules --agents`).

| Agent | Command | Recognised by | Shell hook | Resumes |
|---|---|---|---|---|
| [Aider](https://aider.chat) | `aider` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Amp](https://ampcode.com) | `amp` | screen (wrapper, tmux) | opt-in | in place (wrapper, multiplexer pane) |
| [Antigravity CLI](https://antigravity.google) | `agy` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Augment (auggie)](https://augmentcode.com) | `auggie` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Claude Code](https://claude.com/product/claude-code) | `claude` | hooks, transcripts, session list, screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane); closed sessions by id |
| [Cline CLI](https://cline.bot) | `cline` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Codebuff](https://codebuff.com) | `codebuff` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Codex](https://github.com/openai/codex) | `codex` | hooks, rollout files, process scan, screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane); closed sessions by id |
| [Continue CLI](https://continue.dev) | `cn` | screen (wrapper, tmux) | opt-in | in place (wrapper, multiplexer pane) |
| [GitHub Copilot CLI](https://github.com/features/copilot/cli) | `copilot` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Crush](https://github.com/charmbracelet/crush) | `crush` | screen (wrapper, tmux) | opt-in | in place (wrapper, multiplexer pane) |
| [Cursor CLI](https://cursor.com/cli) | `cursor-agent` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Factory Droid](https://factory.ai) | `droid` | screen (wrapper, tmux) | opt-in | in place (wrapper, multiplexer pane) |
| [Gemini CLI](https://github.com/google-gemini/gemini-cli) | `gemini` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Goose](https://block.github.io/goose) | `goose` | screen (wrapper, tmux) | opt-in | in place (wrapper, multiplexer pane) |
| [Grok Build](https://x.ai) | `grok` | hooks, session list, screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane); closed sessions by id |
| [Junie CLI](https://www.jetbrains.com/junie) | `junie` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Kilo Code CLI](https://kilocode.ai) | `kilocode` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Kimi CLI](https://github.com/MoonshotAI/kimi-cli) | `kimi` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Kiro CLI](https://kiro.dev/cli) | `kiro-cli` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Letta Code](https://letta.com) | `letta` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [opencode](https://opencode.ai) | `opencode` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [OpenHands CLI](https://openhands.dev) | `openhands` | screen (wrapper, tmux) | default | in place (wrapper, multiplexer pane) |
| [Pi coding agent](https://github.com/badlogic/pi-mono) | `pi` | screen (wrapper, tmux) | opt-in | in place (wrapper, multiplexer pane) |
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
- **Recognition under interpreters**: agents that run under Node, Bun or Python (where
  tmux only reports `node` or `python`) are recognised by their command line.

Claude Code and Codex additionally report stalls through their hooks and session files,
so they are covered even outside a wrapper or pane, including the desktop apps. Grok
Build reports them through its hooks (Continuum adds `~/.grok/hooks/continuum.json`;
Grok runs it in folders you have trusted) and lists its open sessions, so a closed Grok
session can be continued with `grok --resume`.

Print-mode and SDK runs (`claude -p`, Agent SDK scripts, `codex exec`, Codex subagent
threads), and agents started by another agent's tool, are left alone: nobody types into
them, and continuing one would start work nobody asked for.

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
command-line agents are covered: Cursor CLI (`cursor-agent`), Antigravity CLI (`agy`),
and any agent you run in an editor's built-in terminal. Claude Code and Codex sessions in their
desktop apps are the exception. Codex app sessions are continued inside the app,
through its local server. Claude Desktop sessions are continued by themselves as soon
as Desktop lets go of them, never copied while it may still hold them. See
[How it works](how-it-works.md#desktop-apps).
