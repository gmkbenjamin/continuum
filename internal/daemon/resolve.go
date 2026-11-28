package daemon

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/store"
)

// WrapperResumeSessionID returns the session ID explicitly targeted by a
// wrapper's command line (e.g. `codex resume <id>`, `claude --resume <id>`).
func WrapperResumeSessionID(harness string, argv []string) string {
	if len(argv) < 2 {
		return ""
	}
	args := argv
	if len(args) > 1 && strings.HasSuffix(filepath.Base(args[1]), ".js") {
		args = args[1:]
	}
	switch harness {
	case "codex":
		if len(args) >= 3 && args[1] == "resume" && looksLikeSessionID(args[2]) {
			return args[2]
		}
	default:
		for i := 1; i < len(args); i++ {
			a := args[i]
			if (a == "--resume" || a == "-r") && i+1 < len(args) && looksLikeSessionID(args[i+1]) {
				return args[i+1]
			}
			if v, ok := strings.CutPrefix(a, "--resume="); ok && looksLikeSessionID(v) {
				return v
			}
		}
	}
	return ""
}

func looksLikeSessionID(s string) bool {
	if len(s) < 6 || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// ResolveWrapperBindings computes the unambiguous mapping from session key
// ("harness:session_id") to a live Continuum wrapper instance.
//
// Both the daemon and the CLI call this function so they share one source of
// truth for which live wrapper owns which conversation:
//  1. Explicit argv resume (`codex resume <id>`, `claude --resume <id>`).
//  2. Tracked Instance.ActiveSessionID from live wrapper hooks.
//  3. Explicit hook-recorded live wrapper locations on sessions (marking those
//     wrappers claimed so another session cannot also bind to them).
//  4. Unique directory binding for harnesses without an authoritative live
//     session list (e.g. Codex running hooks via a shared app-server): when
//     there is uniquely one live wrapper and at most one interactive agent
//     process in (harness, cwd), the single most recently active non-ended
//     session seen at or after the wrapper started binds to that wrapper.
func ResolveWrapperBindings(
	r *rules.Rules,
	sessions map[string]*store.Session,
	instances map[string]*store.Instance,
	wrappers []deliver.WrapperInfo,
	procs []proc.Process,
	cwdFn func(int) string,
) map[string]deliver.WrapperInfo {
	if len(wrappers) == 0 || len(sessions) == 0 {
		return nil
	}
	byID := make(map[string]deliver.WrapperInfo, len(wrappers))
	countByKey := map[string]int{}
	for _, w := range wrappers {
		byID[w.ID] = w
		if w.Cwd != "" {
			countByKey[w.Harness+"\x00"+filepath.Clean(w.Cwd)]++
		}
	}

	bindings := map[string]deliver.WrapperInfo{}
	claimed := map[string]bool{}

	// 1. Exact resume target from wrapper argv.
	for _, w := range wrappers {
		if id := WrapperResumeSessionID(w.Harness, w.Argv); id != "" {
			key := store.SessionKey(w.Harness, id)
			if _, exists := sessions[key]; exists {
				bindings[key] = w
				claimed[w.ID] = true
			}
		}
	}

	// 2. Tracked wrapper instance's active conversation (from hooks).
	for _, w := range wrappers {
		if claimed[w.ID] || instances == nil {
			continue
		}
		inst := instances[w.ID]
		if inst == nil || inst.ActiveSessionID == "" {
			continue
		}
		key := store.SessionKey(w.Harness, inst.ActiveSessionID)
		s := sessions[key]
		if s == nil || s.Ended || DriverFor(s.Harness).IgnoreTranscript(s.TranscriptPath) {
			continue
		}
		if _, already := bindings[key]; already {
			continue
		}
		bindings[key] = w
		claimed[w.ID] = true
	}

	// 3. Sessions already explicitly pointing at a live wrapper claim it.
	// If multiple non-ended sessions share one wrapper (e.g. /clear or
	// subagents before filtering), the most recently active one wins.
	bestForWrapper := map[string]*store.Session{}
	for _, s := range sessions {
		if s == nil || s.SessionID == "" || s.Ended || s.Location == nil || s.Location.Kind != "wrapper" {
			continue
		}
		if DriverFor(s.Harness).IgnoreTranscript(s.TranscriptPath) {
			continue
		}
		w, alive := byID[s.Location.Target]
		if !alive || claimed[w.ID] {
			continue
		}
		if cur := bestForWrapper[w.ID]; cur == nil || s.LastSeen.After(cur.LastSeen) {
			bestForWrapper[w.ID] = s
		}
	}
	for wid, s := range bestForWrapper {
		key := store.SessionKey(s.Harness, s.SessionID)
		if _, already := bindings[key]; !already {
			bindings[key] = byID[wid]
		}
		claimed[wid] = true
	}

	// 3. Unique directory binding for unclaimed wrappers.
	for _, w := range wrappers {
		if claimed[w.ID] || w.Cwd == "" {
			continue
		}
		// If the wrapper explicitly resumed another conversation, never bind
		// an unrelated session to it.
		if WrapperResumeSessionID(w.Harness, w.Argv) != "" {
			continue
		}
		var h *rules.Harness
		if r != nil {
			h = r.Harness(w.Harness)
		}
		if !DriverFor(w.Harness).AllowUniqueWrapperBinding(h) {
			continue
		}
		cleanDir := filepath.Clean(w.Cwd)
		// Require uniquely one live wrapper for this agent in this directory.
		if countByKey[w.Harness+"\x00"+cleanDir] != 1 {
			continue
		}
		// If we can inspect the process table, verify there isn't a second
		// unwrapped agent process running in the same folder.
		if h != nil && len(procs) > 0 && cwdFn != nil && countHarnessProcsInDir(h, cleanDir, procs, cwdFn) > 1 {
			continue
		}
		var best *store.Session
		cutoff := w.Started.Add(-5 * time.Second)
		for _, s := range sessions {
			if s == nil || s.SessionID == "" || s.Ended || s.Harness != w.Harness || s.Cwd == "" {
				continue
			}
			if DriverFor(s.Harness).IgnoreTranscript(s.TranscriptPath) {
				continue
			}
			key := store.SessionKey(s.Harness, s.SessionID)
			if _, already := bindings[key]; already {
				continue
			}
			if filepath.Clean(s.Cwd) != cleanDir {
				continue
			}
			// Never steal a session still tied to a different live wrapper.
			if s.Location != nil && s.Location.Kind == "wrapper" {
				if _, stillAlive := byID[s.Location.Target]; stillAlive {
					continue
				}
			}
			if !w.Started.IsZero() && s.LastSeen.Before(cutoff) {
				continue
			}
			if best == nil || s.LastSeen.After(best.LastSeen) {
				best = s
			}
		}
		if best != nil {
			bindings[store.SessionKey(best.Harness, best.SessionID)] = w
			claimed[w.ID] = true
		}
	}

	return bindings
}

func countHarnessProcsInDir(h *rules.Harness, cleanDir string, procs []proc.Process, cwdFn func(int) string) int {
	n := 0
	for _, p := range procs {
		if !isInteractiveHarnessProc(h, p) {
			continue
		}
		if c := cwdFn(p.PID); c != "" && filepath.Clean(c) == cleanDir {
			n++
		}
	}
	return n
}

func isInteractiveHarnessProc(h *rules.Harness, p proc.Process) bool {
	if h == nil || len(p.Args) == 0 {
		return false
	}
	named := false
	for i, a := range p.Args {
		if i > 1 {
			break
		}
		base := filepath.Base(a)
		base = strings.TrimSuffix(strings.TrimSuffix(base, ".exe"), ".js")
		if h.IsHarnessProcess(base) {
			named = true
		}
	}
	if !named && !h.MatchesCommandLine(p.Line) {
		return false
	}
	if h.LiveSessions != nil {
		for _, a := range p.Args[1:] {
			for _, x := range h.LiveSessions.ProcessExcludeArgs {
				if a == x || filepath.Base(a) == x {
					return false
				}
			}
		}
	}
	return true
}

// SessionClosed reports whether j's session is known to be closed. Shared by
// daemon lifecycle checks and status views.
func SessionClosed(p core.Paths, j *core.Job, sess *store.Session, st *live.State) bool {
	if j == nil || DriverFor(j.Harness).ManagesOwnLifecycle(j) {
		return false
	}
	if st != nil && st.Open && (st.Authoritative || st.ProcessMatch || st.DesktopApp != "" || st.Background) {
		return false
	}
	if j.Location != nil && j.Location.Kind == "wrapper" && deliver.WrapperAlive(p, j.Location.Target) {
		return false
	}
	if st != nil && st.Checked && !st.Open {
		return true
	}
	if sess != nil && sess.Ended {
		return true
	}
	if j.Location != nil && j.Location.Kind == "wrapper" && !deliver.WrapperAlive(p, j.Location.Target) {
		return true
	}
	if j.PID > 0 && !proc.Alive(j.PID) {
		return true
	}
	return false
}
