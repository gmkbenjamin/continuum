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
}

func rawString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		for _, k := range []string{"type", "code", "error"} {
			if v, ok := m[k].(string); ok {
				return v
			}
		}
	}
	return ""
}

// BuildEvent turns a hook payload plus the hook's environment into an event.
// ok is false when the hook carries nothing Continuum needs.
func BuildEvent(r *rules.Rules, harness string, in Input, env map[string]string, now time.Time) (core.Event, bool) {
	h := r.Harness(harness)
	ev := core.Event{
		Harness: harness, SessionID: in.SessionID, Cwd: in.Cwd, TranscriptPath: in.TranscriptPath,
		At: now, Source: "hook:" + in.HookEventName,
	}
	if id := env["CONTINUUM_WRAPPER_ID"]; id != "" {
		ev.Location = &core.Location{Kind: "wrapper", Target: id}
	} else {
		ev.Location = mux.LocateFromEnv(r, env)
	}
	if v := env["CLAUDE_CODE_ENTRYPOINT"]; v != "" && harness == "claude" {
		ev.Entrypoint = v
	}
	if pid, err := strconv.Atoi(env["CLAUDE_PID"]); err == nil && harness == "claude" {
		ev.PID = pid
	}
	if st, err := os.Stat(in.TranscriptPath); err == nil {
		ev.TranscriptSize = st.Size()
	}

	switch strings.ToLower(in.HookEventName) {
	case "sessionstart":
		ev.Kind = core.KindSessionStart
	case "sessionend":
		ev.Kind = core.KindSessionEnd
	case "stopfailure":
		text := strings.TrimSpace(in.LastAssistantMessage + "\n" + in.ErrorDetails)
		errCode := rawString(in.Error)
		switch {
		case h.NeverResume(text):
			ev.Kind = core.KindNeverResume
		default:
			if m := h.Classify(text, 8); m != nil {
				ev.Kind = m.Kind
			} else if k, ok := h.StopFailureKinds[errCode]; ok {
				ev.Kind = k
			}
		}
		if ev.Kind == "" {
			return ev, false
		}
		ev.Message = text
		if ev.Kind == core.KindUsageLimit {
			if t, ok := resettime.Parse(text, now, time.Local); ok {
				ev.ResetAt = t
			}
		}
	case "stop", "userpromptsubmit", "posttooluse":
		// A turn ended normally (or the user typed): if the last message is a
		// limit message treat it as one, otherwise it is activity.
		if m := h.Classify(in.LastAssistantMessage, 8); m != nil {
			ev.Kind = m.Kind
			ev.Message = in.LastAssistantMessage
			if t, ok := resettime.Parse(in.LastAssistantMessage, now, time.Local); ok {
				ev.ResetAt = t
			}
		} else {
			ev.Kind = core.KindActivity
		}
	default:
		ev.Kind = core.KindActivity
	}
	if ev.PID == 0 && ev.Kind == core.KindSessionStart {
		ev.PID = findAncestor(h)
	}
	return ev, true
}

// findAncestor returns the pid of the nearest ancestor process that is the
// agent itself (the hook runs as its child or grandchild).
func findAncestor(h *rules.Harness) int {
	pid := os.Getppid()
	for i := 0; i < 6 && pid > 1; i++ {
		ppid, name, ok := proc.Info(pid)
		if !ok {
			return 0
		}
		if h.IsHarnessProcess(name) {
			return pid
		}
		pid = ppid
	}
	return 0
}

