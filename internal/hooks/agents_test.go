package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/proc"
)

// Muse Code's hooks carry Claude Code's fields (captured from Muse Code
// 1.4.24 with the echo provider).
func TestMuseHookPayloads(t *testing.T) {
	r := loadRules(t)
	var in Input
	start := `{"hook_event_name":"SessionStart","source":"startup","session_id":"01a1f3bb-d703-7090-9b3c-b82eb6f8b2d2","cwd":"/w/app","transcript_path":null,"model":"unknown","permission_mode":"default"}`
	if err := json.Unmarshal([]byte(start), &in); err != nil {
		t.Fatal(err)
	}
	ev, ok := BuildEvent(r, "muse", in, map[string]string{"TMUX": "/tmp/tmux-501/default,1,0", "TMUX_PANE": "%3"}, time.Now())
	if !ok || ev.Kind != core.KindSessionStart || ev.SessionID != "01a1f3bb-d703-7090-9b3c-b82eb6f8b2d2" || ev.Cwd != "/w/app" || ev.Location == nil || ev.Location.Target != "%3" {
		t.Fatalf("session start: ok=%v %+v", ok, ev)
	}

	now := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	fail := Input{HookEventName: "StopFailure", SessionID: "m1", Error: []byte(`"rate_limit"`), ErrorDetails: "Your weekly quota is exhausted. Resets at 3:00 PM (UTC)."}
	ev, ok = BuildEvent(r, "muse", fail, nil, now)
	if !ok || ev.Kind != core.KindUsageLimit || !ev.ResetAt.Equal(time.Date(2026, 11, 20, 15, 0, 0, 0, time.UTC)) {
		t.Fatalf("quota: ok=%v %+v", ok, ev)
	}
	for code, want := range map[string]string{"tier_limit": core.KindUsageLimit, "overloaded": core.KindTransient} {
		ev, ok = BuildEvent(r, "muse", Input{HookEventName: "StopFailure", SessionID: "m1", Error: []byte(`"` + code + `"`)}, nil, now)
		if !ok || ev.Kind != want {
			t.Errorf("%s: ok=%v kind=%s", code, ok, ev.Kind)
		}
	}
	ev, ok = BuildEvent(r, "muse", Input{HookEventName: "StopFailure", SessionID: "m1", Error: []byte(`"unauthorized"`), ErrorDetails: "not logged in: run `muse login` first"}, nil, now)
	if !ok || ev.Kind != core.KindNeverResume {
		t.Errorf("login error: ok=%v kind=%s", ok, ev.Kind)
	}
}

// Cursor's hooks name the session conversation_id and the folder
// workspace_roots; a stop with status "error" doesn't say which error.
func TestCursorHookPayloads(t *testing.T) {
	r := loadRules(t)
	var in Input
	raw := `{"conversation_id":"c-42","generation_id":"g","model":"auto","hook_event_name":"sessionStart","cursor_version":"2026.11.17","workspace_roots":["/w/site"],"user_email":"x@y","transcript_path":null}`
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatal(err)
	}
	ev, ok := BuildEvent(r, "cursor", in, nil, time.Now())
	if !ok || ev.Kind != core.KindSessionStart || ev.SessionID != "c-42" || ev.Cwd != "/w/site" {
		t.Fatalf("session start: ok=%v %+v", ok, ev)
	}
	if ev, ok := BuildEvent(r, "cursor", Input{HookEventName: "stop", ConversationID: "c-42", Status: "error"}, nil, time.Now()); ok {
		t.Fatalf("an error stop was reported: %+v", ev)
	}
	if ev, ok := BuildEvent(r, "cursor", Input{HookEventName: "stop", ConversationID: "c-42", Status: "completed"}, nil, time.Now()); !ok || ev.Kind != core.KindActivity {
		t.Fatalf("completed stop: ok=%v %+v", ok, ev)
	}
	reply := Input{HookEventName: "afterAgentResponse", ConversationID: "c-42", Text: "If you hit a usage limit, the API says: You've hit your usage limit."}
	if ev, ok := BuildEvent(r, "cursor", reply, nil, time.Now()); !ok || ev.Kind != core.KindReply {
		t.Fatalf("reply mentioning a limit: ok=%v %+v", ok, ev)
	}
}

func TestCursorFlatHooksFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hooks.json")
	tg := Target{Harness: "cursor", Path: path, Events: []string{"sessionStart", "stop"}, Flat: true, Init: []KeyValue{{"version", json.Number("1")}}}
	if _, err := Install(tg, "/a/continuum", false); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Version int                            `json:"version"`
		Hooks   map[string][]map[string]string `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(mustRead(t, path)), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != 1 || len(got.Hooks["stop"]) != 1 || !strings.Contains(got.Hooks["stop"][0]["command"], "hook cursor") {
		t.Fatalf("hooks.json: %s", mustRead(t, path))
	}
	if st, _ := Status(tg, "/a/continuum"); st != StateCurrent {
		t.Fatalf("status %s", st)
	}
	if ok, _ := Installed(tg); !ok {
		t.Fatal("not seen as installed")
	}
	if _, err := Install(tg, "/b/continuum", false); err != nil { // update: replaced, not added
		t.Fatal(err)
	}
	if n := strings.Count(mustRead(t, path), "hook cursor"); n != 2 {
		t.Fatalf("%d entries after update:\n%s", n, mustRead(t, path))
	}
	if _, err := Uninstall(tg, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("hooks.json Continuum created is still there: %v\n%s", err, mustRead(t, path))
	}

	// The user's own hooks stay, and the file comes back byte for byte.
	orig := "{\n  \"version\": 1,\n  \"hooks\": {\n    \"stop\": [ { \"command\": \"./audit.sh\" } ]\n  }\n}\n"
	os.WriteFile(path, []byte(orig), 0o600)
	if _, err := Install(tg, "/a/continuum", false); err != nil {
		t.Fatal(err)
	}
	if s := mustRead(t, path); !strings.Contains(s, "./audit.sh") || strings.Count(s, "hook cursor") != 2 {
		t.Fatalf("after install:\n%s", s)
	}
	if _, err := Uninstall(tg, false); err != nil {
		t.Fatal(err)
	}
	if s := mustRead(t, path); s != orig {
		t.Fatalf("not restored:\n%s", s)
	}
}

func TestMuseSettingsKeepTheirSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	tg := Target{Harness: "muse", Path: path, Events: []string{"SessionStart", "Stop"}, Timeout: 10, Init: []KeyValue{{"schema_version", json.Number("1")}}}
	// A new file gets the schema version Muse Code requires, and goes away
	// again on uninstall.
	if _, err := Install(tg, "/a/continuum", false); err != nil {
		t.Fatal(err)
	}
	if s := mustRead(t, path); !strings.Contains(s, `"schema_version": 1`) || !strings.Contains(s, "hook muse") {
		t.Fatalf("new settings:\n%s", s)
	}
	if _, err := Uninstall(tg, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("settings file Continuum created is still there")
	}
	orig := "{\n  \"schema_version\": 1,\n  \"provider\": \"meta\",\n  \"tui\": {\n    \"foreign_context_notice_shown\": true\n  }\n}"
	os.WriteFile(path, []byte(orig), 0o600)
	if _, err := Install(tg, "/a/continuum", false); err != nil {
		t.Fatal(err)
	}
	if _, err := Uninstall(tg, false); err != nil {
		t.Fatal(err)
	}
	if s := mustRead(t, path); s != orig {
		t.Fatalf("not restored:\n%s", s)
	}
}

// Print mode and headless subcommands run hooks too; nobody types into them.
func TestHeadlessRunsAreNotInteractive(t *testing.T) {
	r := loadRules(t)
	defer func(f func(int) []string) { ProcArgs = f }(ProcArgs)
	tree := map[int]proc.Node{
		100: {PPID: 200, Name: "sh"},
		200: {PPID: 300, Name: "muse-bin-1.4.24-R9131.1"},
		300: {PPID: 1, Name: "zsh"},
	}
	cases := []struct {
		harness, name string
		args          []string
		want          bool
	}{
		{"muse", "muse-bin-1.4.24-R9131.1", []string{"/u/.local/bin/muse-bin-1.4.24-R9131.1", "exec", "fix the tests"}, false},
		{"muse", "muse-bin-1.4.24-R9131.1", []string{"/u/.local/bin/muse-bin-1.4.24-R9131.1", "--provider", "meta"}, true},
		{"muse", "muse-bin-1.4.24-R9131.1", []string{"/u/.local/bin/muse-bin-1.4.24-R9131.1", "resume", "--last"}, true},
		{"grok", "grok", []string{"grok", "-p", "summarise"}, false},
		{"grok", "grok", []string{"grok"}, true},
		{"muse", "muse-bin-1.4.24-R9131.1", nil, true}, // unknown: interactive
	}
	for _, c := range cases {
		n := tree[200]
		n.Name = c.name
		tree[200] = n
		args := c.args
		ProcArgs = func(pid int) []string {
			if pid != 200 {
				t.Fatalf("args of %d asked for", pid)
			}
			return args
		}
		if got := interactive(r, c.harness, tree, 100); got != c.want {
			t.Errorf("%s %v: interactive=%v want %v", c.harness, c.args, got, c.want)
		}
	}
	// Claude Code and Codex are never judged by their command line.
	ProcArgs = func(int) []string { t.Fatal("claude's args read"); return nil }
	tree[200] = proc.Node{PPID: 300, Name: "claude"}
	if !interactive(r, "claude", tree, 100) {
		t.Error("claude judged by its arguments")
	}
}

// Cursor and Grok Build run Claude Code's hooks too; with Continuum's own
// hook for them installed, those borrowed runs must not report again.
func TestHasOwnHook(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows
	t.Setenv("CONTINUUM_HOME", filepath.Join(home, ".continuum"))
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("GROK_HOME", filepath.Join(home, ".grok"))
	if hasOwnHook("cursor") || hasOwnHook("grok") {
		t.Fatal("no hook installed yet")
	}
	cfg, _ := core.LoadConfig(core.GetPaths().Config)
	for _, tg := range Targets(cfg) {
		if tg.Harness == "cursor" || tg.Harness == "grok" {
			if _, err := Install(tg, "/a/continuum", false); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !hasOwnHook("cursor") {
		t.Fatal("Continuum's Cursor hook not found")
	}
	// Grok Build runs Claude Code's hooks and its own: before this was
	// parsed, the escaped quotes in the file hid the hook and Grok's
	// events were reported twice.
	if !hasOwnHook("grok") {
		t.Fatal("Continuum's Grok hook not found")
	}
	if hasOwnHook("muse") {
		t.Fatal("Muse hook reported without one")
	}
}

// Muse Code runs hooks with a cleaned environment: where the session runs
// is read from the agent's own process.
func TestLocationFromTheAgentsEnvironment(t *testing.T) {
	r := loadRules(t)
	defer func(f func(int) map[string]string) { ProcEnviron = f }(ProcEnviron)
	tree := map[int]proc.Node{
		100: {PPID: 200, Name: "sh"},
		200: {PPID: 300, Name: "muse-bin-1.4.24-R9131.1"},
		300: {PPID: 1, Name: "zsh"},
	}
	ProcEnviron = func(pid int) map[string]string {
		if pid != 200 {
			return nil
		}
		return map[string]string{"TMUX": "/tmp/tmux-501/default,1,0", "TMUX_PANE": "%5", "MUSE_SESSION_ID": "old"}
	}
	hookEnv := map[string]string{"HOME": "/u", "MUSE_SESSION_ID": "m1"}
	env := withAgentEnv(r, hookEnv, tree, 100)
	if env["TMUX_PANE"] != "%5" || env["MUSE_SESSION_ID"] != "m1" {
		t.Fatalf("env %v", env)
	}
	ev, ok := BuildEvent(r, "muse", Input{HookEventName: "SessionStart", SessionID: "m1"}, env, time.Now())
	if !ok || ev.Location == nil || ev.Location.Kind != "tmux" || ev.Location.Target != "%5" {
		t.Fatalf("location %+v", ev.Location)
	}
	// A hook that already says where it runs is left as it is.
	own := map[string]string{"CONTINUUM_WRAPPER_ID": "w1"}
	if got := withAgentEnv(r, own, tree, 100); got["TMUX_PANE"] != "" {
		t.Fatalf("agent env mixed into a complete one: %v", got)
	}
}

// Install adds only hooks to an existing file, and uninstall gives it back
// byte for byte, an empty "hooks" included.
func TestExistingFilesComeBackExactly(t *testing.T) {
	cases := []struct {
		tg   Target
		orig string
	}{
		{Target{Harness: "muse", Events: []string{"SessionStart", "Stop"}, Timeout: 10, Init: []KeyValue{{"schema_version", json.Number("1")}}}, "{\"provider\":\"meta\"}"},
		{Target{Harness: "muse", Events: []string{"SessionStart", "Stop"}, Timeout: 10, Init: []KeyValue{{"schema_version", json.Number("1")}}}, "{}"},
		{Target{Harness: "cursor", Events: []string{"stop"}, Flat: true, Init: []KeyValue{{"version", json.Number("1")}}}, "{\"version\":1,\"hooks\":{}}"},
	}
	for _, c := range cases {
		path := filepath.Join(t.TempDir(), "f.json")
		os.WriteFile(path, []byte(c.orig), 0o600)
		c.tg.Path = path
		if _, err := Install(c.tg, "/a/continuum", false); err != nil {
			t.Fatal(err)
		}
		if s := mustRead(t, path); strings.Contains(s, "schema_version") != strings.Contains(c.orig, "schema_version") {
			t.Errorf("%s: install added a setting:\n%s", c.orig, s)
		}
		if _, err := Uninstall(c.tg, false); err != nil {
			t.Fatal(err)
		}
		if s := mustRead(t, path); s != c.orig {
			t.Errorf("not restored: %q want %q", s, c.orig)
		}
	}
}

// The Cursor CLI runs under node, started as `agent` (`exec -a "$0" node
// …`): macOS shows it as `agent`, Linux as `node`. It is recognised by its
// command line.
func TestCursorCLIByCommandLine(t *testing.T) {
	r := loadRules(t)
	defer func(f func(int) []string) { ProcArgs = f }(ProcArgs)
	script := "/Users/u/.local/share/cursor-agent/versions/2026.11.17-f532c45/index.js"
	for _, name := range []string{"agent", "node"} {
		args := map[int][]string{200: {name, script, "-p", "summarise this"}}
		ProcArgs = func(pid int) []string { return args[pid] }
		tree := map[int]proc.Node{
			100: {PPID: 200, Name: "sh"},
			200: {PPID: 300, Name: name},
			300: {PPID: 1, Name: "zsh"},
		}
		if a, pid := NearestAgent(r, tree, 100); a != "cursor" || pid != 200 {
			t.Fatalf("%s: agent %q pid %d", name, a, pid)
		}
		if interactive(r, "cursor", tree, 100) {
			t.Errorf("%s: `agent -p` taken for an interactive session", name)
		}
		args[200] = []string{name, script, "fix the help page"}
		if !interactive(r, "cursor", tree, 100) {
			t.Errorf("%s: interactive session with a prompt dropped", name)
		}
		// Started from Claude Code's shell tool: nested.
		tree[300] = proc.Node{PPID: 400, Name: "zsh"}
		tree[400] = proc.Node{PPID: 1, Name: "claude"}
		if !nested(r, tree, 100) {
			t.Errorf("%s: cursor under claude not nested", name)
		}
	}
	// Another program named node or agent is nobody's.
	ProcArgs = func(int) []string { return []string{"node", "/srv/app/server.js"} }
	tree := map[int]proc.Node{100: {PPID: 200, Name: "sh"}, 200: {PPID: 1, Name: "node"}}
	if a, _ := NearestAgent(r, tree, 100); a != "" {
		t.Errorf("a node server taken for %s", a)
	}
	// The Cursor editor runs the same hooks file: no Cursor CLI above it.
	tree = map[int]proc.Node{100: {PPID: 200, Name: "sh"}, 200: {PPID: 300, Name: "Cursor Helper (Plugin)"}, 300: {PPID: 1, Name: "Cursor"}}
	if a, _ := NearestAgent(r, tree, 100); a != "" {
		t.Errorf("the editor taken for %s", a)
	}
	if !r.Harness("cursor").HookRequiresCLI || !r.Harness("muse").HookRequiresCLI || r.Harness("claude").HookRequiresCLI {
		t.Error("hook_requires_cli rules")
	}
}

// A prompt that starts with a subcommand's name is still a prompt: exact
// arguments, not words.
func TestPromptStartingLikeASubcommand(t *testing.T) {
	r := loadRules(t)
	defer func(f func(int) []string) { ProcArgs = f }(ProcArgs)
	ProcArgs = func(int) []string { return []string{"grok", "help me fix the failing tests"} }
	tree := map[int]proc.Node{100: {PPID: 200, Name: "sh"}, 200: {PPID: 1, Name: "grok"}}
	if !interactive(r, "grok", tree, 100) {
		t.Fatal("grok \"help me …\" dropped")
	}
}
