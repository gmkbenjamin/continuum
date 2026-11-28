// Package hooks connects Continuum to agent lifecycle hooks: it handles hook
// invocations (fast, silent, never failing) and installs/removes Continuum's
// entries in the agents' settings files.
package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/mux"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/resettime"
	"github.com/continuum-dev/continuum/internal/rules"
)

// Input is the union of hook payload fields Continuum reads. Agents add
// fields over time; unknown ones are ignored.
type Input struct {
	HookEventName        string          `json:"hook_event_name"`
	SessionID            string          `json:"session_id"`
	TranscriptPath       string          `json:"transcript_path"`
	Cwd                  string          `json:"cwd"`
	Error                json.RawMessage `json:"error"`
	ErrorDetails         string          `json:"error_details"`
	LastAssistantMessage string          `json:"last_assistant_message"`
	Reason               string          `json:"reason"`
	Source               string          `json:"source"`
	// Cursor CLI: conversation_id (also sent as session_id), the workspace
	// folders instead of cwd, the outcome of a turn on stop, and the reply
	// text on afterAgentResponse.
	ConversationID string   `json:"conversation_id"`
	WorkspaceRoots []string `json:"workspace_roots"`
	Status         string   `json:"status"`
	Text           string   `json:"text"`
	// Model: the model of the turn (Cursor sends it on beforeSubmitPrompt
	// and afterAgentResponse).
	Model string `json:"model"`
	// Antigravity CLI: camelCase protojson fields.
	HookEventNameCamel        string   `json:"hookEventName"`
	ConversationIDCamel       string   `json:"conversationId"`
	TranscriptPathCamel       string   `json:"transcriptPath"`
	WorkspacePaths            []string `json:"workspacePaths"`
	ModelName                 string   `json:"modelName"`
	TerminationReason         string   `json:"terminationReason"`
	ErrorDetailsCamel         string   `json:"errorDetails"`
	ErrorMessageCamel         string   `json:"errorMessage"`
	LastAssistantMessageCamel string   `json:"lastAssistantMessage"`
}

func rawString(raw json.RawMessage) string {
	code, msg := rawErrorParts(raw)
	if code != "" {
		return code
	}
	return msg
}

func rawErrorParts(raw json.RawMessage) (code, msg string) {
	if len(raw) == 0 {
		return "", ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s), ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		for _, k := range []string{"type", "code", "error"} {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				code = strings.TrimSpace(v)
				break
			}
		}
		for _, k := range []string{"message", "userErrorMessage", "user_message", "details"} {
			if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
				msg = strings.TrimSpace(v)
				break
			}
		}
	}
	return code, msg
}

