package hooks

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/resettime"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/sources"
)

// hookDriver encapsulates agent-specific hook payload parsing, process
// filtering, and shell command formatting.
type hookDriver interface {
	EnrichEvent(in Input, env map[string]string, ev *core.Event)
	// HandleStop optionally parses a Stop/AfterAgentResponse event with
	// agent-specific semantics. handled=false falls back to generic Stop handling.
	HandleStop(h *rules.Harness, in Input, now time.Time, ev *core.Event) (handled, keep bool)
	SanitizeProcess(r *rules.Rules, tree map[int]proc.Node, ppid int, ev *core.Event)
	ValidateCLI(in Input, env map[string]string, r *rules.Rules, tree map[int]proc.Node, ppid int, ev core.Event) (bool, string)
	SelfReportsInteractive() bool
	ShellFallback() string
	WindowsFallback() string
}

type defaultHookDriver struct{ harness string }

func (defaultHookDriver) EnrichEvent(Input, map[string]string, *core.Event) {}
func (defaultHookDriver) HandleStop(*rules.Harness, Input, time.Time, *core.Event) (bool, bool) {
	return false, false
}
func (defaultHookDriver) SanitizeProcess(*rules.Rules, map[int]proc.Node, int, *core.Event) {}
func (d defaultHookDriver) ValidateCLI(_ Input, _ map[string]string, r *rules.Rules, tree map[int]proc.Node, ppid int, _ core.Event) (bool, string) {
	if len(tree) > 0 {
		if a, _ := NearestAgent(r, tree, ppid); a != d.harness {
			return false, "not run by its command-line agent; ignored"
		}
	}
	return true, ""
}
func (defaultHookDriver) SelfReportsInteractive() bool { return false }
func (defaultHookDriver) ShellFallback() string        { return "true" }
func (defaultHookDriver) WindowsFallback() string      { return "" }

type claudeHookDriver struct{ defaultHookDriver }

func (claudeHookDriver) EnrichEvent(_ Input, env map[string]string, ev *core.Event) {
	if env == nil {
		return
	}
	if v := env["CLAUDE_CODE_ENTRYPOINT"]; v != "" {
		ev.Entrypoint = v
	}
	if pid, err := strconv.Atoi(env["CLAUDE_PID"]); err == nil {
		ev.PID = pid
	}
}
func (claudeHookDriver) SelfReportsInteractive() bool { return true }

type codexHookDriver struct{ defaultHookDriver }

func (codexHookDriver) SanitizeProcess(r *rules.Rules, tree map[int]proc.Node, ppid int, ev *core.Event) {
	_, pid := NearestAgent(r, tree, ppid)
	if ev.PID > 0 {
		pid = ev.PID
	}
	if pid > 0 && live.CodexAppServer(ProcArgs(pid)) {
		// The shared app-server inherited its launcher's terminal, which may
		// belong to an entirely different conversation.
		ev.Location, ev.PID = nil, 0
	}
}
func (codexHookDriver) SelfReportsInteractive() bool { return true }

type antigravityHookDriver struct{ defaultHookDriver }

func (antigravityHookDriver) EnrichEvent(_ Input, env map[string]string, ev *core.Event) {
	if ev.SessionID == "" && env != nil {
		ev.SessionID = env["ANTIGRAVITY_CONVERSATION_ID"]
	}
}

func (antigravityHookDriver) HandleStop(h *rules.Harness, in Input, now time.Time, ev *core.Event) (bool, bool) {
	errCode, errMsg := rawErrorParts(in.Error)
	var errParts []string
	for _, p := range []string{errCode, errMsg, strings.TrimSpace(in.ErrorDetails)} {
		if p != "" {
			errParts = append(errParts, p)
		}
	}
	errText := strings.Join(errParts, "\n")
	if strings.EqualFold(in.TerminationReason, "ERROR") || errText != "" {
		switch {
		case errText != "" && h.NeverResume(errText):
			ev.Kind = core.KindNeverResume
		case errText != "":
			if m := h.Classify(errText, 8); m != nil {
				ev.Kind = m.Kind
			} else if errCode != "" {
				if k, ok := h.StopFailureKinds[strings.ToLower(errCode)]; ok {
					ev.Kind = k
				} else {
					ev.Kind = core.KindTurnFailed
				}
			} else {
				ev.Kind = core.KindTurnFailed
			}
		default:
			return true, false
		}
		ev.Message = errText
		ev.Model = in.Model
		if ev.Kind == core.KindUsageLimit {
			if t, ok := resettime.Parse(errText, now, time.Local); ok {
				ev.ResetAt = t
			}
		}
		return true, true
	}
	switch strings.ToUpper(strings.TrimSpace(in.TerminationReason)) {
	case "NO_TOOL_CALL", "COMPLETED", "END_TURN", "STOP", "DONE":
		reply := clip(in.LastAssistantMessage, 8000)
		if reply != "" && h.Classify(reply, strings.Count(reply, "\n")+1) != nil {
			ev.Kind = core.KindReply
			ev.Message = reply
			ev.Model = in.Model
		} else {
			ev.Kind = core.KindActivity
			ev.Model = in.Model
			ev.Answered = h.StopHookAnswers
		}
		return true, true
	case "USER_CANCELED":
		ev.Kind = core.KindActivity
		ev.Model = in.Model
		return true, true
	default:
		return true, false
	}
}

