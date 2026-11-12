// Package scope decides which sessions Continuum resumes: every session
// (mode "all", the default) or only the ones you chose (mode "chosen").
// Excluded sessions are never resumed in either mode.
//
// An entry names a session id, a directory (every session started in it or
// below it), or a `continuum run --choose` wrapper.
package scope

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
)

const (
	ModeAll    = "all"
	ModeChosen = "chosen"
)

// Entry is one chosen or excluded session, directory or wrapper.
type Entry struct {
	Harness string    `json:"harness,omitempty"`
	Session string    `json:"session,omitempty"`
	Dir     string    `json:"dir,omitempty"`
	Wrapper string    `json:"wrapper,omitempty"`
	Label   string    `json:"label,omitempty"`
	Added   time.Time `json:"added"`
}

func (e Entry) String() string {
	switch {
	case e.Session != "":
		s := e.Session
		if len(s) > 8 {
			s = s[:8]
		}
		if e.Harness != "" {
			s = e.Harness + " session " + s
		}
		if e.Label != "" {
			s += " (" + e.Label + ")"
		}
		return s
	case e.Dir != "":
		return "every session in " + e.Dir
	case e.Wrapper != "":
		s := "continuum run " + e.Wrapper
		if e.Label != "" {
			s += " (" + e.Label + ")"
		}
		return s
	}
	return "?"
}

// Scope is ~/.continuum/scope.json.
type Scope struct {
	Mode     string  `json:"mode"`
	Chosen   []Entry `json:"chosen,omitempty"`
	Excluded []Entry `json:"excluded,omitempty"`
}

func Path(p core.Paths) string { return filepath.Join(p.Home, "scope.json") }

// Load reads the scope; a missing file means "all".
func Load(p core.Paths) (Scope, error) {
	var s Scope
	data, err := os.ReadFile(Path(p))
	if err != nil {
		if os.IsNotExist(err) {
			return Scope{Mode: ModeAll}, nil
		}
		return Scope{Mode: ModeAll}, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return Scope{Mode: ModeAll}, err
	}
	if s.Mode != ModeChosen {
		s.Mode = ModeAll
	}
	return s, nil
}

func (s Scope) Save(p core.Paths) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return core.WriteFileAtomic(Path(p), append(data, '\n'), 0o600)
}

// Covers reports whether a job is in scope, and why not when it is not.
func (s Scope) Covers(j *core.Job) (bool, string) {
	for _, e := range s.Excluded {
		if e.Matches(j) {
			return false, "excluded: " + e.String()
		}
	}
	if s.Mode != ModeChosen {
		return true, ""
	}
	for _, e := range s.Chosen {
		if e.Matches(j) {
			return true, ""
		}
	}
	return false, "not chosen (Continuum is set to resume chosen sessions only)"
}

// Matches reports whether the entry names the job's session.
func (e Entry) Matches(j *core.Job) bool {
	if e.Harness != "" && j.Harness != "" && e.Harness != j.Harness {
		return false
	}
	switch {
	case e.Session != "":
		return j.SessionID != "" && (j.SessionID == e.Session || (len(e.Session) >= 6 && strings.HasPrefix(j.SessionID, e.Session)))
	case e.Dir != "":
		return j.Cwd != "" && within(j.Cwd, e.Dir)
	case e.Wrapper != "":
		return j.Location != nil && j.Location.Kind == "wrapper" && j.Location.Target == e.Wrapper
	}
	return false
}

func within(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	if path == dir {
		return true
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// same reports whether two entries name the same thing.
func same(a, b Entry) bool {
	return a.Session == b.Session && a.Dir == b.Dir && a.Wrapper == b.Wrapper && (a.Harness == b.Harness || a.Harness == "" || b.Harness == "")
}

// Add puts e in list unless it is already there. It returns the new list and
// whether it changed.
func Add(list []Entry, e Entry) ([]Entry, bool) {
	for _, x := range list {
		if same(x, e) {
			return list, false
		}
	}
	if e.Added.IsZero() {
		e.Added = time.Now()
	}
	return append(list, e), true
}

// Remove drops entries naming e (or everything when all is true).
func Remove(list []Entry, e Entry, all bool) ([]Entry, int) {
	if all {
		return nil, len(list)
	}
	var out []Entry
	n := 0
	for _, x := range list {
		if same(x, e) || (e.Session != "" && x.Session != "" && strings.HasPrefix(x.Session, e.Session)) {
			n++
			continue
		}
		out = append(out, x)
	}
	return out, n
}

// PruneWrappers drops wrapper entries whose wrapper is gone.
func (s *Scope) PruneWrappers(alive func(id string) bool) bool {
	changed := false
	keep := func(list []Entry) []Entry {
		var out []Entry
		for _, e := range list {
			if e.Wrapper != "" && !alive(e.Wrapper) {
				changed = true
				continue
			}
			out = append(out, e)
		}
		return out
	}
	s.Chosen, s.Excluded = keep(s.Chosen), keep(s.Excluded)
	return changed
}

var ErrAmbiguous = errors.New("matches more than one session")
