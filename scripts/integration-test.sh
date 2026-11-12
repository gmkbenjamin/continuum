#!/usr/bin/env bash
# End-to-end test: a fake agent stalls on a usage limit inside each supported
# multiplexer (all detached / unfocused) and inside `continuum run` with no
# terminal at all. Continuum must type the resume prompt into every one.
#
# Uses an isolated CONTINUUM_HOME and never touches real agent settings.
set -u
ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=${WORK:-$(mktemp -d "${TMPDIR:-/tmp}/continuum-it.XXXXXX")}
export CONTINUUM_HOME="$WORK/home"
export CONTINUUM_BIN="$WORK/continuum"
# The test's own tmux server: the pane scanner watches only this one, never
# yours.
IT_TMUX_SOCKET="${TMUX_TMPDIR:-/tmp}/tmux-$(id -u)/continuum-it"
FAKE="$ROOT/scripts/fake-agent.sh"
mkdir -p "$CONTINUUM_HOME" "$WORK/claude/projects" "$WORK/codex/sessions" "$WORK/markers"
chmod +x "$FAKE"

(cd "$ROOT" && go build -o "$CONTINUUM_BIN" ./cmd/continuum) || exit 1
# Stand-ins for Claude Code and Codex: real programs of those names, as the
# agents are (Continuum checks which program runs in a pane before typing).
mkdir -p "$WORK/bin"
(cd "$ROOT" && go build -o "$WORK/bin/claude" ./scripts/fakeagent) || exit 1
cp "$WORK/bin/claude" "$WORK/bin/codex"

cat > "$CONTINUUM_HOME/config.json" <<EOF
{
  "claude_home": "$WORK/claude",
  "codex_home": "$WORK/codex",
  "notify": false,
  "delay_after_reset_seconds": 1,
  "native_grace_seconds": {"claude": 0, "codex": 0},
  "strategies": ["wrapper", "multiplexer"],
  "binaries": {"claude": "$WORK/no-such-claude", "codex": "$WORK/no-such-codex"},
  "poll_seconds": 1,
  "scan_panes": true,
  "scan_tmux_socket": "$IT_TMUX_SOCKET"
}
EOF

PROMPT_PREFIX="Your usage limit has reset"
pass=0; fail=0; skipped=()
declare -a NAMES=()

have() { command -v "$1" >/dev/null 2>&1; }
agent_cmd() { # harness session [event]
  # env -u: the fake agents must not inherit the pid of a real Claude session
  # running this script (Continuum would rightly refuse to touch it).
  printf 'env -u CLAUDE_PID -u CLAUDECODE CONTINUUM_HOME=%q CONTINUUM_BIN=%q %q %q %q %q' \
    "$CONTINUUM_HOME" "$CONTINUUM_BIN" "$WORK/bin/$1" "$2" "$WORK/markers/$2" "${3:-StopFailure}"
}

cleanup() {
  have tmux && tmux -L continuum-it kill-server 2>/dev/null
  have screen && screen -S continuum-it -X quit >/dev/null 2>&1
  have zellij && zellij delete-session --force continuum-it >/dev/null 2>&1
  have rex && [ -n "${REX_IT:-}" ] && rex kill "$REX_IT" >/dev/null 2>&1
  have herdr && { herdr session stop continuum-it >/dev/null 2>&1; herdr session delete continuum-it >/dev/null 2>&1; }
  [ -n "${WRAP_PID:-}" ] && { kill "$WRAP_PID" 2>/dev/null; wait "$WRAP_PID" 2>/dev/null; }
  "$CONTINUUM_BIN" stop >/dev/null 2>&1
  # Anything still running from this test's directory (e.g. screen's children).
  pkill -f "$WORK" 2>/dev/null
}
trap cleanup EXIT

# --- tmux (detached session, never attached: the "minimised window" case) ---
if have tmux; then
  tmux -L continuum-it new-session -d -s it -x 160 -y 40 "$(agent_cmd claude it-tmux StopFailure)"
  NAMES+=(it-tmux)
else skipped+=(tmux); fi

# --- safety: an approval menu is open, Continuum must NOT type into it ---
if have tmux; then
  tmux -L continuum-it new-window -d -t it "FAKE_DIALOG=1 $(agent_cmd codex it-dialog)"
fi

# --- safety: the agent quit after its limit and another program now runs in
# the pane (the old screen still shows the agent): Continuum must NOT type ---
if have tmux; then
  tmux -L continuum-it new-window -d -t it "FAKE_EXIT=1 $(agent_cmd claude it-exited); exec cat > $WORK/typed-into-cat"
fi

# --- GNU screen (detached) ---
if have screen; then
  screen -dmS continuum-it bash -c "$(agent_cmd codex it-screen)"
  NAMES+=(it-screen)
else skipped+=(screen); fi

# --- zellij (background session) ---
if have zellij; then
  zellij attach --create-background continuum-it >/dev/null 2>&1
  sleep 1
  zellij --session continuum-it run --name it -- bash -c "$(agent_cmd codex it-zellij)" >/dev/null 2>&1
  NAMES+=(it-zellij)
else skipped+=(zellij); fi

