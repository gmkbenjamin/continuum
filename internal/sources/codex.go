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
}

type codexWindow struct {
	UsedPercent float64 `json:"used_percent"`
	ResetsAt    int64   `json:"resets_at"`
}

type codexLimits struct {
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
			continue
		}
		if cl.Type == "turn_context" && p.Cwd != "" {
			state.Cwd = p.Cwd
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
				state.Limits = p.RateLimits
			}
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
				ev.ResetAt = codexReset(state.Limits, msg, at, local)
			}
			events = append(events, ev)
			lastActivity = nil
		case "user_message", "task_started", "agent_message", "agent_reasoning", "exec_command_begin":
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
