// Package deliver sends the resume prompt to a stalled agent session. It tries,
// in order: the Continuum PTY wrapper the agent runs in, the multiplexer pane
// it runs in, and finally the agent's own CLI (claude --bg --resume, codex
// queue --resume, ...). None of these need the agent's window to be focused.
package deliver

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/mux"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
)

// Request is written into a wrapper's inbox.
type Request struct {
	ID   string `json:"id"`
	Text string `json:"text"`
	At   string `json:"at"`
}

type Options struct {
	Rules      *rules.Rules
	Paths      core.Paths
	Config     core.Config
	Prompt     string
	DryRun     bool
	Strategies []string
	Log        func(format string, args ...any)
	// Live is what is known about whether the session is open right now. Nil
	// means unknown: then only the hook-recorded pid is used.
	Live *live.State
}

// ErrStillOpen means the session is open somewhere Continuum cannot type
// into, and every remaining route would have started a second copy.
var ErrStillOpen = errors.New("the session is (or may be) still open somewhere Continuum cannot type into, and Continuum never starts a second copy")

// Result describes a successful delivery.
type Result struct {
	Via    string
	Detail string
}

// ErrNoRoute means no strategy could reach the session.
var ErrNoRoute = errors.New("no delivery route reached the session")

func (o Options) logf(format string, args ...any) {
	if o.Log != nil {
		o.Log(format, args...)
	}
}

// Deliver sends the resume prompt for job.
func Deliver(o Options, job *core.Job) (Result, error) {
	h := o.Rules.Harness(job.Harness)
	var errs []string
	stillOpen := false
	for _, s := range o.Strategies {
		var res Result
		var err error
		switch s {
		case "wrapper":
			res, err = viaWrapper(o, job)
		case "multiplexer":
			res, err = viaMultiplexer(o, h, job)
		case "terminal-app":
			res, err = viaTerminalApp(o, h, job)
		case "native", "headless":
			if !o.Config.HeadlessAllowed() {
				err = errors.New("disabled by allow_headless=false")
			} else {
				res, err = viaNative(o, h, job)
			}
		default:
			err = fmt.Errorf("unknown strategy %q", s)
		}
		if err == nil {
			o.logf("route %s: delivered (%s)", s, res.Detail)
			return res, nil
		}
		if errors.Is(err, errSkip) {
			o.logf("route %s: not applicable", s)
		} else {
			o.logf("route %s: %v", s, err)
		}
		if errors.Is(err, ErrStillOpen) {
			stillOpen = true
		}
		if !errors.Is(err, errSkip) {
			errs = append(errs, s+": "+err.Error())
		}
	}
	if len(errs) == 0 {
		return Result{}, ErrNoRoute
	}
	if stillOpen {
		return Result{}, fmt.Errorf("%w; %w (%s)", ErrNoRoute, ErrStillOpen, strings.Join(errs, "; "))
	}
	return Result{}, fmt.Errorf("%w (%s)", ErrNoRoute, strings.Join(errs, "; "))
}

var errSkip = errors.New("not applicable")

// WrapperDir returns the directory of a wrapper instance.
func WrapperDir(p core.Paths, id string) string { return filepath.Join(p.Wrappers, id) }

