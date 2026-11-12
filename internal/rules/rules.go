// Package rules holds the data-driven knowledge Continuum has about agents and
// multiplexers. Message wording, CLI flags and multiplexer commands live in
// default_rules.json so an agent update can be absorbed by editing data, not code.
package rules

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
)

//go:embed default_rules.json
var defaultJSON []byte

// DefaultJSON returns the embedded rules document.
func DefaultJSON() []byte { return append([]byte(nil), defaultJSON...) }

type Defaults struct {
	ResumePrompt               string   `json:"resume_prompt"`
	DelayAfterResetSeconds     int      `json:"delay_after_reset_seconds"`
	NativeGraceSeconds         int      `json:"native_grace_seconds"`
	TransientBackoffSeconds    []int    `json:"transient_backoff_seconds"`
	UnknownResetBackoffSeconds []int    `json:"unknown_reset_backoff_seconds"`
	MaxAttempts                int      `json:"max_attempts"`
	EnterDelayMS               int      `json:"enter_delay_ms"`
	TailLines                  int      `json:"tail_lines"`
	BlockingPatterns           []string `json:"blocking_patterns"`
	// Shared knowledge every harness with use_common_patterns inherits, so
	// a new agent works with a few lines of rules.
	CommonPatterns            []Pattern `json:"common_patterns"`
	CommonNeverResumePatterns []string  `json:"common_never_resume_patterns"`
	// Interpreters an agent may run under (tmux then reports "node", not
	// "gemini"): such panes are identified by their command line.
	Interpreters []string `json:"interpreters"`
}

type Pattern struct {
	Kind  string `json:"kind"`
	Regex string `json:"regex"`
	re    *regexp.Regexp
}

type NativeCommand struct {
	Name                   string              `json:"name"`
	RequiresHelp           []string            `json:"requires_help"`
	RequiresSubcommandHelp map[string][]string `json:"requires_subcommand_help"`
	Args                   []string            `json:"args"`
	Cwd                    string              `json:"cwd"`
	Detach                 bool                `json:"detach"`
	// When says which sessions the command may be used for:
	//   "stopped" (default): only when the original session is no longer open,
	//       so running the command can never start a second copy of it;
	//   "background": only for a session that runs in the agent's own
	//       background service (the command reaches that same session);
	//   "any": always (only for commands that address the open session itself).
	When string `json:"when"`
	// PTY runs the command in a terminal Continuum owns and types the prompt
	// into it, for commands that open the session interactively (attach).
	PTY bool `json:"pty"`
	// ReachesDesktop: the command delivers through the desktop app's own
	// backend (e.g. codex queue → the local Codex app server), so it reaches
	// a session the desktop app has open instead of starting a copy.
	ReachesDesktop bool `json:"reaches_desktop"`
}

// DesktopApp describes an agent's desktop app, which can hold a session open
// without a terminal Continuum can type into.
type DesktopApp struct {
	Name string `json:"name"`
	// Entrypoints name sessions started from the app (transcript
	// "entrypoint", hook CLAUDE_CODE_ENTRYPOINT, rollout "originator").
	Entrypoints []string `json:"entrypoints"`
	// ProcessPatterns match the app's running process (full command line).
	ProcessPatterns []string `json:"process_patterns"`
	// VersionDir holds one folder per bundled agent version; a new folder
	// means the app updated, and what was learned about it is re-checked.
	VersionDir string `json:"version_dir"`
	// GuardUnknown: treat sessions of unknown origin as possibly open in the
	// app while it runs (safe default if an update drops the entrypoint).
	GuardUnknown bool `json:"guard_unknown"`

	procRes []*regexp.Regexp
}

// IsEntrypoint reports whether ep names a session started from the app.
func (a *DesktopApp) IsEntrypoint(ep string) bool {
	for _, e := range a.Entrypoints {
		if strings.EqualFold(e, ep) {
			return true
		}
	}
	return false
}

// MatchesProcess reports whether a command line is the app.
func (a *DesktopApp) MatchesProcess(line string) bool { return anyMatch(a.procRes, line) }

