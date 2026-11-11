#!/usr/bin/env bash
# A stand-in for an AI coding agent, used by the integration tests.
# It prints a usage-limit message, reports it through Continuum's hook the way
# Claude Code or Codex would, then waits for someone to type into it.
#
#   fake-agent.sh HARNESS SESSION MARKER_FILE [HOOK_EVENT]
harness=$1 session=$2 marker=$3 event=${4:-Stop}
msg=${FAKE_MSG:-"You've hit your usage limit. Try again in 4 seconds."}

echo "› working on the task"
echo "■ $msg"
if [ "$event" = "StopFailure" ]; then
  payload=$(printf '{"hook_event_name":"StopFailure","session_id":"%s","cwd":"%s","error":"rate_limit","last_assistant_message":"%s"}' "$session" "$PWD" "$msg")
else
  payload=$(printf '{"hook_event_name":"%s","session_id":"%s","cwd":"%s","last_assistant_message":"%s"}' "$event" "$session" "$PWD" "$msg")
fi
printf '%s' "$payload" | "$CONTINUUM_BIN" hook "$harness"
if [ -n "${FAKE_DIALOG:-}" ]; then
  # An approval menu is open: typing + Enter here would approve the command.
  printf ' Do you want to proceed?\n ❯ 1. Yes\n   2. No\n'
fi

IFS= read -r line
echo "RESUMED: $line" > "$marker"
echo "RESUMED: $line"
sleep 120