// BuildEvent turns a hook payload plus the hook's environment into an event.
// ok is false when the hook carries nothing Continuum needs.
func BuildEvent(r *rules.Rules, harness string, in Input, env map[string]string, now time.Time) (core.Event, bool) {
	h := r.Harness(harness)
	if in.HookEventName == "" {
		in.HookEventName = in.HookEventNameCamel
	}
	if in.SessionID == "" {
		in.SessionID = in.ConversationID
	}
	if in.SessionID == "" {
		in.SessionID = in.ConversationIDCamel
	}
	if in.TranscriptPath == "" {
		in.TranscriptPath = in.TranscriptPathCamel
	}
	if in.Cwd == "" && len(in.WorkspaceRoots) > 0 {
		in.Cwd = in.WorkspaceRoots[0]
	}
	if in.Cwd == "" && len(in.WorkspacePaths) > 0 {
		in.Cwd = in.WorkspacePaths[0]
	}
	if in.Model == "" {
		in.Model = in.ModelName
	}
	if in.ErrorDetails == "" {
		in.ErrorDetails = in.ErrorDetailsCamel
	}
	if in.ErrorDetails == "" {
		in.ErrorDetails = in.ErrorMessageCamel
	}
	if in.LastAssistantMessage == "" {
		in.LastAssistantMessage = in.LastAssistantMessageCamel
	}
	ev := core.Event{
		Harness: harness, SessionID: in.SessionID, Cwd: in.Cwd, TranscriptPath: in.TranscriptPath,
		At: now, Source: "hook:" + in.HookEventName,
	}
	if id := env["CONTINUUM_WRAPPER_ID"]; id != "" {
		ev.Location = &core.Location{Kind: "wrapper", Target: id}
	} else {
		ev.Location = mux.LocateFromEnv(r, env)
	}
	drv := hookDriverFor(harness)
	drv.EnrichEvent(in, env, &ev)
	if h.IgnoredEntrypoint(ev.Entrypoint) {
		return ev, false // print mode or an SDK script: nobody types into it
	}
	if st, err := os.Stat(in.TranscriptPath); err == nil {
		ev.TranscriptSize = st.Size()
	}

	switch strings.ToLower(in.HookEventName) {
	case "sessionstart":
		ev.Kind = core.KindSessionStart
		ev.Model = in.Model
	case "userpromptsubmit", "beforesubmitprompt", "preinvocation":
		// A turn starts: its requests go out no earlier than now.
		ev.Kind = core.KindActivity
		ev.TurnStart = true
		ev.Model = in.Model
	case "sessionend":
		ev.Kind = core.KindSessionEnd
	case "stopfailure":
		text := strings.TrimSpace(in.LastAssistantMessage + "\n" + in.ErrorDetails)
		errCode := rawString(in.Error)
		switch {
		case h.NeverResume(text + "\n" + errCode):
			// Billing and sign-in errors need a person, also when only the
			// error code says so (the rules name codes like
			// authentication_failed).
			ev.Kind = core.KindNeverResume
		default:
			if m := h.Classify(text, 8); m != nil {
				ev.Kind = m.Kind
			} else if k, ok := h.StopFailureKinds[errCode]; ok {
				ev.Kind = k
			}
		}
		if ev.Kind == "" {
			// Another error (a request the API refused, say): not something
			// to resume, but right after a resume it means it didn't take.
			ev.Kind = core.KindTurnFailed
		}
		if text == "" && (ev.Kind == core.KindTurnFailed || ev.Kind == core.KindNeverResume) {
			text = errCode // say what it was
		}
		ev.Message = text
		if ev.Kind == core.KindUsageLimit {
			if t, ok := resettime.Parse(text, now, time.Local); ok {
				ev.ResetAt = t
			}
		}
	case "stop", "afteragentresponse":
		if handled, keep := drv.HandleStop(h, in, now, &ev); handled {
			if !keep {
				return ev, false
			}
			break
		}
		if strings.EqualFold(in.Status, "error") {
			// Cursor ended the turn on an error but doesn't say which: the
			// screen tells whether it is a limit. Reporting it as activity
			// would read as "working normally".
			return ev, false
		}
		if in.LastAssistantMessage == "" {
			in.LastAssistantMessage = in.Text
		}
		// A turn ended normally. Real limits and API errors arrive through
		// StopFailure (and the transcript), so this is activity, unless the
		// reply reads like a limit message: then it is reported as a reply,
		// which tells the daemon that the same text read off the screen is
		// not a stall. All of the reply counts, not only its last lines: the
		// wrapper reads up to the last 40 lines of the screen.
		reply := clip(in.LastAssistantMessage, 8000)
		if h.Classify(reply, strings.Count(reply, "\n")+1) != nil {
			ev.Kind = core.KindReply
			ev.Message = reply
		} else {
			ev.Kind = core.KindActivity
			if h.StopHookAnswers && in.LastAssistantMessage != "" {
				// The model answered. The hook doesn't say when the turn's
				// requests went out: the daemon takes the turn's start,
				// which the prompt-submit hook reported, and sets RequestAt.
				ev.Answered = true
				ev.Model = in.Model
			}
		}
	default:
		ev.Kind = core.KindActivity
	}
	if ev.PID == 0 && ev.Kind == core.KindSessionStart {
		ev.PID = findAncestor(h)
	}
	return ev, true
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// hasOwnHook reports whether Continuum's own hooks for harness are installed
// (then that agent also runs them, and a borrowed hook of another agent's
// must not report the same event twice).
func hasOwnHook(harness string) bool {
	cfg, _ := core.LoadConfig(core.GetPaths().Config)
	for _, t := range Targets(cfg) {
		if t.Harness != harness {
			continue
		}
		// Parsed, not searched as text: in the file the command's quotes
		// are escaped, which a plain text match misses.
		ok, _ := Installed(t)
		return ok
	}
	return false
}

// ProcTree lists processes for the checks below. Tests replace it.
var ProcTree = func() map[int]proc.Node { return proc.Ancestry(os.Getppid()) }

// cliAgent returns the agent a process name belongs to. Desktop apps (named
// with a capital, like "Claude" or "Cursor") don't count.
func cliAgent(r *rules.Rules, name string) string {
	if name == "" || unicode.IsUpper([]rune(name)[0]) {
		return ""
	}
	for _, n := range r.HarnessNames() {
		if r.Harness(n).IsHarnessProcess(name) {
			return n
		}
	}
	return ""
}

// nearestAgent is the agent running the hook: the closest ancestor of pid
// that is an agent's CLI ("" when none is).
func nearestAgent(r *rules.Rules, tree map[int]proc.Node, pid int) string {
	a, _ := NearestAgent(r, tree, pid)
	return a
}

// NearestAgent returns the agent pid runs under: the closest ancestor (or
// pid itself) that is an agent's CLI, and that process's pid.
func NearestAgent(r *rules.Rules, tree map[int]proc.Node, pid int) (string, int) {
	for steps := 0; pid > 1 && steps < 30; steps++ {
		n, ok := tree[pid]
		if !ok {
			return "", 0
		}
		if a := agentAt(r, n, pid); a != "" {
			return a, pid
		}
		pid = n.PPID
	}
	return "", 0
}

// agentAt is the agent's CLI that process pid (named n.Name) runs, or "".
// By name; or, when the name is an interpreter (node, bun, python) or a
// command alias (the Cursor CLI shows as `agent`, or as `node` on Linux), by
// its exact command line.
func agentAt(r *rules.Rules, n proc.Node, pid int) string {
	alias := n.Name != "" && isAlias(r, n.Name)
	if !alias {
		if a := cliAgent(r, n.Name); a != "" {
			return a
		}
	}
	if n.Name == "" || unicode.IsUpper([]rune(n.Name)[0]) || !(alias || r.IsInterpreter(n.Name)) {
		return ""
	}
	// An alias name may be another program's too (`agent` is also a generic
	// agent name): the command line decides first.
	if args := ProcArgs(pid); len(args) > 0 {
		line := strings.Join(args, " ")
		for _, name := range r.HarnessNames() {
			if r.Harness(name).MatchesCommandLine(line) {
				return name
			}
		}
	}
	if alias {
		return cliAgent(r, n.Name)
	}
	return ""
}

func isAlias(r *rules.Rules, name string) bool {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(name)), ".exe")
	for _, h := range r.HarnessNames() {
		for _, a := range r.Harness(h).CommandAliases {
			if strings.EqualFold(a.Command, base) {
				return true
			}
		}
	}
	return false
}

