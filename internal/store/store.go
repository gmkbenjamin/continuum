// Package store persists Continuum's jobs, known sessions and tailer offsets
// as small JSON files. Only the daemon writes them.
package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
)

// Session is what Continuum knows about one agent session.
type Session struct {
	Harness        string         `json:"harness"`
	SessionID      string         `json:"session_id"`
	Cwd            string         `json:"cwd,omitempty"`
	TranscriptPath string         `json:"transcript_path,omitempty"`
	Location       *core.Location `json:"location,omitempty"`
	PID            int            `json:"pid,omitempty"`
	Entrypoint     string         `json:"entrypoint,omitempty"`
	Ended          bool           `json:"ended,omitempty"`
	// EndedAt: when it ended. An event from before that (delivered late)
	// doesn't reopen it.
	EndedAt  time.Time `json:"ended_at,omitzero"`
	LastSeen time.Time `json:"last_seen"`
	// TurnAt: when the session's latest turn started (a prompt-submit hook).
	// Model: the model it last used, when its hooks say.
	TurnAt time.Time `json:"turn_at,omitzero"`
	Model  string    `json:"model,omitempty"`
}

// Instance represents a running terminal/wrapper hosting an agent process,
// distinct from a Conversation (Session). One Instance may host multiple
// conversations over its lifetime (/clear, /resume), and one conversation may
// be attached to 0, 1, or N instances.
type Instance struct {
	ID              string         `json:"id"`
	Harness         string         `json:"harness"`
	Location        *core.Location `json:"location,omitempty"`
	Cwd             string         `json:"cwd,omitempty"`
	Argv            []string       `json:"argv,omitempty"`
	PID             int            `json:"pid,omitempty"`
	StartedAt       time.Time      `json:"started_at,omitzero"`
	ActiveSessionID string         `json:"active_session_id,omitempty"`
	UpdatedAt       time.Time      `json:"updated_at,omitzero"`
}

func SessionKey(harness, id string) string { return harness + ":" + id }

func LoadJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, v)
}

func SaveJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return core.WriteFileAtomic(path, data, 0o600)
}

func LoadJobs(path string) (map[string]*core.Job, error) {
	jobs := map[string]*core.Job{}
	err := LoadJSON(path, &jobs)
	if jobs == nil {
		jobs = map[string]*core.Job{}
	}
	return jobs, err
}

// SortedJobs returns jobs, pending first and then by due time.
func SortedJobs(jobs map[string]*core.Job) []*core.Job {
	out := make([]*core.Job, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, j)
	}
	sort.Slice(out, func(i, k int) bool {
		pi, pk := out[i].State == core.StatePending, out[k].State == core.StatePending
		if pi != pk {
			return pi
		}
		if pi {
			return out[i].DueAt.Before(out[k].DueAt)
		}
		return out[i].UpdatedAt.After(out[k].UpdatedAt)
	})
	return out
}

// AppendHistory records an action in history.jsonl.
func AppendHistory(path string, entry map[string]any) {
	entry["time"] = time.Now().Format(time.RFC3339)
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// WithLock acquires an exclusive lock on path (retrying ErrLocked up to wait),
// runs fn, and releases the lock.
func WithLock(path string, wait time.Duration, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	deadline := time.Now().Add(wait)
	for {
		f, err := Lock(path)
		if err == nil {
			defer f.Close()
			return fn()
		}
		if !errors.Is(err, ErrLocked) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(10 * time.Millisecond)
	}
}