// UsedWhen returns the command's When, defaulting to "stopped".
func (n NativeCommand) UsedWhen() string {
	if n.When == "" {
		return "stopped"
	}
	return n.When
}

// LiveSessions says how to find out which sessions of an agent are open right
// now, so Continuum never starts a copy of a session that is still running.
type LiveSessions struct {
	// Command prints a JSON array of open sessions (e.g. claude agents --json).
	Command      []string `json:"command"`
	RequiresHelp []string `json:"requires_help"`
	// RegistryDir holds one JSON file per running session, keyed by pid.
	RegistryDir string `json:"registry_dir"`
	// RegistryFile is one JSON array of the open sessions (session id, pid,
	// cwd), as Grok Build keeps in ~/.grok/active_sessions.json.
	RegistryFile string `json:"registry_file"`
	// Process scan: processes named like the agent, minus helper processes.
	ScanProcesses      bool     `json:"scan_processes"`
	ProcessExcludeArgs []string `json:"process_exclude_args"`
	BusyStates         []string `json:"busy_states"`
	BackgroundKinds    []string `json:"background_kinds"`
}

// Wrap decides which invocations `continuum run --auto` wraps. Everything
// else (print mode, version, subcommands that are not the interactive agent)
// is run directly, untouched.
type Wrap struct {
	PassthroughFlags       []string `json:"passthrough_flags"`
	PassthroughSubcommands []string `json:"passthrough_subcommands"`
	// OnlySubcommands, for CLIs built around subcommands: wrap the bare
	// command and these subcommands only.
	OnlySubcommands []string `json:"only_subcommands"`
}

// Wants reports whether args (without the program name) start an interactive
// session worth wrapping.
func (w *Wrap) Wants(args []string) bool {
	if w == nil {
		return true
	}
	positional := ""
	for _, a := range args {
		if a == "--" {
			break
		}
		for _, f := range w.PassthroughFlags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return false
			}
		}
		if positional == "" && !strings.HasPrefix(a, "-") {
			positional = a
		}
	}
	for _, s := range w.PassthroughSubcommands {
		if positional == s {
			return false
		}
	}
	if len(w.OnlySubcommands) > 0 && positional != "" {
		for _, s := range w.OnlySubcommands {
			if positional == s {
				return true
			}
		}
		return false
	}
	return true
}

type Harness struct {
	Name                 string            `json:"-"`
	DisplayName          string            `json:"display_name"`
	Binary               string            `json:"binary"`
	ProcessNames         []string          `json:"process_names"`
	ProcessNamePatterns  []string          `json:"process_name_patterns"`
	ResumePrompt         string            `json:"resume_prompt"`
	NativeGraceSeconds   *int              `json:"native_grace_seconds"`
	StopFailureKinds     map[string]string `json:"stop_failure_kinds"`
	Patterns             []Pattern         `json:"patterns"`
	NativeWaitPatterns   []string          `json:"native_wait_patterns"`
	NativeGaveUpPatterns []string          `json:"native_gave_up_patterns"`
	NeverResumePatterns  []string          `json:"never_resume_patterns"`
	UIMarkers            []string          `json:"ui_markers"`
	NativeCommands       []NativeCommand   `json:"native_commands"`
	LiveSessions         *LiveSessions     `json:"live_sessions"`
	Wrap                 *Wrap             `json:"wrap"`
	// ProcessArgPatterns match the full command line, for agents that run
	// under an interpreter (node .../@google/gemini-cli/...).
	ProcessArgPatterns []string `json:"process_arg_patterns"`
	UseCommonPatterns  bool     `json:"use_common_patterns"`
	// ShellHook: wrap this agent's command in the shell hook. Off for agents
	// whose command name other tools also use (amp, goose, pi, ...); turn
	// those on with config "shell_hook_agents".
	ShellHook *bool  `json:"shell_hook"`
	Homepage  string `json:"homepage"`
	// DesktopApps that can hold this agent's sessions.
	DesktopApps []*DesktopApp `json:"desktop_apps"`
	// IgnoreEntrypoints match entrypoints of runs nobody types into (print
	// mode, SDK scripts): Continuum never schedules a resume for them.
	IgnoreEntrypoints []string `json:"ignore_entrypoints"`
	// HookEnv: environment variables this agent sets when it runs a hook. A
	// hook installed for another agent (which this one also runs) is then
	// recognised as this agent's.
	HookEnv []string `json:"hook_env"`

	nativeWait, nativeGaveUp, neverResume, uiMarkers, processRes, argRes, ignoreEntry []*regexp.Regexp
	commonMerged                                                                      bool
}

