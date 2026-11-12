// Command fakeagent stands in for an AI coding agent in the integration
// tests. Built as "claude" or "codex", it runs like the real one does: a
// program of that name in the pane, with a readable environment. It prints a
// usage-limit message and the agent's status line, reports the limit through
// Continuum's hook, then waits for someone to type into it.
//
//	claude SESSION MARKER_FILE [HOOK_EVENT]
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: claude|codex SESSION MARKER_FILE [HOOK_EVENT]")
		os.Exit(2)
	}
	harness := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	session, marker := os.Args[1], os.Args[2]
	event := "StopFailure"
	if len(os.Args) > 3 {
		event = os.Args[3]
	}
	msg := os.Getenv("FAKE_MSG")
	if msg == "" {
		msg = "You've hit your usage limit. Try again in 4 seconds."
	}
	cwd, _ := os.Getwd()

	fmt.Println("› working on the task")
	fmt.Println("■ " + msg)
	switch harness {
	case "claude":
		fmt.Println("  ? for shortcuts")
	case "codex":
		fmt.Println("  100% context left")
	}

	payload := map[string]any{"hook_event_name": event, "session_id": session, "cwd": cwd, "last_assistant_message": msg}
	if event == "StopFailure" {
		payload["error"] = "rate_limit"
	}
	data, _ := json.Marshal(payload)
	hook := exec.Command(os.Getenv("CONTINUUM_BIN"), "hook", harness)
	hook.Stdin = strings.NewReader(string(data))
	_ = hook.Run()

	if os.Getenv("FAKE_EXIT") != "" {
		return // the user quit the agent; whatever runs next has the pane
	}
	if os.Getenv("FAKE_DIALOG") != "" {
		// An approval menu is open: typing + Enter here would approve.
		fmt.Print(" Do you want to proceed?\n ❯ 1. Yes\n   2. No\n")
	}

	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	_ = os.WriteFile(marker, []byte("RESUMED: "+line+"\n"), 0o600)
	fmt.Println("RESUMED: " + line)
	time.Sleep(120 * time.Second)
}
