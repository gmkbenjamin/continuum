package deliver

// Typing into a terminal app (Ghostty, ...) through its scripting interface,
// for agents running in a plain terminal tab that has no multiplexer and no
// Continuum wrapper, e.g. sessions started before the shell hook existed.
//
// The terminal is found by the agent's tty, so the right tab is typed into
// whatever is focused, minimised or hidden. Safety: the terminal's foreground
// process must be the agent itself; and when the app can't show Continuum the
// screen, the agent must report on its own that no menu or prompt is open.

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
)

// Hooks for tests.
var (
	termRun   = func(argv []string) (string, error) { return proc.Run(15*time.Second, "", argv...) }
	termProcs = proc.List
	termTTY   = proc.TTY
)

func fill(argv []string, vars map[string]string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		for k, v := range vars {
			a = strings.ReplaceAll(a, "{"+k+"}", v)
		}
		out[i] = a
	}
	return out
}

func viaTerminalApp(o Options, h *rules.Harness, job *core.Job) (Result, error) {
	if len(o.Rules.TerminalApps) == 0 {
		return Result{}, errSkip
	}
	pid := job.PID
	if o.Live != nil && o.Live.Session != nil && o.Live.Session.PID > 0 {
		pid = o.Live.Session.PID
	}
	if pid <= 0 || !proc.Alive(pid) {
		return Result{}, errSkip
	}
	tty := termTTY(pid)
	if tty == "" {
		return Result{}, errSkip
	}
	procs, _ := termProcs()
	var reasons []string
	for name, app := range o.Rules.TerminalApps {
		running := false
		for _, p := range procs {
			if app.MatchesProcess(p.Line) {
				running = true
				break
			}
		}
		if !running || len(app.Find) == 0 {
			continue
		}
		out, err := termRun(fill(app.Find, map[string]string{"tty": tty}))
		if err != nil {
			reasons = append(reasons, app.Name+": "+firstLine(err.Error()))
			continue
		}
		id, fgPID := "", 0
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			f := strings.Split(strings.TrimSpace(line), "\t")
			if len(f) >= 1 && f[0] != "" {
				id = f[0]
				if len(f) > 1 {
					fgPID, _ = strconv.Atoi(strings.TrimSpace(f[1]))
				}
				break
			}
		}
		if id == "" {
			continue // the agent isn't in this app
		}
		// The terminal must be showing the agent, not a shell or another program.
		if fgPID != 0 && fgPID != pid {
			reasons = append(reasons, fmt.Sprintf("%s: the terminal now runs another program (pid %d)", app.Name, fgPID))
			continue
		}
		where := app.Name + " terminal " + id
		if len(app.Capture) > 0 {
			screen, err := termRun(fill(app.Capture, map[string]string{"id": id}))
			if err != nil {
				reasons = append(reasons, app.Name+": cannot read the screen")
				continue
			}
			if d := o.Rules.BlockingDialog(screen); d != "" {
				reasons = append(reasons, fmt.Sprintf("%s has a menu or prompt open (%q); not typing into it", where, strings.TrimSpace(d)))
				continue
			}
		} else if err := agentSaysNoDialog(o); err != nil {
			reasons = append(reasons, where+": "+err.Error())
			continue
		}
		if o.DryRun {
			return Result{Via: name, Detail: "dry run: would type into " + where}, nil
		}
		if _, err := termRun(fill(app.SendText, map[string]string{"id": id, "text": o.Prompt})); err != nil {
			reasons = append(reasons, app.Name+": "+firstLine(err.Error()))
			continue
		}
		time.Sleep(time.Duration(max(o.Rules.Defaults.EnterDelayMS, 100)) * time.Millisecond)
		if _, err := termRun(fill(app.SendEnter, map[string]string{"id": id})); err != nil {
			return Result{}, fmt.Errorf("%s: typed the prompt but could not press Enter: %v", where, err)
		}
		return Result{Via: name, Detail: "typed into " + where}, nil
	}
	if len(reasons) == 0 {
		return Result{}, errSkip
	}
	return Result{}, errors.New(strings.Join(reasons, "; "))
}

// agentSaysNoDialog requires the agent's own session list to report the
// session idle with nothing waiting for an answer, for terminals whose screen
// Continuum can't read.
func agentSaysNoDialog(o Options) error {
	if o.Live == nil || o.Live.Session == nil || (o.Live.Source != "command" && o.Live.Source != "registry" && !strings.HasPrefix(o.Live.Source, "command") && !strings.HasPrefix(o.Live.Source, "registry")) {
		return errors.New("can't read this terminal's screen, and the agent doesn't report whether a menu is open")
	}
	s := o.Live.Session
	if s.WaitingFor != "" || strings.EqualFold(s.Status, "waiting") {
		return fmt.Errorf("the agent reports it is waiting for an answer (%s); not typing", strings.TrimSpace(s.WaitingFor+" "+s.Status))
	}
	return nil
}