// Handle is the body of `continuum hook <harness>`. It never returns an error
// to the agent and prints nothing (some hooks feed stdout to the model).
func Handle(harness string, stdin io.Reader, environ []string) {
	defer func() { _ = recover() }()
	p := core.GetPaths()
	if err := p.Ensure(); err != nil {
		return
	}
	data, _ := io.ReadAll(io.LimitReader(stdin, 4<<20))
	var in Input
	if json.Unmarshal(data, &in) != nil {
		return
	}
	r, _ := rules.Cached(p.Rules)
	if r == nil {
		return
	}
	ev, ok := BuildEvent(r, harness, in, mux.EnvMap(environ), time.Now())
	if !ok {
		return
	}
	if ev.Kind == core.KindActivity {
		// Routine notes are only useful to a running daemon; never let them
		// pile up when it is switched off.
		if entries, err := os.ReadDir(p.Spool); err == nil && len(entries) > 500 {
			return
		}
	}
	_ = core.SpoolEvent(p, ev)
}

// ---- install ---------------------------------------------------------------

// Target describes one agent settings file Continuum manages.
type Target struct {
	Harness string
	Path    string
	Events  []string
	Timeout int
}

func Targets(c core.Config) []Target {
	return []Target{
		{Harness: "claude", Path: filepath.Join(c.ClaudeDir(), "settings.json"), Events: []string{"SessionStart", "StopFailure", "Stop", "SessionEnd"}, Timeout: 10},
		{Harness: "codex", Path: filepath.Join(c.CodexDir(), "hooks.json"), Events: []string{"SessionStart", "Stop"}, Timeout: 10},
	}
}

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
	if runtime.GOOS == "windows" {
		return quote(bin) + " hook " + harness
	}
	inner := `"` + shDoubleQuoteEscape(bin) + `" hook ` + harness + ` 2>/dev/null || true`
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
	hooksV, _ := root.get("hooks")
	hooksObj, ok := hooksV.(*object)
	if !ok {
		if hooksV != nil {
			return false, fmt.Errorf("%s: \"hooks\" is not an object; not touching it", t.Path)
		}
		hooksObj = newObject()
		root.set("hooks", hooksObj)
	}
	want := Command(bin, t.Harness)
	// Drop Continuum hooks from events this version no longer uses.
	for _, event := range append([]string(nil), hooksObj.keys...) {
		if containsEvent(t.Events, event) {
			continue
		}
		v, _ := hooksObj.get(event)
		if list, ok := v.([]any); ok {
			if kept := removeOurs(list); len(kept) != len(list) || countOurs(list) > 0 {
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
		entry.set("type", "command")
		entry.set("command", want)
		entry.set("timeout", json.Number(strconv.Itoa(t.Timeout)))
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
	hooksV, _ := root.get("hooks")
	hooksObj, ok := hooksV.(*object)
	if !ok {
		return false, nil
	}
	for _, event := range append([]string(nil), hooksObj.keys...) {
		v, _ := hooksObj.get(event)
		list, ok := v.([]any)
		if !ok {
			continue
		}
		list = removeOurs(list)
		if len(list) == 0 {
			hooksObj.del(event)
		} else {
			hooksObj.set(event, list)
		}
	}
	if len(hooksObj.keys) == 0 {
		root.del("hooks")
	}
	out, err := pretty(root)
	if err != nil {
		return false, err
	}
	// Give the file back exactly as it was: if what is left matches a
	// backup Continuum took before changing it, use that backup's bytes
	// (same formatting, key order and whitespace).
	if b := matchingBackup(t.Path, out); b != nil {
		out = b
	} else if len(root.keys) == 0 && len(Backups(t.Path)) == 0 {
		// Continuum created this file and nothing else is in it: remove it.
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
func matchingBackup(path string, want []byte) []byte {
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
		if bd.Decode(&b) == nil && reflect.DeepEqual(w, b) {
			return data
		}
	}
	return nil
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
	hooksObj, _ := func() (*object, bool) { v, _ := root.get("hooks"); o, ok := v.(*object); return o, ok }()
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
				} else if s == want {
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
	hooksV, _ := root.get("hooks")
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
	v, _ := o.get("hooks")
	var out []*object
	for _, x := range asList(v) {
		if h, ok := x.(*object); ok {
			out = append(out, h)
		}
	}
	return out
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
