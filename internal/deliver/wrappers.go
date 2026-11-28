package deliver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/proc"
)

// WrapperInfo describes one live `continuum run` PTY wrapper instance.
type WrapperInfo struct {
	ID      string    `json:"id"`
	Harness string    `json:"harness"`
	Cwd     string    `json:"cwd,omitempty"`
	Argv    []string  `json:"argv,omitempty"`
	Started time.Time `json:"started,omitzero"`
	PID     int       `json:"pid,omitempty"`
}

// ReadWrapperInfo reads one live wrapper's metadata by ID.
func ReadWrapperInfo(p core.Paths, id string) (WrapperInfo, bool) {
	if id == "" {
		return WrapperInfo{}, false
	}
	dir := WrapperDir(p, id)
	pidBytes, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		return WrapperInfo{}, false
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if pid <= 0 || !proc.Alive(pid) {
		return WrapperInfo{}, false
	}
	var meta struct {
		Harness string    `json:"harness"`
		Cwd     string    `json:"cwd"`
		Argv    []string  `json:"argv"`
		Started time.Time `json:"started"`
	}
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil || json.Unmarshal(data, &meta) != nil || meta.Harness == "" {
		return WrapperInfo{}, false
	}
	return WrapperInfo{
		ID:      id,
		Harness: meta.Harness,
		Cwd:     meta.Cwd,
		Argv:    meta.Argv,
		Started: meta.Started,
		PID:     pid,
	}, true
}

// LiveWrappers returns every currently alive wrapper under p.Wrappers, sorted
// oldest first by start time (then ID for determinism).
func LiveWrappers(p core.Paths) []WrapperInfo {
	entries, err := os.ReadDir(p.Wrappers)
	if err != nil {
		return nil
	}
	var out []WrapperInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if w, ok := ReadWrapperInfo(p, e.Name()); ok {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, k int) bool {
		if !out[i].Started.Equal(out[k].Started) {
			return out[i].Started.Before(out[k].Started)
		}
		return out[i].ID < out[k].ID
	})
	return out
}