// InShellHook reports whether the shell hook wraps this agent by default.
func (h *Harness) InShellHook() bool {
	if h == nil || h.Binary == "" || h.Wrap == nil {
		return false
	}
	return h.ShellHook == nil || *h.ShellHook
}

// MatchesCommandLine reports whether a full command line runs this agent.
func (h *Harness) MatchesCommandLine(line string) bool {
	if h == nil {
		return false
	}
	return anyMatch(h.argRes, line)
}

// IsInterpreter reports whether a process name is a language runtime an
// agent may run under.
func (r *Rules) IsInterpreter(name string) bool {
	base := strings.ToLower(name)
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, ".exe")
	for _, x := range r.Defaults.Interpreters {
		if base == x || strings.HasPrefix(base, x) && strings.Trim(base[len(x):], "0123456789.") == "" {
			return true
		}
	}
	return false
}

// Arg is either a literal/template string or an optional group that is kept
// only when every placeholder inside it has a value.
type Arg struct {
	Text  string
	Group []string
}

func (a *Arg) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '[' {
		return json.Unmarshal(b, &a.Group)
	}
	return json.Unmarshal(b, &a.Text)
}

func (a Arg) MarshalJSON() ([]byte, error) {
	if a.Group != nil {
		return json.Marshal(a.Group)
	}
	return json.Marshal(a.Text)
}

type Multiplexer struct {
	Name            string   `json:"-"`
	Priority        int      `json:"priority"`
	EnvTarget       string   `json:"env_target"`
	EnvSocket       string   `json:"env_socket"`
	RequireEnv      []string `json:"require_env"`
	SocketTransform string   `json:"socket_transform"`
	TargetFormat    string   `json:"target_format"`
	SendText        []Arg    `json:"send_text"`
	SendTextStdin   string   `json:"send_text_stdin"`
	SendEnter       []Arg    `json:"send_enter"`
	SendEnterStdin  string   `json:"send_enter_stdin"`
	Capture         []Arg    `json:"capture"`
	CaptureFrom     string   `json:"capture_from"`
	Process         []Arg    `json:"process"`
	List            []Arg    `json:"list"`
}

// TerminalApp is a terminal emulator with a scripting interface that can type
// into a specific terminal by its tty, without focusing it.
type TerminalApp struct {
	Name            string   `json:"name"`
	ProcessPatterns []string `json:"process_patterns"`
	Find            []string `json:"find"`       // {tty} -> lines "id<TAB>pid"
	SendText        []string `json:"send_text"`  // {id} {text}
	SendEnter       []string `json:"send_enter"` // {id}
	Capture         []string `json:"capture"`    // {id} -> screen text; empty: can't read the screen

	procRes []*regexp.Regexp
}

// MatchesProcess reports whether a command line is the terminal app.
func (a *TerminalApp) MatchesProcess(line string) bool { return anyMatch(a.procRes, line) }

type Rules struct {
	blocking     []*regexp.Regexp
	Version      int                     `json:"version"`
	Defaults     Defaults                `json:"defaults"`
	Harnesses    map[string]*Harness     `json:"harnesses"`
	Multiplexers map[string]*Multiplexer `json:"multiplexers"`
	TerminalApps map[string]*TerminalApp `json:"terminal_apps"`
}

