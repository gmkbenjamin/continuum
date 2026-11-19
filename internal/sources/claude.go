package sources

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/resettime"
	"github.com/continuum-dev/continuum/internal/rules"
)

// claudeLine is the subset of a Claude Code transcript entry Continuum reads.
// Unknown fields are ignored, so new transcript fields never break parsing.
type claudeLine struct {
	Type              string          `json:"type"`
	Subtype           string          `json:"subtype"`
	SessionID         string          `json:"sessionId"`
	Cwd               string          `json:"cwd"`
	Timestamp         string          `json:"timestamp"`
	IsAPIErrorMessage bool            `json:"isApiErrorMessage"`
	Error             json.RawMessage `json:"error"`
	Content           json.RawMessage `json:"content"`
	Message           *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	IsMeta     bool   `json:"isMeta"`
	Entrypoint string `json:"entrypoint"` // cli, claude-desktop, sdk-*, ...
	// UsageLimitNote marks a note Claude Code adds to a turn about the usage
	// limit; "wrap_up" means the limit was reached and the turn is finishing
	// on a short grace allowance.
	UsageLimitNote string `json:"usageLimitNote"`
}

// ClaudeState carries per-transcript context between scans.
type ClaudeState struct {
	// WrapUp holds Claude Code's note when the turn in progress was told the
	// usage limit is reached and is finishing on a grace allowance. Such a
	// turn ends normally, with no limit error, so its end is the stall.
	WrapUp string `json:"wrap_up,omitempty"`
}

// wrapUpNote: Claude Code's note that the usage limit is reached and the turn
// may only finish on a short grace allowance ("grace stop").
func wrapUpNote(cl claudeLine, text string) bool {
	if cl.Type != "user" {
		return false
	}
	if cl.UsageLimitNote == "wrap_up" {
		return true
	}
	return cl.IsMeta && strings.HasPrefix(strings.TrimSpace(text), "[Usage limit reached;")
}

// turnEnd: the line Claude Code writes when a turn has finished.
func turnEnd(cl claudeLine) bool {
	return cl.Type == "system" && (cl.Subtype == "stop_hook_summary" || cl.Subtype == "turn_duration")
}

// toolResult: a user line that only carries tool results, part of the turn.
func toolResult(cl claudeLine) bool {
	if cl.Message == nil {
		return false
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(cl.Message.Content, &blocks) != nil || len(blocks) == 0 {
		return false
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			return false
		}
	}
	return true
}

// ContentText flattens a message content field (string or blocks).
func ContentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []map[string]any
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if t, ok := b["text"].(string); ok {
				parts = append(parts, t)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func errString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) == nil {
		for _, k := range []string{"type", "error", "code"} {
			if v, ok := m[k].(string); ok {
				return v
			}
		}
	}
	return ""
}

// notAPrompt: a user line that doesn't continue the conversation: metadata,
// a local slash command (/usage, /status) or its output, or a `!` shell
// command. Typing one of those after a limit doesn't mean the session went on.
func notAPrompt(cl claudeLine, text string) bool {
	if cl.IsMeta {
		return true
	}
	for _, tag := range []string{"<local-command-", "<command-name>", "<command-message>", "<bash-input>", "<bash-stdout>", "<bash-stderr>"} {
		if strings.Contains(text, tag) {
			return true
		}
	}
	return false
}

// ParseClaude turns transcript lines into events, reading them on their own.
func ParseClaude(r *rules.Rules, path string, lines []Line, local *time.Location) []core.Event {
	return ParseClaudeState(r, path, lines, &ClaudeState{}, local)
}

// ParseClaudeState turns new transcript lines into events; st carries what
// earlier lines of the same transcript said (a turn still wrapping up).
func ParseClaudeState(r *rules.Rules, path string, lines []Line, st *ClaudeState, local *time.Location) []core.Event {
	if st == nil {
		st = &ClaudeState{}
	}
	h := r.Harness("claude")
	var events []core.Event
	var lastActivity *core.Event
	sessionFromName := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	for _, ln := range lines {
		var cl claudeLine
		if json.Unmarshal(ln.Data, &cl) != nil {
			continue
		}
		at := parseTime(cl.Timestamp)
		if at.IsZero() {
			continue
		}
		sid := cl.SessionID
		if sid == "" {
			sid = sessionFromName
		}
		base := core.Event{
			Harness: "claude", SessionID: sid, Cwd: cl.Cwd, TranscriptPath: path,
			At: at, Source: "transcript", TranscriptSize: ln.EndPos, Entrypoint: cl.Entrypoint,
		}
		text := ""
		if cl.Message != nil {
			text = ContentText(cl.Message.Content)
		}
		if text == "" {
			text = ContentText(cl.Content)
		}

		if h.IgnoredEntrypoint(cl.Entrypoint) {
			continue // print mode or an SDK script: nobody types into it
		}
		switch {
		case cl.Type == "assistant" && cl.IsAPIErrorMessage:
			ev := base
			ev.Message = text
			errKind := errString(cl.Error)
			switch {
			case h.NeverResume(text):
				ev.Kind = core.KindNeverResume
			default:
				m := h.Classify(text, 6)
				switch {
				case m != nil:
					ev.Kind = m.Kind
				case errKind != "":
					if k, ok := h.StopFailureKinds[errKind]; ok {
						ev.Kind = k
					}
				}
			}
			if ev.Kind == "" {
				continue // an API error Continuum should not act on (bad request, auth...)
			}
			st.WrapUp = "" // the grace allowance ran out: this error is the stall
			if ev.Kind == core.KindUsageLimit {
				if t, ok := resettime.Parse(text, at, local); ok {
					ev.ResetAt = t
				}
			}
			events = append(events, ev)
			lastActivity = nil
		case cl.Type == "system" && text != "" && h.NativeGaveUp(text):
			ev := base
			ev.Kind = core.KindNativeGaveUp
			ev.Message = text
			events = append(events, ev)
			lastActivity = nil
		case wrapUpNote(cl, text):
			// The limit is reached but the turn goes on for a little while:
			// the stall is where it ends, and its work until then isn't the
			// session moving on.
			st.WrapUp = text
		case st.WrapUp != "" && turnEnd(cl):
			ev := base
			ev.Kind = core.KindUsageLimit
			ev.Message = st.WrapUp
			if t, ok := resettime.Parse(st.WrapUp, at, local); ok {
				ev.ResetAt = t
			}
			st.WrapUp = ""
			events = append(events, ev)
			lastActivity = nil
		case st.WrapUp != "" && (cl.Type == "assistant" || cl.Type == "user" && (notAPrompt(cl, text) || toolResult(cl))):
			// Still the wrapping-up turn.
		case cl.Type == "assistant" || cl.Type == "user" && !notAPrompt(cl, text):
			if cl.Type == "user" {
				st.WrapUp = "" // a new prompt: the session went on
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