func (antigravityHookDriver) ValidateCLI(in Input, env map[string]string, r *rules.Rules, tree map[int]proc.Node, ppid int, ev core.Event) (bool, string) {
	if !isAntigravityCLI(in, env, r, tree, ppid) {
		return false, "not run by Antigravity CLI; ignored"
	}
	if ev.TranscriptPath != "" && sources.IsAntigravitySubagent(ev.TranscriptPath) {
		return false, "Antigravity subagent conversation; ignored"
	}
	return true, ""
}

func (antigravityHookDriver) ShellFallback() string   { return `printf "{}\n"` }
func (antigravityHookDriver) WindowsFallback() string { return "2>NUL || echo {}" }

func hookDriverFor(harness string) hookDriver {
	switch harness {
	case "claude":
		return claudeHookDriver{defaultHookDriver{harness: harness}}
	case "codex":
		return codexHookDriver{defaultHookDriver{harness: harness}}
	case "antigravity":
		return antigravityHookDriver{defaultHookDriver{harness: harness}}
	default:
		return defaultHookDriver{harness: harness}
	}
}

// CodexHooksTrustNote inspects ~/.codex/config.toml to see whether Continuum's
// entries in ~/.codex/hooks.json are trusted and enabled by Codex. It returns
// an empty string when enabled or not applicable.
func CodexHooksTrustNote(cfg core.Config) string {
	hooksPath := filepath.Join(cfg.CodexDir(), "hooks.json")
	root, _, err := load(hooksPath)
	if err != nil {
		return ""
	}
	hv, _ := root.get("hooks")
	hobj, ok := hv.(*object)
	if !ok {
		return ""
	}
	// Find indices of Continuum's hooks in ~/.codex/hooks.json.
	var wantSections []string
	for _, ev := range []struct{ jsonKey, stateKey string }{
		{"SessionStart", "session_start"},
		{"Stop", "stop"},
	} {
		raw, _ := hobj.get(ev.jsonKey)
		groups, _ := raw.([]any)
		for gi, g := range groups {
			goObj, _ := g.(*object)
			if goObj == nil {
				continue
			}
			hlRaw, _ := goObj.get("hooks")
			hl, _ := hlRaw.([]any)
			for hi, h := range hl {
				hEntry, _ := h.(*object)
				if hEntry == nil {
					continue
				}
				cmdRaw, _ := hEntry.get("command")
				cmd, _ := cmdRaw.(string)
				if isOurs(cmd) {
					wantSections = append(wantSections, hooksPath+":"+ev.stateKey+":"+strconv.Itoa(gi)+":"+strconv.Itoa(hi))
				}
			}
		}
	}
	if len(wantSections) == 0 {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(cfg.CodexDir(), "config.toml"))
	if err != nil {
		return ""
	}
	// Parse [hooks.state."<section>"] blocks for enabled = true.
	sectionEnabled := map[string]bool{}
	sectionSeen := map[string]bool{}
	curSection := ""
	for _, line := range strings.Split(string(data), "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "[") && strings.HasSuffix(trim, "]") {
			curSection = ""
			inner := strings.TrimSpace(trim[1 : len(trim)-1])
			if rest, ok := strings.CutPrefix(inner, `hooks.state."`); ok && strings.HasSuffix(rest, `"`) {
				curSection = rest[:len(rest)-1]
				sectionSeen[curSection] = true
			}
			continue
		}
		if curSection == "" {
			continue
		}
		if k, v, ok := strings.Cut(trim, "="); ok && strings.TrimSpace(k) == "enabled" {
			if strings.TrimSpace(v) == "true" {
				sectionEnabled[curSection] = true
			}
		}
	}
	disabled := 0
	for _, sec := range wantSections {
		if sectionSeen[sec] && !sectionEnabled[sec] {
			disabled++
		}
	}
	if disabled > 0 {
		return "Codex has not enabled Continuum's hooks in ~/.codex/config.toml (enable them in Codex or add `enabled = true` under [hooks.state]); rollout tailing still covers Codex"
	}
	return ""
}