// WrapperAlive reports whether the wrapper id still runs.
func WrapperAlive(p core.Paths, id string) bool {
	data, err := os.ReadFile(filepath.Join(WrapperDir(p, id), "pid"))
	if err != nil {
		return false
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return proc.Alive(pid)
}

func viaWrapper(o Options, job *core.Job) (Result, error) {
	if job.Location == nil || job.Location.Kind != "wrapper" {
		return Result{}, errSkip
	}
	id := job.Location.Target
	if !WrapperAlive(o.Paths, id) {
		return Result{}, fmt.Errorf("wrapper %s is no longer running", id)
	}
	if o.DryRun {
		return Result{Via: "wrapper", Detail: "dry run: would type into wrapper " + id}, nil
	}
	req := Request{ID: core.NewID(), Text: o.Prompt, At: time.Now().Format(time.RFC3339)}
	data, _ := json.Marshal(req)
	inbox := filepath.Join(WrapperDir(o.Paths, id), "inbox")
	file := filepath.Join(inbox, req.ID+".json")
	if err := core.WriteFileAtomic(file, data, 0o600); err != nil {
		return Result{}, err
	}
	// The wrapper deletes the request once typed, or leaves a .rejected note.
	rejected := filepath.Join(inbox, req.ID+".rejected")
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(file); os.IsNotExist(err) {
			if why, err := os.ReadFile(rejected); err == nil {
				os.Remove(rejected)
				return Result{}, fmt.Errorf("wrapper %s: %s", id, strings.TrimSpace(string(why)))
			}
			return Result{Via: "wrapper", Detail: "typed into continuum run " + id}, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	os.Remove(file)
	return Result{}, fmt.Errorf("wrapper %s did not pick up the request", id)
}

func viaMultiplexer(o Options, h *rules.Harness, job *core.Job) (Result, error) {
	loc := job.Location
	if loc == nil || loc.Kind == "" || loc.Kind == "wrapper" {
		return Result{}, errSkip
	}
	if _, ok := o.Rules.Multiplexers[loc.Kind]; !ok {
		return Result{}, fmt.Errorf("unknown multiplexer %s", loc.Kind)
	}
	if o.Live != nil && o.Live.Checked && !o.Live.Open && o.Live.Authoritative {
		return Result{}, fmt.Errorf("the session is closed (the agent says so); its old pane %s may now hold something else", loc.String())
	}
	if err := VerifyPane(o.Rules, h, job, loc); err != nil {
		return Result{}, err
	}
	if o.DryRun {
		return Result{Via: loc.Kind, Detail: "dry run: would type into " + loc.String()}, nil
	}
	delay := time.Duration(o.Rules.Defaults.EnterDelayMS) * time.Millisecond
	if err := mux.Send(o.Rules, loc, o.Prompt, delay); err != nil {
		return Result{}, err
	}
	return Result{Via: loc.Kind, Detail: "typed into " + loc.String()}, nil
}

// Procs is how delivery inspects processes. Tests replace it.
var Procs = struct {
	List      func() ([]proc.Process, error)
	Environ   func(pid int) map[string]string
	Supported func() bool
	Alive     func(pid int) bool
	Info      func(pid int) (ppid int, name string, ok bool)
	State     func(pid int) string
	StartTime func(pid int) (time.Time, bool)
}{proc.List, proc.Environ, proc.EnvironSupported, proc.Alive, proc.Info, proc.State, proc.StartTime}

type paneVerdict int

const (
	paneUnknown paneVerdict = iota // processes can't be checked here: fall back to the screen
	paneOwned                      // the session's own agent process runs there
	paneOther                      // an agent runs there, but it is a different session
	paneEmpty                      // no agent runs there any more
)

// VerifyPane makes sure the pane still runs this session's agent, in the
// foreground and waiting for input, before anything is typed: a prompt must
// never land in a shell, an editor or a different session, and Enter must
// never land in a menu or approval prompt.
func VerifyPane(r *rules.Rules, h *rules.Harness, job *core.Job, loc *core.Location) error {
	text, err := mux.Capture(r, loc)
	if err != nil {
		return fmt.Errorf("pane %s is unavailable: %w", loc.String(), err)
	}
	if dialog := r.BlockingDialog(text); dialog != "" {
		return fmt.Errorf("pane %s has a menu or prompt open (%q); not typing into it", loc.String(), dialog)
	}
	pid, verdict := paneAgent(r, h, job, loc)
	switch verdict {
	case paneOwned:
		st := Procs.State(pid)
		if st != "" && strings.ContainsAny(st[:1], "TtZ") {
			return fmt.Errorf("pane %s: %s is suspended (Ctrl+Z) or stopped; not typing into what is in front of it", loc.String(), h.DisplayName)
		}
		if st != "" && !strings.Contains(st, "+") {
			return fmt.Errorf("pane %s: %s is not in the foreground of its terminal (another program is); not typing into it", loc.String(), h.DisplayName)
		}
		if job != nil && job.PID <= 0 {
			job.PID = pid
		}
		return nil
	case paneOther:
		return fmt.Errorf("pane %s now runs a different %s session; not typing this session's message into it", loc.String(), h.DisplayName)
	case paneEmpty:
		return fmt.Errorf("pane %s no longer runs %s", loc.String(), h.DisplayName)
	}
	// Processes can't be matched to panes here (Windows, or the process list
	// failed): rely on the pane's foreground program and the screen.
	if name, ok := mux.ProcessName(r, loc); ok && name != "" {
		if h.IsHarnessProcess(name) {
			return nil
		}
		if !r.IsInterpreter(name) {
			return fmt.Errorf("pane %s runs %s, not %s", loc.String(), name, h.DisplayName)
		}
	}
	if h.HasStrongUIMarker(strings.Join(rules.TailLines(text, 15), "\n")) {
		return nil
	}
	return fmt.Errorf("pane %s no longer shows %s", loc.String(), h.DisplayName)
}

// paneAgent finds the agent process running in pane loc, from the pane
// variable every process in it inherits (TMUX_PANE, REX_BLOCK, ...).
func paneAgent(r *rules.Rules, h *rules.Harness, job *core.Job, loc *core.Location) (int, paneVerdict) {
	m, ok := r.Multiplexers[loc.Kind]
	if !ok || m.EnvTarget == "" || !Procs.Supported() {
		return 0, paneUnknown
	}
	list, err := Procs.List()
	if err != nil || len(list) == 0 {
		return 0, paneUnknown
	}
	lines := make(map[int]string, len(list))
	for _, p := range list {
		lines[p.PID] = p.Line
	}
	byLine := func(pid int) bool {
		line := lines[pid]
		f := strings.Fields(line)
		return len(f) > 0 && h.IsHarnessProcess(f[0]) || line != "" && h.MatchesCommandLine(line)
	}
	isAgent := func(pid int) bool { // the line, else the process name (one ps call)
		if byLine(pid) {
			return true
		}
		_, name, ok := Procs.Info(pid)
		return ok && h.IsHarnessProcess(name)
	}
	inPane := func(pid int) bool { return mux.InPane(r, Procs.Environ(pid), loc) }
	recorded := 0
	if job != nil {
		recorded = job.PID
	}
	if recorded > 0 && Procs.Alive(recorded) && isAgent(recorded) && inPane(recorded) {
		return recorded, paneOwned
	}
	found := map[int]bool{}
	var order []int
	unreadable := 0
	for _, p := range list {
		if p.PID == os.Getpid() || len(p.Args) == 0 || !byLine(p.PID) {
			continue
		}
		env := Procs.Environ(p.PID)
		if len(env) == 0 {
			// macOS hides the environment of its own programs (/bin/bash,
			// /usr/bin/python3): this one can't be placed in a pane.
			unreadable++
			continue
		}
		if mux.InPane(r, env, loc) {
			found[p.PID] = true
			order = append(order, p.PID)
		}
	}
	// The agent itself, not its helper processes (which share its pane).
	var top []int
	for _, pid := range order {
		if ppid, _, ok := Procs.Info(pid); !ok || !found[ppid] {
			top = append(top, pid)
		}
	}
	if len(top) == 0 {
		if unreadable > 0 {
			return 0, paneUnknown // it may be one of those: check the screen
		}
		return 0, paneEmpty
	}
	if recorded > 0 {
		// The session's own process has gone; whatever runs there now is a
		// session the user started since.
		return top[0], paneOther
	}
	// No process recorded: it can only be this session if it was already
	// running when the session stalled.
	for _, pid := range top {
		if job == nil || job.LimitAt.IsZero() {
			return pid, paneOwned
		}
		if t, ok := Procs.StartTime(pid); ok && t.After(job.LimitAt.Add(time.Minute)) {
			continue
		}
		return pid, paneOwned
	}
	return top[0], paneOther
}

// capability cache: binary path+mtime -> help text.
var (
	helpMu    sync.Mutex
	helpCache = map[string]string{}
)

func helpText(bin string, sub ...string) string {
	key := bin + " " + strings.Join(sub, " ")
	if st, err := os.Stat(bin); err == nil {
		key += "@" + st.ModTime().String()
	}
	helpMu.Lock()
	if v, ok := helpCache[key]; ok {
		helpMu.Unlock()
		return v
	}
	helpMu.Unlock()
	argv := append([]string{bin}, sub...)
	argv = append(argv, "--help")
	out, _ := proc.Run(20*time.Second, "", argv...)
	helpMu.Lock()
	helpCache[key] = out
	helpMu.Unlock()
	return out
}

// Supported reports whether bin offers everything cmd needs.
func Supported(bin string, cmd rules.NativeCommand) (bool, string) {
	top := helpText(bin)
	for _, tok := range cmd.RequiresHelp {
		if !strings.Contains(top, tok) {
			return false, fmt.Sprintf("%s --help has no %q", filepath.Base(bin), tok)
		}
	}
	for sub, toks := range cmd.RequiresSubcommandHelp {
		txt := helpText(bin, sub)
		for _, tok := range toks {
			if !strings.Contains(txt, tok) {
				return false, fmt.Sprintf("%s %s --help has no %q", filepath.Base(bin), sub, tok)
			}
		}
	}
	return true, ""
}

// Binary resolves the agent's executable.
func Binary(c core.Config, h *rules.Harness) (string, error) {
	name := h.Binary
	if v, ok := c.Binaries[h.Name]; ok && v != "" {
		name = v
	}
	if name == "" {
		return "", errors.New("no binary configured")
	}
	return proc.LookPath(name)
}

// sessionStopped reports whether the session is known to be closed, so that
// continuing it under its own id cannot create a second copy.
func sessionStopped(o Options, job *core.Job) (bool, string) {
	if o.Live != nil && o.Live.Checked {
		if o.Live.Open {
			where := "open"
			if o.Live.DesktopApp != "" {
				where = "open in " + o.Live.DesktopApp
			} else if s := o.Live.Session; s != nil && s.PID > 0 {
				where = fmt.Sprintf("open in process %d", s.PID)
			}
			return false, "the session is still " + where
		}
		return true, ""
	}
	if job.PID > 0 {
		if proc.Alive(job.PID) {
			return false, fmt.Sprintf("the session's process %d is still running", job.PID)
		}
		return true, ""
	}
	return false, "cannot tell whether the session is still open"
}

func viaNative(o Options, h *rules.Harness, job *core.Job) (Result, error) {
	if len(h.NativeCommands) == 0 {
		return Result{}, errSkip
	}
	if job.SessionID == "" {
		return Result{}, errors.New("session id unknown")
	}
	bin, err := Binary(o.Config, h)
	if err != nil {
		return Result{}, err
	}
	stopped, openWhy := sessionStopped(o, job)
	background := o.Live != nil && o.Live.Open && o.Live.Background
	var reasons []string
	blockedByOpen := false
	for _, nc := range h.NativeCommands {
		switch nc.UsedWhen() {
		case "stopped":
			// A command that delivers through the desktop app's own backend
			// reaches the session the app holds: that is not a copy.
			viaApp := nc.ReachesDesktop && o.Live != nil && o.Live.DesktopApp != ""
			if !stopped && !viaApp && !o.Config.CopiesAllowed() {
				reasons = append(reasons, nc.Name+": "+openWhy)
				blockedByOpen = true
				continue
			}
		case "background":
			if !background {
				continue
			}
		}
		if ok, why := Supported(bin, nc); !ok {
			reasons = append(reasons, nc.Name+": "+why)
			continue
		}
		short := ""
		if o.Live != nil && o.Live.Session != nil {
			short = o.Live.Session.ShortID
		}
		if short == "" {
			short = job.SessionID
		}
		vars := map[string]string{"bin": bin, "session": job.SessionID, "short_id": short, "prompt": o.Prompt, "cwd": job.Cwd}
		argv := make([]string, 0, len(nc.Args))
		for _, a := range nc.Args {
			v := a
			for k, val := range vars {
				v = strings.ReplaceAll(v, "{"+k+"}", val)
			}
			argv = append(argv, v)
		}
		dir := job.Cwd
		if dir != "" {
			if st, err := os.Stat(dir); err != nil || !st.IsDir() {
				dir = ""
			}
		}
		if o.DryRun {
			return Result{Via: nc.Name, Detail: "dry run: would run " + strings.Join(argv, " ")}, nil
		}
		env := CleanEnv(os.Environ())
		if nc.PTY {
			if err := typeViaPTY(o.Rules, h, argv, dir, env, o.Prompt); err != nil {
				reasons = append(reasons, nc.Name+": "+err.Error())
				continue
			}
			return Result{Via: nc.Name, Detail: "typed into the open session through " + strings.Join(argv[:min(2, len(argv))], " ")}, nil
		}
		logDir := o.Paths.Home
		if logDir == "" {
			logDir = core.GetPaths().Home
		}
		logPath := filepath.Join(logDir, "native-"+nc.Name+".log")
		if nc.Detach {
			pid, err := proc.StartDetached(argv, dir, env, logPath)
			if err != nil {
				reasons = append(reasons, nc.Name+": "+err.Error())
				continue
			}
			return Result{Via: nc.Name, Detail: fmt.Sprintf("started %s (pid %d), output in %s", filepath.Base(bin), pid, logPath)}, nil
		}
		out, err := runIn(dir, env, 90*time.Second, argv)
		if err != nil {
			reasons = append(reasons, nc.Name+": "+err.Error())
			continue
		}
		return Result{Via: nc.Name, Detail: strings.TrimSpace(firstLine(out))}, nil
	}
	if len(reasons) == 0 {
		return Result{}, errSkip
	}
	err = errors.New(strings.Join(reasons, "; "))
	if blockedByOpen {
		err = fmt.Errorf("%w: %v", ErrStillOpen, err)
	}
	return Result{}, err
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// CleanEnv drops variables that tie a process to the terminal or agent it was
// started from, so a resumed agent does not think it is nested in another.
func CleanEnv(environ []string) []string {
	drop := []string{"CLAUDECODE=", "CLAUDE_CODE_", "CLAUDE_PID=", "CLAUDE_EFFORT=", "CLAUDE_PLUGIN", "CODEX_COMPANION", "CODEX_THREAD", "CODEX_SANDBOX",
		"TMUX=", "TMUX_PANE=", "STY=", "WINDOW=", "ZELLIJ", "HERDR_", "REX_BLOCK=", "REX_SESSION=", "WEZTERM_PANE=", "KITTY_WINDOW_ID=", "CONTINUUM_WRAPPER_ID="}
	keep := []string{"CLAUDE_CONFIG_DIR=", "CLAUDE_CODE_USE_BEDROCK=", "CLAUDE_CODE_USE_VERTEX=", "CLAUDE_CODE_OAUTH_TOKEN="}
	var out []string
outer:
	for _, kv := range environ {
		for _, k := range keep {
			if strings.HasPrefix(kv, k) {
				out = append(out, kv)
				continue outer
			}
		}
		for _, d := range drop {
			if strings.HasPrefix(kv, d) {
				continue outer
			}
		}
		if strings.HasPrefix(kv, "PATH=") {
			kv = "PATH=" + proc.AugmentedPATH()
		}
		out = append(out, kv)
	}
	return out
}