// Load returns the embedded rules merged with an optional override file.
// A broken override file is reported but never stops Continuum: the embedded
// rules are used instead.
func Load(overridePath string) (*Rules, error) {
	base, err := parse(defaultJSON)
	if err != nil {
		return nil, fmt.Errorf("embedded rules: %w", err)
	}
	if overridePath == "" {
		return base, base.compile()
	}
	data, err := os.ReadFile(overridePath)
	if err != nil {
		if os.IsNotExist(err) {
			return base, base.compile()
		}
		return base, joinErr(base.compile(), fmt.Errorf("read %s: %w", overridePath, err))
	}
	merged, mErr := mergeJSON(defaultJSON, data)
	if mErr != nil {
		return base, joinErr(base.compile(), fmt.Errorf("rules override %s ignored: %w", overridePath, mErr))
	}
	r, pErr := parse(merged)
	if pErr != nil {
		return base, joinErr(base.compile(), fmt.Errorf("rules override %s ignored: %w", overridePath, pErr))
	}
	if cErr := r.compile(); cErr != nil {
		return base, joinErr(base.compile(), fmt.Errorf("rules override %s ignored: %w", overridePath, cErr))
	}
	return r, nil
}

func joinErr(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

func parse(data []byte) (*Rules, error) {
	var raw struct {
		Version      int                        `json:"version"`
		Defaults     Defaults                   `json:"defaults"`
		Harnesses    map[string]json.RawMessage `json:"harnesses"`
		Multiplexers map[string]json.RawMessage `json:"multiplexers"`
		TerminalApps map[string]json.RawMessage `json:"terminal_apps"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	r := &Rules{Version: raw.Version, Defaults: raw.Defaults, Harnesses: map[string]*Harness{}, Multiplexers: map[string]*Multiplexer{}, TerminalApps: map[string]*TerminalApp{}}
	for name, msg := range raw.TerminalApps {
		if strings.HasPrefix(name, "_") {
			continue
		}
		a := &TerminalApp{}
		if err := json.Unmarshal(msg, a); err != nil {
			return nil, fmt.Errorf("terminal app %s: %w", name, err)
		}
		r.TerminalApps[name] = a
	}
	for name, msg := range raw.Harnesses {
		if strings.HasPrefix(name, "_") {
			continue
		}
		h := &Harness{}
		if err := json.Unmarshal(msg, h); err != nil {
			return nil, fmt.Errorf("harness %s: %w", name, err)
		}
		h.Name = name
		r.Harnesses[name] = h
	}
	for name, msg := range raw.Multiplexers {
		if strings.HasPrefix(name, "_") {
			continue
		}
		m := &Multiplexer{}
		if err := json.Unmarshal(msg, m); err != nil {
			return nil, fmt.Errorf("multiplexer %s: %w", name, err)
		}
		m.Name = name
		r.Multiplexers[name] = m
	}
	return r, nil
}

func compileAll(list []string) ([]*regexp.Regexp, error) {
	out := make([]*regexp.Regexp, 0, len(list))
	for _, s := range list {
		re, err := regexp.Compile(s)
		if err != nil {
			return nil, fmt.Errorf("regex %q: %w", s, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func (r *Rules) compile() error {
	var berr error
	if r.blocking, berr = compileAll(r.Defaults.BlockingPatterns); berr != nil {
		return fmt.Errorf("blocking_patterns: %w", berr)
	}
	for name, h := range r.Harnesses {
		if h.UseCommonPatterns && !h.commonMerged {
			h.commonMerged = true
			h.Patterns = append(h.Patterns, r.Defaults.CommonPatterns...)
			h.NeverResumePatterns = append(h.NeverResumePatterns, r.Defaults.CommonNeverResumePatterns...)
		}
		for i := range h.Patterns {
			re, err := regexp.Compile(h.Patterns[i].Regex)
			if err != nil {
				return fmt.Errorf("harness %s pattern %q: %w", name, h.Patterns[i].Regex, err)
			}
			h.Patterns[i].re = re
		}
		var err error
		if h.nativeWait, err = compileAll(h.NativeWaitPatterns); err != nil {
			return fmt.Errorf("harness %s: %w", name, err)
		}
		if h.nativeGaveUp, err = compileAll(h.NativeGaveUpPatterns); err != nil {
			return fmt.Errorf("harness %s: %w", name, err)
		}
		if h.neverResume, err = compileAll(h.NeverResumePatterns); err != nil {
			return fmt.Errorf("harness %s: %w", name, err)
		}
		if h.ignoreEntry, err = compileAll(h.IgnoreEntrypoints); err != nil {
			return fmt.Errorf("harness %s: %w", name, err)
		}
		if h.uiMarkers, err = compileAll(h.UIMarkers); err != nil {
			return fmt.Errorf("harness %s: %w", name, err)
		}
		if h.processRes, err = compileAll(h.ProcessNamePatterns); err != nil {
			return fmt.Errorf("harness %s: %w", name, err)
		}
		if h.argRes, err = compileAll(h.ProcessArgPatterns); err != nil {
			return fmt.Errorf("harness %s: %w", name, err)
		}
		for _, a := range h.DesktopApps {
			if a.procRes, err = compileAll(a.ProcessPatterns); err != nil {
				return fmt.Errorf("harness %s desktop app %s: %w", name, a.Name, err)
			}
		}
	}
	for name, a := range r.TerminalApps {
		var err error
		if a.procRes, err = compileAll(a.ProcessPatterns); err != nil {
			return fmt.Errorf("terminal app %s: %w", name, err)
		}
	}
	if r.Defaults.MaxAttempts <= 0 {
		r.Defaults.MaxAttempts = 8
	}
	if r.Defaults.EnterDelayMS <= 0 {
		r.Defaults.EnterDelayMS = 400
	}
	if r.Defaults.TailLines <= 0 {
		r.Defaults.TailLines = 40
	}
	if r.Defaults.ResumePrompt == "" {
		r.Defaults.ResumePrompt = "Continue."
	}
	if len(r.Defaults.TransientBackoffSeconds) == 0 {
		r.Defaults.TransientBackoffSeconds = []int{120, 300, 900, 1800, 3600}
	}
	if len(r.Defaults.UnknownResetBackoffSeconds) == 0 {
		r.Defaults.UnknownResetBackoffSeconds = []int{900, 1800, 3600}
	}
	return nil
}

// Harness returns the named harness, falling back to "generic".
func (r *Rules) Harness(name string) *Harness {
	if h, ok := r.Harnesses[name]; ok {
		return h
	}
	return r.Harnesses["generic"]
}

// HarnessNames returns harness names sorted, "generic" last.
func (r *Rules) HarnessNames() []string {
	var names []string
	for n := range r.Harnesses {
		if n != "generic" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if _, ok := r.Harnesses["generic"]; ok {
		names = append(names, "generic")
	}
	return names
}

// MultiplexersByPriority returns multiplexers, innermost first.
func (r *Rules) MultiplexersByPriority() []*Multiplexer {
	out := make([]*Multiplexer, 0, len(r.Multiplexers))
	for _, m := range r.Multiplexers {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (h *Harness) Prompt(d Defaults) string {
	if h != nil && h.ResumePrompt != "" {
		return h.ResumePrompt
	}
	return d.ResumePrompt
}

func (h *Harness) Grace(d Defaults) int {
	if h != nil && h.NativeGraceSeconds != nil {
		return *h.NativeGraceSeconds
	}
	return d.NativeGraceSeconds
}

// Match is one detection hit.
type Match struct {
	Kind string // usage_limit | transient
	Text string // the matched line, with a line of context either side
	Hit  string // the text the pattern matched
}

// Classify scans text for limit messages. Only the last tailLines non-empty
// lines are considered so that an agent talking *about* limits earlier in a
// conversation does not trigger anything.
func (h *Harness) Classify(text string, tailLines int) *Match {
	if h == nil {
		return nil
	}
	lines := TailLines(text, tailLines)
	// Walk from the bottom: the most recent message wins.
	for i := len(lines) - 1; i >= 0; i-- {
		line := lines[i]
		for _, p := range h.Patterns {
			if p.re != nil && p.re.MatchString(line) {
				ctx := line
				if i+1 < len(lines) {
					ctx = line + "\n" + lines[i+1]
				}
				if i > 0 {
					ctx = lines[i-1] + "\n" + ctx
				}
				return &Match{Kind: p.Kind, Text: strings.TrimSpace(ctx), Hit: p.re.FindString(line)}
			}
		}
	}
	return nil
}

func anyMatch(res []*regexp.Regexp, text string) bool {
	for _, re := range res {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

func (h *Harness) NeverResume(text string) bool   { return h != nil && anyMatch(h.neverResume, text) }
func (h *Harness) NativeWaiting(text string) bool { return h != nil && anyMatch(h.nativeWait, text) }
func (h *Harness) NativeGaveUp(text string) bool  { return h != nil && anyMatch(h.nativeGaveUp, text) }
func (h *Harness) HasUIMarker(text string) bool   { return h != nil && anyMatch(h.uiMarkers, text) }

// HookHarness returns the agent whose hook variables are set in env (an
// agent that runs another agent's hooks), or "".
func (r *Rules) HookHarness(env map[string]string) string {
	for _, n := range r.HarnessNames() {
		for _, v := range r.Harness(n).HookEnv {
			if env[v] != "" {
				return n
			}
		}
	}
	return ""
}

// IgnoredEntrypoint reports whether runs with this entrypoint (print mode, an
// SDK script) are left alone.
func (h *Harness) IgnoredEntrypoint(ep string) bool {
	return h != nil && ep != "" && anyMatch(h.ignoreEntry, ep)
}

// HasStrongUIMarker is HasUIMarker without the one-symbol markers (a prompt
// character or a box corner), which a shell prompt or an editor can show too.
func (h *Harness) HasStrongUIMarker(text string) bool {
	if h == nil {
		return false
	}
	for i, re := range h.uiMarkers {
		letters := 0
		for _, c := range h.UIMarkers[i] {
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
				letters++
			}
		}
		if letters >= 4 && re.MatchString(text) {
			return true
		}
	}
	return false
}

// IsHarnessProcess reports whether a process name belongs to this harness.
func (h *Harness) IsHarnessProcess(name string) bool {
	if h == nil {
		return false
	}
	base := name
	if i := strings.LastIndexAny(base, `/\`); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(strings.ToLower(base), ".exe")
	for _, p := range h.ProcessNames {
		if strings.EqualFold(base, p) || strings.HasPrefix(base, strings.ToLower(p)+"-") {
			return true
		}
	}
	return anyMatch(h.processRes, base)
}

// BlockingDialog returns the line showing an open menu, approval prompt or
// other dialog in the bottom of the screen, or "" when the agent is simply
// waiting for input. Typing there could pick an option, so callers must not.
func (r *Rules) BlockingDialog(screen string) string {
	return r.dialogIn(TailLines(screen, 12))
}

// BlockingDialogAnywhere is BlockingDialog over all of text, for output
// whose last lines may be partial redraws rather than the whole screen.
func (r *Rules) BlockingDialogAnywhere(text string) string {
	return r.dialogIn(strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n"))
}

// dialogIn checks each line, then the lines together: some menus are only
// recognisable across lines (a "❯ Yes" line above a "No" line).
func (r *Rules) dialogIn(lines []string) string {
	for _, l := range lines {
		for _, re := range r.blocking {
			if re.MatchString(l) {
				return strings.TrimSpace(l)
			}
		}
	}
	joined := strings.Join(lines, "\n")
	for _, re := range r.blocking {
		if m := re.FindString(joined); m != "" {
			for _, l := range strings.Split(m, "\n") {
				if l = strings.TrimSpace(l); l != "" {
					return l
				}
			}
		}
	}
	return ""
}

// TailLines returns the last n non-blank lines of text.
func TailLines(text string, n int) []string {
	raw := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out := make([]string, 0, n)
	for i := len(raw) - 1; i >= 0 && len(out) < n; i-- {
		l := strings.TrimRight(raw[i], " \t\r")
		if strings.TrimSpace(l) == "" {
			continue
		}
		out = append(out, l)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

var (
	cacheMu   sync.Mutex
	cacheKey  string
	cacheVal  *Rules
	cacheErr  error
	cacheTime int64
)

// Cached loads rules once per override-file modification time.
func Cached(overridePath string) (*Rules, error) {
	var mod int64
	if st, err := os.Stat(overridePath); err == nil {
		mod = st.ModTime().UnixNano()
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cacheVal != nil && cacheKey == overridePath && cacheTime == mod {
		return cacheVal, cacheErr
	}
	cacheVal, cacheErr = Load(overridePath)
	cacheKey, cacheTime = overridePath, mod
	return cacheVal, cacheErr
}