// nested walks up from pid: past the agent running the hook (and helper
// processes of the same name), looking for another agent further up.
// Desktop apps (named with a capital, like "Claude") don't count: they host
// sessions people work in.
func nested(r *rules.Rules, tree map[int]proc.Node, pid int) bool {
	if len(tree) == 0 {
		return false
	}
	steps := 0
	var ownAgent string
	for ; pid > 1 && steps < 30; steps++ {
		n, ok := tree[pid]
		if !ok {
			return false
		}
		if ownAgent = agentAt(r, n, pid); ownAgent != "" {
			break
		}
		pid = n.PPID
	}
	if pid <= 1 || steps >= 30 {
		return false
	}
	own := tree[pid].Name
	// Helper processes of the agent's own (same name, and not another
	// agent: `node` may also run a different one). Bounded: on Windows a
	// parent id can be stale and reused, which can make a loop.
	for pid = tree[pid].PPID; pid > 1 && steps < 60 && tree[pid].Name == own; pid = tree[pid].PPID {
		if a := agentAt(r, tree[pid], pid); a != "" && a != ownAgent {
			return true
		}
		steps++
	}
	for ; pid > 1 && steps < 60; steps++ {
		n, ok := tree[pid]
		if !ok {
			return false
		}
		if agentAt(r, n, pid) != "" {
			return true
		}
		pid = n.PPID
	}
	return false
}

// listsProcesses: the agent's own list of open sessions names the process
// holding each (a command or registry, not a process scan).
func listsProcesses(h *rules.Harness) bool {
	ls := h.LiveSessions
	return ls != nil && (len(ls.Command) > 0 || ls.RegistryDir != "" || ls.RegistryFile != "")
}

// findAncestor returns the pid of the nearest ancestor process that is the
// agent itself (the hook runs as its child or grandchild).
func findAncestor(h *rules.Harness) int {
	pid := os.Getppid()
	for i := 0; i < 6 && pid > 1; i++ {
		name := proc.Name(pid)
		if name == "" {
			return 0
		}
		if h.IsHarnessProcess(name) {
			return pid
		}
		pid = proc.Parent(pid)
	}
	return 0
}

// Handle is the body of `continuum hook <harness>`. It never returns an error
// to the agent and prints nothing (some hooks feed stdout to the model). It
// reports whether an event was accepted and spooled.
//
// The agent waits for it, before every prompt and at the end of every turn,
// so it starts no program: what it needs about processes (the hook's
// ancestors, an agent's arguments and environment) is read from the kernel.
// Running ps took over 10 seconds on a busy or monitored computer.
func Handle(harness string, stdin io.Reader, environ []string) bool {
	defer func() { _ = recover() }()
	p := core.GetPaths()
	if err := p.Ensure(); err != nil {
		return false
	}
	ppid, at := os.Getppid(), time.Now()
	data, _ := io.ReadAll(io.LimitReader(stdin, 4<<20))
	var in Input
	if json.Unmarshal(data, &in) != nil {
		debugHook(p, harness, "payload is not JSON; ignored")
		return false
	}
	eventName := in.HookEventName
	if eventName == "" {
		eventName = in.HookEventNameCamel
	}
	r, _ := rules.Cached(p.Rules)
	if r == nil {
		return false
	}
	env := mux.EnvMap(environ)
	tree := ProcTree()
	env = withAgentEnv(r, env, tree, ppid)
	if other := r.HookHarness(env); other != "" && other != harness {
		// Another agent ran this agent's hook (Grok Build and Cursor run
		// Claude Code's), unless the variable was only inherited by this agent.
		if a := nearestAgent(r, tree, ppid); a == "" || a == other {
			if hasOwnHook(other) {
				debugHook(p, harness, "run by "+other+", which runs Continuum's own "+other+" hook too; ignored")
				return false
			}
			debugHook(p, harness, "run by "+other+"; reporting it as "+other)
			harness = other
		}
	}
	ev, ok := BuildEvent(r, harness, in, env, at)
	if !ok {
		debugHook(p, harness, eventName+": nothing to report")
		return false
	}
	// (Claude Code's hooks normally give CLAUDE_PID; this fills in when not.)
	if ev.PID == 0 && listsProcesses(r.Harness(harness)) {
		// An agent whose own session list names each session's process:
		// report the process on every event (from the process tree already
		// read), so the daemon knows which process a conversation is being
		// used in when two hold it.
		if name, pid := NearestAgent(r, tree, ppid); name == harness {
			ev.PID = pid
		}
	}
	drv := hookDriverFor(harness)
	drv.SanitizeProcess(r, tree, ppid, &ev)
	if h := r.Harness(harness); h.HookRequiresCLI {
		if ok, reason := drv.ValidateCLI(in, env, r, tree, ppid, ev); !ok {
			debugHook(p, harness, eventName+": "+reason)
			return false
		}
	}
	if !interactive(r, harness, tree, ppid) {
		// Print mode or a headless subcommand (`grok -p`, `muse exec`,
		// `agent -p`): nobody types into it.
		debugHook(p, harness, eventName+": not an interactive session; ignored")
		return false
	}
	if nested(r, tree, ppid) {
		// An agent another agent started (a `claude -p` from a Bash tool,
		// a script): it inherited its parent's pane or wrapper, and nobody
		// types into it.
		debugHook(p, harness, eventName+": started by another agent; ignored")
		return false
	}
	debugHook(p, harness, fmt.Sprintf("%s: %s session=%s where=%s", eventName, ev.Kind, ev.SessionID, ev.Location.String()))
	if ev.Kind == core.KindActivity {
		// Routine notes are only useful to a running daemon; never let them
		// pile up when it is switched off.
		if entries, err := os.ReadDir(p.Spool); err == nil && len(entries) > 500 {
			return false
		}
	}
	_ = core.SpoolEvent(p, ev)
	return true
}

