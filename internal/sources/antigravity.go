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

// antigravityLine is the subset of an Antigravity CLI transcript.jsonl entry
// Continuum reads. Unknown fields are ignored.
type antigravityLine struct {
	StepIndex    int             `json:"step_index"`
	Source       string          `json:"source"`
	Type         string          `json:"type"`
	Status       string          `json:"status"`
	CreatedAt    string          `json:"created_at"`
	Model        string          `json:"model"`
	Content      json.RawMessage `json:"content"`
	Error        json.RawMessage `json:"error"`
	ErrorMessage string          `json:"error_message"`
	UserMessage  string          `json:"user_message"`
	Message      string          `json:"message"`
}

// AntigravityState carries per-transcript context between scans.
type AntigravityState struct {
	RequestAt time.Time `json:"request_at,omitzero"`
	Model     string    `json:"model,omitempty"`
}

// Keep reports whether there is anything to carry to the next scan.
func (st *AntigravityState) Keep() bool { return !st.RequestAt.IsZero() || st.Model != "" }

// antigravitySessionID extracts the conversation id from
// .../brain/<conversation-id>/.system_generated/logs/transcript.jsonl.
func antigravitySessionID(path string) string {
	dir := filepath.Dir(filepath.Clean(path))
	if filepath.Base(dir) == "logs" {
		parent := filepath.Dir(dir)
		if filepath.Base(parent) == ".system_generated" {
			if id := filepath.Base(filepath.Dir(parent)); id != "" && id != "." && id != string(filepath.Separator) {
				return id
			}
		}
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

// ParseAntigravity turns Antigravity transcript lines into events on their own.
func ParseAntigravity(r *rules.Rules, path string, lines []Line, local *time.Location) []core.Event {
	return ParseAntigravityState(r, path, lines, &AntigravityState{}, local)
}

// ParseAntigravityState turns new Antigravity transcript lines into events; st
// carries request timing and the active model between scans.
func ParseAntigravityState(r *rules.Rules, path string, lines []Line, st *AntigravityState, local *time.Location) []core.Event {
	if st == nil {
		st = &AntigravityState{}
	}
	h := r.Harness("antigravity")
	sid := antigravitySessionID(path)
	var events []core.Event
	var lastActivity, answered *core.Event
	flushAnswered := func() {
		if answered != nil {
			events = append(events, *answered)
			answered = nil
		}
	}

	for _, ln := range lines {
		var al antigravityLine
		if json.Unmarshal(ln.Data, &al) != nil {
			continue
		}
		at := parseTime(al.CreatedAt)
		if at.IsZero() {
			continue
		}
		if al.Model != "" {
			st.Model = al.Model
		}
		base := core.Event{
			Harness: "antigravity", SessionID: sid, TranscriptPath: path,
			At: at, Source: "transcript", TranscriptSize: ln.EndPos,
		}
		content := ContentText(al.Content)
		errCode := strings.TrimSpace(errString(al.Error))
		isErr := strings.EqualFold(al.Type, "ERROR_MESSAGE") ||
			strings.EqualFold(al.Type, "ERROR") ||
			(strings.EqualFold(al.Type, "PLANNER_RESPONSE") && (strings.EqualFold(al.Status, "ERROR") ||
				strings.EqualFold(al.Status, "FAILED") ||
				errCode != "" || al.ErrorMessage != ""))

		switch {
		case isErr:
			text := joinUniqueNonEmpty(errCode, al.ErrorMessage, al.UserMessage, al.Message, content)
			if text == "" {
				continue
			}
			ev := base
			ev.Message = text
			ev.Model = st.Model
			switch {
			case h.NeverResume(text):
				ev.Kind = core.KindNeverResume
			default:
				if m := h.Classify(text, 8); m != nil {
					ev.Kind = m.Kind
				} else if errCode != "" {
					if k, ok := h.StopFailureKinds[strings.ToLower(errCode)]; ok {
						ev.Kind = k
					}
				}
			}
			if ev.Kind == "" {
				continue
			}
			if ev.Kind == core.KindUsageLimit {
				if t, ok := resettime.Parse(text, at, local); ok {
					ev.ResetAt = t
				}
			}
			st.RequestAt = time.Time{}
			flushAnswered()
			events = append(events, ev)
			lastActivity = nil

		case strings.EqualFold(al.Type, "USER_INPUT"):
			st.RequestAt = at
			ev := base
			ev.Kind = core.KindActivity
			lastActivity = &ev

		case strings.EqualFold(al.Type, "PLANNER_RESPONSE") && strings.EqualFold(al.Status, "DONE"):
			model := al.Model
			if model == "" {
				model = st.Model
			}
			if content != "" && h.Classify(content, strings.Count(content, "\n")+1) != nil {
				flushAnswered()
				ev := base
				ev.Kind = core.KindReply
				ev.Message = content
				ev.Model = model
				events = append(events, ev)
			} else if !st.RequestAt.IsZero() {
				ev := base
				ev.Kind = core.KindActivity
				ev.RequestAt = st.RequestAt
				ev.Model = model
				answered = &ev
			} else {
				ev := base
				ev.Kind = core.KindActivity
				ev.Model = model
				lastActivity = &ev
			}
			st.RequestAt = time.Time{}
		}
	}

	if answered != nil {
		if lastActivity != nil && !lastActivity.At.After(answered.At) {
			lastActivity.RequestAt, lastActivity.Model = answered.RequestAt, answered.Model
		} else {
			events = append(events, *answered)
		}
	}
	if lastActivity != nil {
		events = append(events, *lastActivity)
	}
	return events
}

func joinUniqueNonEmpty(parts ...string) string {
	var out []string
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return strings.Join(out, "\n")
}
