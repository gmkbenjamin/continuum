// Package mux talks to terminal multiplexers (tmux, screen, zellij, herdr,
// Rex, WezTerm, kitty, and any defined in rules) through their own CLIs. Input
// goes straight to a pane, so it works whether the window is focused,
// minimised, hidden or detached.
package mux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
)

// Runner executes a command. Tests replace it.
var Runner = func(timeout time.Duration, stdin string, argv ...string) (string, error) {
	if len(argv) > 0 {
		if p, err := proc.LookPath(argv[0]); err == nil {
			argv = append([]string{p}, argv[1:]...)
		}
	}
	return proc.Run(timeout, stdin, argv...)
}

const cmdTimeout = 8 * time.Second

// LocateFromEnv works out which pane the current process runs in. env maps
// variable names to values (use EnvMap(os.Environ())).
func LocateFromEnv(r *rules.Rules, env map[string]string) *core.Location {
	for _, m := range r.MultiplexersByPriority() {
		if loc := locate(m, env); loc != nil {
			return loc
		}
	}
	return nil
}

// InPane reports whether a process with environment env runs in the pane
// loc (its pane variable, e.g. TMUX_PANE or REX_BLOCK, names that pane).
func InPane(r *rules.Rules, env map[string]string, loc *core.Location) bool {
	if loc == nil {
		return false
	}
	m, ok := r.Multiplexers[loc.Kind]
	if !ok {
		return false
	}
	got := locate(m, env)
	if got == nil || got.Target != loc.Target {
		return false
	}
	// A pane id is only unique within its server or session (tmux's %3,
	// screen's window 0 and zellij's pane 0 exist in each one): when both
	// say which, it must be the same.
	return got.Socket == "" || loc.Socket == "" || got.Socket == loc.Socket
}

func locate(m *rules.Multiplexer, env map[string]string) *core.Location {
	if m == nil || m.EnvTarget == "" {
		return nil
	}
	target := env[m.EnvTarget]
	if target == "" {
		return nil
	}
	for _, req := range m.RequireEnv {
		if env[req] == "" {
			return nil
		}
	}
	socket := ""
	if m.EnvSocket != "" {
		socket = env[m.EnvSocket]
	}
	if m.SocketTransform == "first_csv_field" {
		if i := strings.IndexByte(socket, ','); i >= 0 {
			socket = socket[:i]
		}
	}
	if m.TargetFormat != "" && !strings.Contains(target, "_") {
		target = fmt.Sprintf(m.TargetFormat, target)
	}
	return &core.Location{Kind: m.Name, Target: target, Socket: socket}
}

func EnvMap(environ []string) map[string]string {
	out := make(map[string]string, len(environ))
	for _, kv := range environ {
		if i := strings.IndexByte(kv, '='); i > 0 {
			out[kv[:i]] = kv[i+1:]
		}
	}
	return out
}

// Expand fills placeholders. Optional groups are dropped when any of their
// placeholders is empty.
func Expand(args []rules.Arg, vars map[string]string) []string {
	var out []string
	for _, a := range args {
		if a.Group != nil {
			var grp []string
			ok := true
			for _, g := range a.Group {
				v, full := fill(g, vars)
				if !full {
					ok = false
					break
				}
				grp = append(grp, v)
			}
			if ok {
				out = append(out, grp...)
			}
			continue
		}
		v, _ := fill(a.Text, vars)
		out = append(out, v)
	}
	return out
}

// fill replaces {name} placeholders; full is false when any was empty.
func fill(s string, vars map[string]string) (string, bool) {
	full := true
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '{')
		if i < 0 {
			b.WriteString(s)
			break
		}
		j := strings.IndexByte(s[i:], '}')
		if j < 0 {
			b.WriteString(s)
			break
		}
		name := s[i+1 : i+j]
		if strings.ContainsAny(name, " #") || name == "" || (i > 0 && s[i-1] == '#') {
			// Not a placeholder, e.g. tmux's #{pane_id}.
			b.WriteString(s[:i+j+1])
			s = s[i+j+1:]
			continue
		}
		val, ok := vars[name]
		if !ok || val == "" {
			full = false
		}
		b.WriteString(s[:i])
		b.WriteString(val)
		s = s[i+j+1:]
	}
	return b.String(), full
}

func vars(loc *core.Location, text string) map[string]string {
	return map[string]string{
		"target":      loc.Target,
		"socket":      loc.Socket,
		"text":        text,
		"text_screen": screenEscape(text),
	}
}