// isAntigravityCLI reports whether a hook from ~/.gemini/config/hooks.json was
// run by the Antigravity CLI (`agy`) rather than Antigravity 2.0 or the IDE.
func isAntigravityCLI(in Input, env map[string]string, r *rules.Rules, tree map[int]proc.Node, ppid int) bool {
	tp := filepath.ToSlash(in.TranscriptPath)
	if tp == "" {
		tp = filepath.ToSlash(in.TranscriptPathCamel)
	}
	appDir := env["ANTIGRAVITY_APP_DATA_DIR"]
	lsVer := env["ANTIGRAVITY_LS_VERSION"]
	if tp != "" && !strings.Contains(tp, "/antigravity-cli/") &&
		(strings.Contains(tp, "/antigravity/") || strings.Contains(tp, "/antigravity-ide/")) {
		return false
	}
	if appDir != "" && !core.IsAntigravityCLIDataDir(appDir, lsVer) {
		return false
	}
	if lv := strings.ToLower(lsVer); lv != "" && !strings.HasPrefix(lv, "cli-") {
		return false
	}
	if env["CONTINUUM_WRAPPER_ID"] != "" ||
		strings.Contains(tp, "/antigravity-cli/") ||
		strings.HasPrefix(strings.ToLower(lsVer), "cli-") {
		return true
	}
	if len(tree) == 0 {
		return true
	}
	a, _ := NearestAgent(r, tree, ppid)
	return a == "antigravity"
}

// ProcEnviron reads a process's environment. Tests replace it.
var ProcEnviron = proc.Environ

// withAgentEnv fills in where the session runs from the agent's own
// environment when the hook's has none of it: Muse Code runs hooks with a
// cleaned environment (HOME, PATH, TERM and its own variables), so the
// wrapper id and the multiplexer pane are only in the agent's process. The
// hook's own variables win.
func withAgentEnv(r *rules.Rules, env map[string]string, tree map[int]proc.Node, ppid int) map[string]string {
	if env["CONTINUUM_WRAPPER_ID"] != "" || mux.LocateFromEnv(r, env) != nil {
		return env
	}
	_, pid := NearestAgent(r, tree, ppid)
	if pid <= 0 {
		return env
	}
	agent := ProcEnviron(pid)
	if len(agent) == 0 {
		return env
	}
	out := make(map[string]string, len(agent)+len(env))
	for k, v := range agent {
		out[k] = v
	}
	for k, v := range env {
		out[k] = v
	}
	return out
}

// ProcArgs returns a process's arguments. Tests replace it.
var ProcArgs = proc.Args

// interactive reports whether the agent running the hook was started as an
// interactive session, judged by the same rules that decide what the shell
// hook wraps. Claude Code and Codex say so themselves (entrypoint, rollout
// originator), so they are not checked here; when the agent's process or its
// arguments can't be found, it counts as interactive.
func interactive(r *rules.Rules, harness string, tree map[int]proc.Node, ppid int) bool {
	if hookDriverFor(harness).SelfReportsInteractive() {
		return true
	}
	h := r.Harness(harness)
	if h == nil || h.Wrap == nil {
		return true
	}
	a, pid := NearestAgent(r, tree, ppid)
	if a != harness || pid <= 0 {
		return true
	}
	args := ProcArgs(pid)
	if len(args) < 2 {
		return true
	}
	rest := args[1:]
	// An agent under an interpreter: the script it runs comes first
	// (node …/cursor-agent/…/index.js -p "…").
	for len(rest) > 0 && looksLikeScript(rest[0]) {
		rest = rest[1:]
	}
	return h.Wrap.Wants(rest)
}