# --- herdr (isolated named session, headless server) ---
if have herdr; then
  (herdr --session continuum-it server >/dev/null 2>&1 &)
  sleep 1.5
  pane=$(herdr --session continuum-it workspace create --cwd "$WORK" --no-focus 2>/dev/null |
    python3 -c 'import json,sys; print(json.load(sys.stdin)["result"]["root_pane"]["pane_id"])' 2>/dev/null)
  if [ -n "$pane" ]; then
    sleep 1
    herdr --session continuum-it pane run "$pane" "$(agent_cmd codex it-herdr)" >/dev/null
    NAMES+=(it-herdr)
  else skipped+=(herdr); fi
else skipped+=(herdr); fi

# --- Rex (separate session, not focused) ---
if have rex && rex whoami >/dev/null 2>&1; then
  REX_IT=$(rex new continuum-it --json -- bash -c "$(agent_cmd claude it-rex StopFailure)" 2>/dev/null |
    python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get("session_id") or d.get("id") or d.get("session",{}).get("session_id",""))' 2>/dev/null)
  [ -n "$REX_IT" ] && NAMES+=(it-rex) || skipped+=(rex)
else skipped+=(rex); fi

# --- an agent running under node, no hooks, found by the pane scanner ---
# tmux only reports "node" for it; Continuum must recognise Gemini CLI from the
# command line, see the limit on screen and type into the pane. The scanner
# watches the test's own tmux server (scan_tmux_socket).
if have tmux && have node; then
  GEM="$WORK/node_modules/@google/gemini-cli/dist"
  mkdir -p "$GEM"
  cat > "$GEM/index.js" <<'JS'
const fs = require('fs'); const marker = process.argv[2];
process.stdout.write('Gemini CLI\n✕ [API Error: You have exhausted your daily quota. Please try again in 3s.]\n\n> Type your message or @path/to/file\n');
let buf = '';
process.stdin.setEncoding('utf8');
process.stdin.on('data', d => {
  buf += d; const i = buf.search(/[\r\n]/);
  if (i >= 0) { fs.writeFileSync(marker, 'RESUMED: ' + buf.slice(0, i) + '\n'); setTimeout(() => process.exit(0), 120000); }
});
JS
  tmux -L continuum-it new-session -d -s it-node -x 160 -y 40 "node $GEM/index.js $WORK/markers/it-node"
  NAMES+=(it-node)
else skipped+=(node-agent); fi

# --- continuum run, no terminal and no multiplexer at all ---
(cd "$WORK" && exec env -u TMUX -u TMUX_PANE -u STY -u ZELLIJ -u ZELLIJ_PANE_ID -u REX_BLOCK -u HERDR_PANE_ID \
  "$CONTINUUM_BIN" run --harness generic -- bash "$FAKE" generic it-wrapper "$WORK/markers/it-wrapper" \
  </dev/null >"$WORK/wrapper.out" 2>&1) &
WRAP_PID=$!
NAMES+=(it-wrapper)

# --- wait for every resume ---
deadline=$((SECONDS + 90))
for n in "${NAMES[@]}"; do
  while [ ! -s "$WORK/markers/$n" ] && [ $SECONDS -lt $deadline ]; do sleep 0.5; done
  if [ -s "$WORK/markers/$n" ] && grep -q "RESUMED: $PROMPT_PREFIX" "$WORK/markers/$n"; then
    echo "✓ ${n#it-}: resumed"
    pass=$((pass + 1))
  else
    echo "✗ ${n#it-}: not resumed"
    fail=$((fail + 1))
  fi
done
if have tmux; then
  if [ -s "$WORK/markers/it-dialog" ]; then
    echo "✗ safety: typed into an open approval menu"
    fail=$((fail + 1))
  elif "$CONTINUUM_BIN" list --json | grep -q 'menu or prompt open'; then
    echo "✓ safety: refused to type into an open approval menu (will retry later)"
    pass=$((pass + 1))
  else
    echo "✗ safety: no refusal recorded for the approval menu"
    fail=$((fail + 1))
  fi
fi
if have tmux; then
  # Its resume was due with the others; give it a moment to be tried.
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    "$CONTINUUM_BIN" list --json | grep -q '"it-exited".*"attempts": *[1-9]\|it-exited.*no longer runs\|it-exited.*runs cat' && break
    sleep 1
  done
  if [ -s "$WORK/typed-into-cat" ]; then
    echo "✗ safety: typed into the program that replaced the agent"
    fail=$((fail + 1))
  elif "$CONTINUUM_BIN" list --json | grep -q 'no longer runs Claude Code\|runs cat, not Claude Code'; then
    echo "✓ safety: refused to type into a pane the agent had left"
    pass=$((pass + 1))
  else
    echo "✗ safety: no refusal recorded for the pane the agent had left"
    fail=$((fail + 1))
  fi
fi
[ ${#skipped[@]} -gt 0 ] && echo "· skipped (not installed): ${skipped[*]}"

echo
"$CONTINUUM_BIN" list --all
if [ $fail -gt 0 ]; then
  echo; echo "--- daemon log ---"; tail -40 "$CONTINUUM_HOME/daemon.log"
  echo "--- history ---"; cat "$CONTINUUM_HOME/history.jsonl"
fi
echo
echo "$pass passed, $fail failed"
[ $fail -eq 0 ]
