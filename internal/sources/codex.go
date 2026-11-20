package sources

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/resettime"
	"github.com/continuum-dev/continuum/internal/rules"
)

type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexPayload struct {
	Type       string          `json:"type"`
	ID         string          `json:"id"`
	SessionID  string          `json:"session_id"`
	Cwd        string          `json:"cwd"`
	Originator string          `json:"originator"`
	Source     json.RawMessage `json:"source"`
	Message    string          `json:"message"`
	Error      json.RawMessage `json:"error"`
	RateLimits *codexLimits    `json:"rate_limits"`
	CodexError json.RawMessage `json:"codex_error_info"`
	Role       string          `json:"role"`
}

type codexWindow struct {
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    int64   `json:"resets_at"`
}

type codexLimits struct {
	LimitID              string       `json:"limit_id"`
	Primary              *codexWindow `json:"primary"`
	Secondary            *codexWindow `json:"secondary"`
	RateLimitReachedType *string      `json:"rate_limit_reached_type"`
}

// CodexState carries per-file context between scans.
type CodexState struct {
	SessionID  string       `json:"session_id"`
	Cwd        string       `json:"cwd"`
	Originator string       `json:"originator"`
	Limits     *codexLimits `json:"limits,omitempty"`
	// Headless: a `codex exec` run or a subagent thread. Nobody types into
	// those, and continuing one would start work the user didn't ask for.
	Headless bool `json:"headless,omitempty"`
	// Trigger: when the latest request to the model went out (a turn
	// started, or a tool's output was sent back), not yet answered.
	// InFlight: the request the model is answering now.
	Trigger  time.Time `json:"trigger,omitzero"`
	InFlight time.Time `json:"in_flight,omitzero"`
	// LimitsAt: when Limits was read. TurnAt: when the latest turn started.
	// A snapshot older than the turn may be another model's limit.
	LimitsAt time.Time `json:"limits_at,omitzero"`
	TurnAt   time.Time `json:"turn_at,omitzero"`
}

// headroom: every window of the plan's limits has room left, and none is
// reported reached. Answers paid from credits leave the windows at 100%.
func (l *codexLimits) headroom() bool {
	if l == nil || l.Primary == nil && l.Secondary == nil {
		return false
	}
	if l.RateLimitReachedType != nil && *l.RateLimitReachedType != "" {
		return false
	}
	for _, w := range []*codexWindow{l.Primary, l.Secondary} {
		if w != nil && w.UsedPercent >= 99.5 {
			return false
		}
	}
	return true
}

// codexModelOutput: a response item the model wrote (not input sent to it).
func codexModelOutput(p codexPayload) bool {
	switch p.Type {
	case "reasoning", "function_call", "custom_tool_call", "local_shell_call", "web_search_call":
		return true
	case "message":
		return p.Role == "assistant"
	}
	return false
}

// codexHeadless reports whether a rollout's session_meta is a non-interactive
// run (codex exec, a subagent thread) rather than a session someone works in.
func codexHeadless(originator string, source json.RawMessage) bool {
	if strings.Contains(strings.ToLower(originator), "exec") {
		return true
	}
	var s string
	if json.Unmarshal(source, &s) == nil {
		return strings.EqualFold(s, "exec") || strings.EqualFold(s, "mcp")
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(source, &obj) == nil {
		_, sub := obj["subagent"]
		return sub
	}
	return false
}

var uuidRe = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// CodexSessionFromPath extracts the thread id from a rollout file name.
func CodexSessionFromPath(path string) string {
	ids := uuidRe.FindAllString(filepath.Base(path), -1)
	if len(ids) == 0 {
		return ""
	}
	return ids[len(ids)-1]
}

// codexMeta reads the session_meta line at the start of a rollout file.
func codexMeta(path string) CodexState {
	st := CodexState{SessionID: CodexSessionFromPath(path)}
	f, err := os.Open(path)
	if err != nil {
		return st
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for i := 0; i < 5 && sc.Scan(); i++ {
		var cl codexLine
		if json.Unmarshal(sc.Bytes(), &cl) != nil || cl.Type != "session_meta" {
			continue
		}
		var p codexPayload
		if json.Unmarshal(cl.Payload, &p) == nil {
			if p.ID != "" {
				st.SessionID = p.ID
			} else if p.SessionID != "" {
				st.SessionID = p.SessionID
			}
			st.Cwd = p.Cwd
			st.Originator = p.Originator
			st.Headless = codexHeadless(p.Originator, p.Source)
		}
		break
	}
	return st
}

func codexErr(raw json.RawMessage) (msg, info string) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		msg, _ = m["message"].(string)
		switch v := m["codex_error_info"].(type) {
		case string:
			info = v
		case map[string]any:
			for k := range v {
				info = k
			}
		}
	}
	return msg, info
}

var codexTransientInfo = map[string]bool{
	"server_overloaded": true, "internal_server_error": true, "response_stream_disconnected": true,
	"response_stream_connection_failed": true, "response_too_many_failed_attempts": true, "http_connection_failed": true,
}