func looksLikeScript(a string) bool {
	if !strings.ContainsAny(a, `/\`) {
		return false
	}
	switch strings.ToLower(filepath.Ext(a)) {
	case ".js", ".mjs", ".cjs", ".ts", ".py":
		return true
	}
	return false
}

// ---- install ---------------------------------------------------------------

// Target describes one agent settings file Continuum manages.
type Target struct {
	Harness string
	Path    string
	Events  []string
	Timeout int
	// StatusMessage describes the hook to the agent, which shows it while
	// the hook runs and in its hook lists (Codex shows hooks without one as
	// unnamed). Empty for agents whose hook format has no such field.
	StatusMessage string
	// Quiet: events whose hook is written without the status message: they
	// run on every prompt, where a line flashing by each time is noise.
	Quiet []string
	// Flat: each event lists hook entries directly ({"command": ...}), as
	// Cursor's and Antigravity's hooks.json do, instead of Claude Code's
	// groups of hooks.
	Flat bool
	// RootKey is the top-level JSON key holding the events map (default
	// "hooks"; Antigravity's hooks.json keys entries by hook name, e.g.
	// "continuum").
	RootKey string
	// Init: top-level keys the agent requires in the file, added when
	// missing (Cursor's "version", Muse Code's "schema_version").
	Init []KeyValue
}

func (t Target) rootKey() string {
	if t.RootKey != "" {
		return t.RootKey
	}
	return "hooks"
}

// KeyValue is one top-level setting.
type KeyValue struct {
	Key   string
	Value any
}

// HookStatusMessage names Continuum's hooks in the agents that show one.
const HookStatusMessage = "Continuum: tracks this session to resume it after usage limits"

// Targets lists the settings files Continuum's hooks go in. Grok Build, Muse
// Code and the Cursor CLI also get a prompt-submit hook: it says when a turn
// started, so a turn their Stop hook reports answered shows the limit lifted
// only if it started after the limit hit (see lifted.go). Claude Code gets
// one too: it says at once which process the conversation is being used in,
// when it is open in two (the other is then an out-of-date copy).
func Targets(c core.Config) []Target {
	return []Target{
		{Harness: "claude", Path: filepath.Join(c.ClaudeDir(), "settings.json"), Events: []string{"SessionStart", "UserPromptSubmit", "StopFailure", "Stop", "SessionEnd"}, Timeout: 10, StatusMessage: HookStatusMessage, Quiet: []string{"UserPromptSubmit"}},
		{Harness: "codex", Path: filepath.Join(c.CodexDir(), "hooks.json"), Events: []string{"SessionStart", "Stop"}, Timeout: 10, StatusMessage: HookStatusMessage},
		// Grok Build reads every JSON file in ~/.grok/hooks; this one is
		// Continuum's own, removed whole on uninstall.
		{Harness: "grok", Path: GrokHookFile(c), Events: []string{"SessionStart", "UserPromptSubmit", "StopFailure", "Stop", "SessionEnd"}, Timeout: 10, StatusMessage: HookStatusMessage, Quiet: []string{"UserPromptSubmit"}},
		// Muse Code runs Claude Code-style hooks from its settings.json.
		{Harness: "muse", Path: filepath.Join(c.MuseDir(), "settings.json"), Events: []string{"SessionStart", "UserPromptSubmit", "StopFailure", "Stop", "SessionEnd"}, Timeout: 10, StatusMessage: HookStatusMessage, Quiet: []string{"UserPromptSubmit"},
			Init: []KeyValue{{"schema_version", json.Number("1")}}},
		// Cursor CLI: ~/.cursor/hooks.json, version 1, flat entries (no
		// status message in its format).
		{Harness: "cursor", Path: filepath.Join(c.CursorDir(), "hooks.json"), Events: []string{"sessionStart", "beforeSubmitPrompt", "afterAgentResponse", "stop", "sessionEnd"}, Flat: true,
			Init: []KeyValue{{"version", json.Number("1")}}},
		// Antigravity CLI: ~/.gemini/config/hooks.json, named hook object
		// with flat command entries that must return a JSON object on stdout.
		{Harness: "antigravity", Path: filepath.Join(c.AntigravityConfigDir(), "hooks.json"), RootKey: "continuum", Events: []string{"SessionStart", "PreInvocation", "Stop"}, Timeout: 10, Flat: true},
	}
}

// GrokHookFile is Continuum's hook file for Grok Build.
func GrokHookFile(c core.Config) string { return filepath.Join(c.GrokDir(), "hooks", "continuum.json") }

func quote(path string) string {
	if strings.ContainsAny(path, " \t'\"()&;") {
		return `"` + strings.ReplaceAll(path, `"`, `\"`) + `"`
	}
	return path
}

// Command is the hook command line for a harness. On macOS and Linux it runs
// through /bin/sh with errors silenced and success forced, so that even if
// Continuum is deleted without `continuum uninstall`, the agent never shows a
// hook error: the hook just does nothing.
func Command(bin, harness string) string {
	drv := hookDriverFor(harness)
	if runtime.GOOS == "windows" {
		if fb := drv.WindowsFallback(); fb != "" {
			return quote(bin) + " hook " + harness + " " + fb
		}
		return quote(bin) + " hook " + harness
	}
	inner := `"` + shDoubleQuoteEscape(bin) + `" hook ` + harness + ` 2>/dev/null || ` + drv.ShellFallback()
	return "/bin/sh -c '" + strings.ReplaceAll(inner, "'", `'\''`) + "'"
}

func shDoubleQuoteEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")
	return r.Replace(s)
}

// ourCommand matches every form of Continuum's hook command, old and new:
// "/x/continuum hook claude", "\"C:\\x\\continuum.exe\" hook codex",
// "/bin/sh -c '\"/x/continuum\" hook claude 2>/dev/null || true'".
var ourCommand = regexp.MustCompile(`(?:^|[\s'"/\\])continuum(?:\.exe)?["']?\s+hook\s+[A-Za-z0-9_-]+`)

func isOurs(cmd string) bool { return ourCommand.MatchString(cmd) }

// Install adds Continuum's hooks to t.Path. It is idempotent and keeps every
// other setting and hook untouched. It returns whether the file changed.
func Install(t Target, bin string, dryRun bool) (bool, error) {
	root, original, err := load(t.Path)
	if err != nil {
		return false, err
	}
	rk := t.rootKey()
	hooksV, _ := root.get(rk)
	hooksObj, ok := hooksV.(*object)
	if !ok {
		if hooksV != nil {
			return false, fmt.Errorf("%s: %q is not an object; not touching it", t.Path, rk)
		}
		hooksObj = newObject()
		if len(strings.TrimSpace(string(original))) == 0 {
			// A new file: what the agent requires in one. An existing file is
			// left as it is, so uninstall can give it back byte for byte.
			for _, kv := range t.Init {
				root.set(kv.Key, kv.Value)
			}
		}
		root.set(rk, hooksObj)
	}
	want := Command(bin, t.Harness)
	// Drop Continuum hooks from events this version no longer uses.
	for _, event := range append([]string(nil), hooksObj.keys...) {
		if containsEvent(t.Events, event) {
			continue
		}
		v, _ := hooksObj.get(event)
		if list, ok := v.([]any); ok {
			had := countOurs(list)
			if kept := removeOurs(list); len(kept) != len(list) || had > 0 {
				if len(kept) == 0 {
					hooksObj.del(event)
				} else {
					hooksObj.set(event, kept)
				}
			}
		}
	}
	for _, event := range t.Events {
		groups, _ := hooksObj.get(event)
		list, _ := groups.([]any)
		list = removeOurs(list)
		entry := newObject()
		if t.Flat {
			if t.Timeout > 0 {
				entry.set("type", "command")
			}
			entry.set("command", want)
			if t.Timeout > 0 {
				entry.set("timeout", json.Number(strconv.Itoa(t.Timeout)))
			}
			list = append(list, entry)
			hooksObj.set(event, list)
			continue
		}
		entry.set("type", "command")
		entry.set("command", want)
		entry.set("timeout", json.Number(strconv.Itoa(t.Timeout)))
		if m := t.statusMessage(event); m != "" {
			entry.set("statusMessage", m)
		}
		group := newObject()
		group.set("hooks", []any{entry})
		list = append(list, group)
		hooksObj.set(event, list)
	}
	return write(t.Path, root, original, dryRun)
}

// Uninstall removes Continuum's hooks from t.Path.
func Uninstall(t Target, dryRun bool) (bool, error) {
	if _, err := os.Stat(t.Path); os.IsNotExist(err) {
		return false, nil
	}
	root, original, err := load(t.Path)
	if err != nil {
		return false, err
	}
	rk := t.rootKey()
	hooksV, _ := root.get(rk)
	if _, ok := hooksV.(*object); !ok {
		return false, nil
	}
	stripOurs(root, rk)
	out, err := pretty(root)
	if err != nil {
		return false, err
	}
	// Give the file back exactly as it was: if what is left matches a
	// backup Continuum took before changing it, use that backup's bytes
	// (same formatting, key order and whitespace).
	if b := matchingBackup(t.Path, out, rk); b != nil {
		out = b
	} else if onlyInit(root, t) && onlyOursBackups(t) {
		// Continuum created this file (any backups came only from updates to
		// its own hooks) and nothing else is in it: remove it.
		if dryRun {
			return true, nil
		}
		return true, os.Remove(t.Path)
	}
	if string(out) == string(original) {
		return false, nil
	}
	if dryRun {
		return true, nil
	}
	perm := os.FileMode(0o600)
	if st, err := os.Stat(t.Path); err == nil {
		perm = st.Mode().Perm()
	}
	return true, core.WriteFileAtomic(t.Path, out, perm)
}

// matchingBackup returns the bytes of the most recent backup whose content
// equals want (as JSON), or nil.
func matchingBackup(path string, want []byte, rootKey string) []byte {
	var w any
	d := json.NewDecoder(strings.NewReader(string(want)))
	d.UseNumber()
	if d.Decode(&w) != nil {
		return nil
	}
	backups := Backups(path)
	for i := len(backups) - 1; i >= 0; i-- {
		data, err := os.ReadFile(backups[i])
		if err != nil {
			continue
		}
		var b any
		bd := json.NewDecoder(strings.NewReader(string(data)))
		bd.UseNumber()
		// An empty "hooks" the file had is dropped with Continuum's: the
		// same file.
		if bd.Decode(&b) == nil && reflect.DeepEqual(withoutEmptyHooks(w, rootKey), withoutEmptyHooks(b, rootKey)) {
			return data
		}
	}
	return nil
}

func withoutEmptyHooks(v any, rootKey string) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	if h, ok := m[rootKey].(map[string]any); ok && len(h) == 0 {
		c := make(map[string]any, len(m))
		for k, x := range m {
			if k != rootKey {
				c[k] = x
			}
		}
		return c
	}
	return v
}

func containsEvent(list []string, e string) bool {
	for _, x := range list {
		if x == e {
			return true
		}
	}
	return false
}

func countOurs(list []any) int {
	n := 0
	for _, g := range list {
		for _, hk := range hooksOf(g) {
			if c, _ := hk.get("command"); c != nil {
				if s, _ := c.(string); isOurs(s) {
					n++
				}
			}
		}
	}
	return n
}

// Hook states.
const (
	StateCurrent  = "current"  // exactly the hooks this version installs
	StateOutdated = "outdated" // ours, but an older form or another binary path
	StatePartial  = "partial"  // some events missing
	StateMissing  = "missing"  // none
)

// Status compares t.Path with what Install(t, bin) would write.
func Status(t Target, bin string) (string, string) {
	root, _, err := load(t.Path)
	if err != nil {
		return StateMissing, err.Error()
	}
	hooksObj, _ := func() (*object, bool) { v, _ := root.get(t.rootKey()); o, ok := v.(*object); return o, ok }()
	if hooksObj == nil {
		return StateMissing, "no hooks"
	}
	want := Command(bin, t.Harness)
	found, exact, stray := 0, 0, 0
	for _, event := range hooksObj.keys {
		v, _ := hooksObj.get(event)
		n := 0
		for _, g := range asList(v) {
			for _, hk := range hooksOf(g) {
				c, _ := hk.get("command")
				s, _ := c.(string)
				if !isOurs(s) {
					continue
				}
				n++
				if !containsEvent(t.Events, event) {
					stray++
				} else if s == want && hasStatusMessage(hk, t, event) {
					exact++
				}
			}
		}
		if containsEvent(t.Events, event) && n > 0 {
			found++
		}
	}
	switch {
	case found == 0 && stray == 0:
		return StateMissing, "no Continuum hooks"
	case exact == len(t.Events) && found == len(t.Events) && stray == 0:
		return StateCurrent, want
	case found < len(t.Events) && exact == found && stray == 0:
		return StatePartial, fmt.Sprintf("%d of %d events", found, len(t.Events))
	}
	return StateOutdated, "from an older Continuum or another location"
}

// hasStatusMessage: hk carries the expected metadata Install writes for t, so
// installs from before Continuum named its hooks (or updated flat handler
// fields) count as outdated.
func hasStatusMessage(hk *object, t Target, event string) bool {
	if t.Flat {
		if t.Timeout > 0 {
			tv, _ := hk.get("type")
			ts, _ := tv.(string)
			tmo, _ := hk.get("timeout")
			if ts != "command" || fmt.Sprint(tmo) != strconv.Itoa(t.Timeout) {
				return false
			}
		}
		return true
	}
	v, _ := hk.get("statusMessage")
	m, _ := v.(string)
	return m == t.statusMessage(event)
}

// statusMessage is the message written for event's hook ("" for none).
func (t Target) statusMessage(event string) string {
	if containsEvent(t.Quiet, event) {
		return ""
	}
	return t.StatusMessage
}

// Backups lists the backups Continuum saved of t.Path before changing it.
func Backups(path string) []string {
	m, _ := filepath.Glob(path + ".continuum-backup-*")
	return m
}

// Installed reports whether t.Path has Continuum hooks for every event.
func Installed(t Target) (bool, string) {
	root, _, err := load(t.Path)
	if err != nil {
		return false, err.Error()
	}
	hooksV, _ := root.get(t.rootKey())
	hooksObj, ok := hooksV.(*object)
	if !ok {
		return false, "no hooks"
	}
	var missing []string
	cmd := ""
	for _, event := range t.Events {
		v, _ := hooksObj.get(event)
		found := false
		for _, g := range asList(v) {
			for _, hk := range hooksOf(g) {
				if c, _ := hk.get("command"); c != nil {
					if s, _ := c.(string); isOurs(s) {
						found, cmd = true, s
					}
				}
			}
		}
		if !found {
			missing = append(missing, event)
		}
	}
	if len(missing) > 0 {
		return false, "missing " + strings.Join(missing, ", ")
	}
	return true, cmd
}

func asList(v any) []any { l, _ := v.([]any); return l }

func hooksOf(g any) []*object {
	o, ok := g.(*object)
	if !ok {
		return nil
	}
	if _, flat := o.get("command"); flat {
		return []*object{o} // a flat entry (Cursor)
	}
	v, _ := o.get("hooks")
	var out []*object
	for _, x := range asList(v) {
		if h, ok := x.(*object); ok {
			out = append(out, h)
		}
	}
	return out
}

func stripOurs(root *object, rootKey string) int {
	hooksV, _ := root.get(rootKey)
	hooksObj, ok := hooksV.(*object)
	if !ok {
		return 0
	}
	removed := 0
	for _, event := range append([]string(nil), hooksObj.keys...) {
		v, _ := hooksObj.get(event)
		list, ok := v.([]any)
		if !ok {
			continue
		}
		removed += countOurs(list)
		list = removeOurs(list)
		if len(list) == 0 {
			hooksObj.del(event)
		} else {
			hooksObj.set(event, list)
		}
	}
	if len(hooksObj.keys) == 0 {
		root.del(rootKey)
	}
	return removed
}

// onlyInit: nothing is left in root but the keys install adds to a new file.
func onlyInit(root *object, t Target) bool {
	for _, k := range root.keys {
		found := false
		for _, kv := range t.Init {
			v, _ := root.get(k)
			if kv.Key == k && fmt.Sprint(v) == fmt.Sprint(kv.Value) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func onlyOursBackups(t Target) bool {
	rk := t.rootKey()
	for _, b := range Backups(t.Path) {
		root, _, err := load(b)
		if err != nil {
			return false
		}
		if stripOurs(root, rk) == 0 || !onlyInit(root, t) {
			return false
		}
	}
	return true
}

// removeOurs drops Continuum's hook entries, and groups left empty by that.
func removeOurs(list []any) []any {
	var out []any
	for _, g := range list {
		o, ok := g.(*object)
		if !ok {
			out = append(out, g)
			continue
		}
		if c, flat := o.get("command"); flat {
			if s, _ := c.(string); isOurs(s) {
				continue // a flat entry (Cursor)
			}
			out = append(out, g)
			continue
		}
		v, _ := o.get("hooks")
		hl, ok := v.([]any)
		if !ok {
			out = append(out, g)
			continue
		}
		var kept []any
		for _, x := range hl {
			if h, ok := x.(*object); ok {
				if c, _ := h.get("command"); c != nil {
					if s, _ := c.(string); isOurs(s) {
						continue
					}
				}
			}
			kept = append(kept, x)
		}
		if len(kept) == 0 {
			continue
		}
		o.set("hooks", kept)
		out = append(out, o)
	}
	return out
}

func load(path string) (*object, []byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return newObject(), nil, nil
		}
		return nil, nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return newObject(), data, nil
	}
	v, err := parseOrdered(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s is not valid JSON (%v); not touching it", path, err)
	}
	o, ok := v.(*object)
	if !ok {
		return nil, nil, errors.New(path + " is not a JSON object; not touching it")
	}
	return o, data, nil
}

func write(path string, root *object, original []byte, dryRun bool) (bool, error) {
	out, err := pretty(root)
	if err != nil {
		return false, err
	}
	if string(out) == string(original) {
		return false, nil
	}
	if dryRun {
		return true, nil
	}
	if original != nil {
		if err := SaveBackup(path, original); err != nil {
			return false, err
		}
	}
	perm := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		perm = st.Mode().Perm()
	}
	return true, core.WriteFileAtomic(path, out, perm)
}

// saveBackup writes a new backup of path. It never overwrites an existing
// backup (two changes in the same second get -2, -3, ...), so the copy of the
// user's original file is never lost.
func SaveBackup(path string, data []byte) error {
	// One copy of each version is enough (the daemon re-adds hooks a
	// dotfiles tool keeps removing; that must not pile up backups).
	if list, _ := filepath.Glob(path + ".continuum-backup-*"); len(list) > 0 {
		for _, b := range list {
			if old, err := os.ReadFile(b); err == nil && string(old) == string(data) {
				return nil
			}
		}
	}
	base := path + ".continuum-backup-" + time.Now().Format("20060102-150405")
	name := base
	for i := 2; i < 1000; i++ {
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, werr := f.Write(data)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				return fmt.Errorf("backup %s: %w", name, werr)
			}
			return nil
		}
		if !os.IsExist(err) {
			return fmt.Errorf("backup %s: %w", name, err)
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return fmt.Errorf("backup %s: too many backups this second", base)
}

// debugHook appends one line to hooks.log when debug logging is on. Hooks
// must stay silent and fast, so this never prints, and the file is capped.
func debugHook(p core.Paths, harness, msg string) {
	cfg, _ := core.LoadConfig(p.Config)
	if !cfg.Debug() {
		return
	}
	path := filepath.Join(p.Home, "hooks.log")
	if st, err := os.Stat(path); err == nil && st.Size() > 2<<20 {
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s %s\n", time.Now().Format("2006-01-02 15:04:05.000"), harness, msg)
}