// screen's command parser treats backslash and caret specially.
func screenEscape(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `^`, `\^`)
	return s
}

func get(r *rules.Rules, loc *core.Location) (*rules.Multiplexer, error) {
	if loc == nil {
		return nil, errors.New("no location")
	}
	m, ok := r.Multiplexers[loc.Kind]
	if !ok {
		return nil, fmt.Errorf("unknown multiplexer %q", loc.Kind)
	}
	return m, nil
}

// Send types text into the pane and presses Enter.
func Send(r *rules.Rules, loc *core.Location, text string, enterDelay time.Duration) error {
	m, err := get(r, loc)
	if err != nil {
		return err
	}
	if len(m.SendText) == 0 || len(m.SendEnter) == 0 {
		return fmt.Errorf("%s: sending is not defined", m.Name)
	}
	if strings.HasPrefix(text, "-") {
		// Some multiplexers' send commands would read a leading "-" as an
		// option of their own; a space in front is harmless to the agent.
		text = " " + text
	}
	v := vars(loc, text)
	stdin, _ := fill(m.SendTextStdin, v)
	if _, err := Runner(cmdTimeout, stdin, Expand(m.SendText, v)...); err != nil {
		return fmt.Errorf("%s send text: %w", m.Name, err)
	}
	time.Sleep(enterDelay)
	stdin, _ = fill(m.SendEnterStdin, v)
	if _, err := Runner(cmdTimeout, stdin, Expand(m.SendEnter, v)...); err != nil {
		return fmt.Errorf("%s send enter: %w", m.Name, err)
	}
	return nil
}

// Capture returns the pane's recent text.
func Capture(r *rules.Rules, loc *core.Location) (string, error) {
	m, err := get(r, loc)
	if err != nil {
		return "", err
	}
	if len(m.Capture) == 0 {
		return "", fmt.Errorf("%s: capture is not defined", m.Name)
	}
	v := vars(loc, "")
	var tmp string
	if m.CaptureFrom == "tmpfile" {
		// A private folder (only this user can enter it), so the pane's text
		// is never readable by others, even for a moment.
		dir, err := os.MkdirTemp("", "continuum-capture-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(dir)
		tmp = filepath.Join(dir, "screen.txt")
		v["tmpfile"] = tmp
	}
	out, err := Runner(cmdTimeout, "", Expand(m.Capture, v)...)
	if err != nil {
		return "", fmt.Errorf("%s capture: %w", m.Name, err)
	}
	if tmp != "" {
		// screen writes the file asynchronously.
		for i := 0; i < 20; i++ {
			if data, err := os.ReadFile(tmp); err == nil && len(data) > 0 {
				return string(data), nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return "", fmt.Errorf("%s capture: no output", m.Name)
	}
	return out, nil
}

// ProcessName returns the pane's foreground command when the multiplexer can
// tell; ok is false when it cannot.
func ProcessName(r *rules.Rules, loc *core.Location) (string, bool) {
	m, err := get(r, loc)
	if err != nil || len(m.Process) == 0 {
		return "", false
	}
	out, err := Runner(cmdTimeout, "", Expand(m.Process, vars(loc, ""))...)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

// Pane is one pane found by listing a multiplexer.
type Pane struct {
	Location *core.Location
	Command  string
}

// List returns panes for multiplexers that define a list command. The default
// tmux server is listed when socket is empty. A third field, when the list
// prints one, is the server's own socket path: the same one its panes' agents
// report (TMUX), so a pane found here and one a hook reported compare equal.
func List(r *rules.Rules, kind, socket string) ([]Pane, error) {
	m, ok := r.Multiplexers[kind]
	if !ok || len(m.List) == 0 {
		return nil, fmt.Errorf("%s: list is not defined", kind)
	}
	out, err := Runner(cmdTimeout, "", Expand(m.List, map[string]string{"socket": socket})...)
	if err != nil {
		return nil, err
	}
	var panes []Pane
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) == 0 || f[0] == "" {
			continue
		}
		p := Pane{Location: &core.Location{Kind: kind, Target: f[0], Socket: socket}}
		if len(f) > 1 {
			p.Command = filepath.Base(f[1])
		}
		if len(f) > 2 && strings.TrimSpace(f[2]) != "" {
			p.Location.Socket = strings.TrimSpace(f[2])
		}
		panes = append(panes, p)
	}
	return panes, nil
}
