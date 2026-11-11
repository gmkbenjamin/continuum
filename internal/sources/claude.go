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

// ParseClaude turns new transcript lines into events.
func ParseClaude(r *rules.Rules, path string, lines []Line, local *time.Location) []core.Event {
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
		case cl.Type == "user" || cl.Type == "assistant":
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
