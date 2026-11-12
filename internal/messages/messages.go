// Package messages decides what Continuum types to continue a session. The
// most specific message wins:
//
//  1. a message for that session (by id, or one started with
//     CONTINUUM_MESSAGE / `continuum run --message`)
//  2. a message for the folder the session runs in (the deepest folder wins)
//  3. a message for the agent (claude, codex, ...)
//  4. your default message
//  5. the built-in message
//
// Each level can have a separate message for API outages (temporary errors),
// used instead of the usage-limit one when the stall was an outage.
//
// Messages are stored in ~/.continuum/messages.json.
package messages

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/scope"
)

// BuiltInOutage is typed after a temporary API error.
const BuiltInOutage = "The previous request failed with a temporary API error. Continue the task from where it stopped; do not repeat work that is already complete."

// MaxLen bounds a message (it is typed into the agent's input box).
const MaxLen = 4000

// Pair is a usage-limit message and an optional outage message.
type Pair struct {
	Text   string `json:"text,omitempty"`
	Outage string `json:"outage,omitempty"`
}

// Rule is a message for a session, a wrapper or a folder.
type Rule struct {
	scope.Entry
	Pair
}

type Messages struct {
	Default Pair            `json:"default,omitempty"`
	Agents  map[string]Pair `json:"agents,omitempty"`
	Rules   []Rule          `json:"sessions,omitempty"`
}

func Path(p core.Paths) string { return filepath.Join(p.Home, "messages.json") }

func Load(p core.Paths) (*Messages, error) {
	m := &Messages{Agents: map[string]Pair{}}
	data, err := os.ReadFile(Path(p))
	if err != nil {
		if os.IsNotExist(err) {
			return m, nil
		}
		return m, err
	}
	if err := json.Unmarshal(data, m); err != nil {
		return &Messages{Agents: map[string]Pair{}}, err
	}
	if m.Agents == nil {
		m.Agents = map[string]Pair{}
	}
	return m, nil
}

func (m *Messages) Save(p core.Paths) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return core.WriteFileAtomic(Path(p), append(data, '\n'), 0o600)
}

// Clean makes a message safe to type: one line (a line break would send it
// early), no tabs, trimmed, bounded.
func Clean(s string) (string, error) {
	s = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\t", " ").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "", errors.New("the message is empty")
	}
	if len(s) > MaxLen {
		return "", errors.New("the message is too long (4000 characters at most)")
	}
	return s, nil
}

// Choice is the message picked for a session, and where it came from.
type Choice struct {
	Text   string
	Source string // "session", "folder ~/x", "agent claude", "default", "built-in"
}

// Resolve picks the message for job j. outage selects the outage message.
// cfg.ResumePrompts (older config) still applies at the agent level.
func (m *Messages) Resolve(j *core.Job, outage bool, cfg core.Config, r *rules.Rules) Choice {
	pick := func(p Pair) string {
		if outage {
			return p.Outage
		}
		return p.Text
	}
	c := Choice{}
	// 1-2: session, wrapper and folder rules; the most specific wins.
	best, bestScore := "", 0
	for _, rule := range m.Rules {
		text := pick(rule.Pair)
		if text == "" || !rule.Matches(j) {
			continue
		}
		score := 1000 // a session or wrapper
		src := "session"
		if rule.Dir != "" {
			score = len(filepath.Clean(rule.Dir)) // deeper folder, higher score
			src = "folder " + rule.Dir
		}
		if score > bestScore {
			best, bestScore, c.Source = text, score, src
		}
	}
	if best != "" {
		c.Text = best
	} else if p, ok := m.Agents[j.Harness]; ok && pick(p) != "" {
		c.Text, c.Source = pick(p), "agent "+j.Harness
	} else if key := j.Harness + map[bool]string{true: "_transient", false: ""}[outage]; cfg.ResumePrompts[key] != "" {
		c.Text, c.Source = cfg.ResumePrompts[key], "agent "+j.Harness+" (config.json)"
	} else if t := pick(m.Default); t != "" {
		c.Text, c.Source = t, "default"
	} else if outage {
		c.Text, c.Source = BuiltInOutage, "built-in"
	} else {
		c.Text, c.Source = r.Harness(j.Harness).Prompt(r.Defaults), "built-in"
	}
	// Every message is typed as one line, wherever it came from (an older
	// config.json, a hand-edited messages.json, a folder name in {folder}).
	if t, err := Clean(fill(c.Text, j, r)); err == nil {
		c.Text = t
	} else if outage {
		c.Text, c.Source = BuiltInOutage, "built-in"
	} else {
		c.Text, c.Source = r.Harness(j.Harness).Prompt(r.Defaults), "built-in"
	}
	return c
}

// fill replaces {agent} and {folder}.
func fill(s string, j *core.Job, r *rules.Rules) string {
	name := j.Harness
	if h := r.Harness(j.Harness); h != nil && h.DisplayName != "" {
		name = h.DisplayName
	}
	folder := filepath.Base(j.Cwd)
	if j.Cwd == "" || folder == "." || folder == "/" {
		folder = "this project"
	}
	return strings.NewReplacer("{agent}", name, "{folder}", folder).Replace(s)
}

// Set adds or replaces the message for an entry (a session, folder or
// wrapper). An empty pair removes it.
func (m *Messages) Set(e scope.Entry, p Pair) {
	for i := range m.Rules {
		if same(m.Rules[i].Entry, e) {
			if p.Text == "" && p.Outage == "" {
				m.Rules = append(m.Rules[:i], m.Rules[i+1:]...)
			} else {
				m.Rules[i].Pair = merge(m.Rules[i].Pair, p)
			}
			return
		}
	}
	if p.Text != "" || p.Outage != "" {
		m.Rules = append(m.Rules, Rule{Entry: e, Pair: p})
	}
}

// Clear removes the message (or only the outage one) for an entry.
func (m *Messages) Clear(e scope.Entry, outageOnly bool) bool {
	for i := range m.Rules {
		if same(m.Rules[i].Entry, e) || (e.Session != "" && strings.HasPrefix(m.Rules[i].Session, e.Session)) {
			if outageOnly {
				m.Rules[i].Outage = ""
				if m.Rules[i].Text == "" {
					m.Rules = append(m.Rules[:i], m.Rules[i+1:]...)
				}
			} else {
				m.Rules = append(m.Rules[:i], m.Rules[i+1:]...)
			}
			return true
		}
	}
	return false
}

func merge(old, p Pair) Pair {
	if p.Text != "" {
		old.Text = p.Text
	}
	if p.Outage != "" {
		old.Outage = p.Outage
	}
	return old
}

func same(a, b scope.Entry) bool {
	return a.Session == b.Session && a.Dir == b.Dir && a.Wrapper == b.Wrapper
}

// PruneWrappers drops messages for `continuum run` sessions that ended.
func (m *Messages) PruneWrappers(alive func(id string) bool) bool {
	var keep []Rule
	changed := false
	for _, r := range m.Rules {
		if r.Wrapper != "" && !alive(r.Wrapper) {
			changed = true
			continue
		}
		keep = append(keep, r)
	}
	m.Rules = keep
	return changed
}