// ParseCodex turns new rollout lines into events. state is updated in place.
func ParseCodex(r *rules.Rules, path string, lines []Line, state *CodexState, local *time.Location) []core.Event {
	h := r.Harness("codex")
	if state.SessionID == "" {
		*state = codexMeta(path)
	}
	var events []core.Event
	var lastActivity *core.Event
	for _, ln := range lines {
		var cl codexLine
		if json.Unmarshal(ln.Data, &cl) != nil {
			continue
		}
		var p codexPayload
		if json.Unmarshal(cl.Payload, &p) != nil {
			continue
		}
		at := parseTime(cl.Timestamp)
		if at.IsZero() {
			continue
		}
		if cl.Type == "session_meta" {
			if p.ID != "" {
				state.SessionID = p.ID
			}
			if p.Cwd != "" {
				state.Cwd = p.Cwd
			}
			state.Originator = p.Originator
			state.Headless = codexHeadless(p.Originator, p.Source)
			continue
		}
		if state.Headless {
			continue
		}
		if cl.Type == "turn_context" && p.Cwd != "" {
			state.Cwd = p.Cwd
		}
		if cl.Type == "response_item" {
			switch {
			case strings.HasSuffix(p.Type, "_call_output"):
				state.Trigger = at // the tool's output goes back to the model
			case codexModelOutput(p) && !state.Trigger.IsZero():
				state.InFlight, state.Trigger = state.Trigger, time.Time{}
			}
			continue
		}
		if cl.Type != "event_msg" {
			continue
		}
		base := core.Event{
			Harness: "codex", SessionID: state.SessionID, Cwd: state.Cwd, TranscriptPath: path,
			At: at, Source: "rollout", TranscriptSize: ln.EndPos, Entrypoint: state.Originator,
		}
		switch p.Type {
		case "token_count":
			if p.RateLimits != nil {
				state.Limits, state.LimitsAt = p.RateLimits, at
				if !state.InFlight.IsZero() && p.RateLimits.headroom() {
					// Answered on the plan, with room left: after another
					// session's limit, the limit is back (a banked reset).
					ev := base
					ev.Kind = core.KindActivity
					ev.RequestAt, ev.LimitID = state.InFlight, p.RateLimits.LimitID
					events = append(events, ev)
				}
			}
			state.InFlight = time.Time{}
		case "task_complete", "error", "turn_aborted", "stream_error":
			msg, info := codexErr(p.Error)
			if msg == "" && p.Type != "task_complete" {
				msg = p.Message
			}
			if infoMsg, i2 := codexErr(p.CodexError); i2 != "" || infoMsg != "" {
				if info == "" {
					info = i2
				}
			}
			if p.Type != "stream_error" {
				// The turn is over (or failed): no request is waiting for
				// an answer, whatever comes next.
				state.Trigger, state.InFlight = time.Time{}, time.Time{}
			}
			if msg == "" && info == "" {
				continue
			}
			ev := base
			ev.Message = msg
			switch {
			case h.NeverResume(msg):
				ev.Kind = core.KindNeverResume
			case strings.HasPrefix(info, "usage_limit"):
				ev.Kind = core.KindUsageLimit
			case codexTransientInfo[info]:
				ev.Kind = core.KindTransient
			default:
				if m := h.Classify(msg, 4); m != nil {
					ev.Kind = m.Kind
				}
			}
			if ev.Kind == "" || (p.Type == "stream_error" && ev.Kind == core.KindTransient) {
				// Codex retries stream errors itself; only a finished, failed turn matters.
				continue
			}
			if ev.Kind == core.KindUsageLimit {
				// The snapshot describes the limit hit only if it was read in
				// this turn; an older one may be another model's (a /model
				// switch). Its id counts only if it shows the limit full.
				var l *codexLimits
				if !state.LimitsAt.Before(state.TurnAt) {
					l = state.Limits
				}
				ev.ResetAt = codexReset(l, msg, at, local)
				if l != nil && !l.headroom() {
					ev.LimitID = l.LimitID
				}
			}
			state.Trigger, state.InFlight = time.Time{}, time.Time{}
			events = append(events, ev)
			lastActivity = nil
		case "user_message", "task_started", "agent_message", "agent_reasoning", "exec_command_begin":
			if p.Type == "user_message" || p.Type == "task_started" {
				state.Trigger, state.TurnAt = at, at
			}
			ev := base
			ev.Kind = core.KindActivity
			lastActivity = &ev
		}
	}
	if lastActivity != nil {
		events = append(events, *lastActivity)
	}
	return events
}

// codexReset prefers the exact epoch from the exhausted rate-limit window and
// falls back to the wording ("try again at 5:53 PM").
func codexReset(l *codexLimits, msg string, at time.Time, local *time.Location) time.Time {
	if l != nil {
		var best int64
		for _, w := range []*codexWindow{l.Primary, l.Secondary} {
			if w != nil && w.UsedPercent >= 99.5 && w.ResetsAt > best {
				best = w.ResetsAt
			}
		}
		if best > 0 && time.Unix(best, 0).After(at) {
			return time.Unix(best, 0)
		}
	}
	if t, ok := resettime.Parse(msg, at, local); ok {
		return t
	}
	return time.Time{}
}
