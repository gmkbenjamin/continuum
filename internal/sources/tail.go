// Package sources watches the files agents already write (Claude Code
// transcripts, Codex rollouts) and turns limit errors into events. This works
// for terminal and desktop sessions alike and needs no hooks.
package sources

import (
	"bufio"
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Tailer remembers how far each file has been read.
type Tailer struct {
	Offsets map[string]int64 `json:"offsets"`
	// FirstSeen bounds how much of a file seen for the first time is read.
	InitialBytes int64 `json:"-"`
}

func NewTailer() *Tailer {
	return &Tailer{Offsets: map[string]int64{}, InitialBytes: 512 * 1024}
}

// Line is one complete JSONL line with the file offset just after it.
type Line struct {
	Path   string
	Data   []byte
	EndPos int64
}

// Scan walks root for *.jsonl files modified after since and returns the new
// complete lines in each, grouped by file.
func (t *Tailer) Scan(root string, since time.Time, maxDepth int) map[string][]Line {
	out := map[string][]Line{}
	rootDepth := strings.Count(filepath.Clean(root), string(os.PathSeparator))
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && path != root {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if maxDepth > 0 && strings.Count(filepath.Clean(path), string(os.PathSeparator))-rootDepth >= maxDepth {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.ModTime().Before(since) {
			return nil
		}
		lines := t.readNew(path, info.Size())
		if len(lines) > 0 {
			out[path] = lines
		}
		return nil
	})
	t.prune(since)
	return out
}

func (t *Tailer) readNew(path string, size int64) []Line {
	off, seen := t.Offsets[path]
	if !seen {
		off = size - t.InitialBytes
		if off < 0 {
			off = 0
		}
	}
	if size < off {
		off = 0 // truncated or replaced
	}
	if size == off {
		t.Offsets[path] = off
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return nil
	}
	r := bufio.NewReaderSize(io.LimitReader(f, size-off), 256*1024)
	pos := off
	var lines []Line
	first := !seen && off > 0
	for {
		data, err := r.ReadBytes('\n')
		if err != nil {
			// Incomplete last line: read it next time.
			break
		}
		pos += int64(len(data))
		if first {
			// We started mid-file; the first piece is a partial line.
			first = false
			continue
		}
		data = bytes.TrimSpace(data)
		if len(data) == 0 {
			continue
		}
		lines = append(lines, Line{Path: path, Data: data, EndPos: pos})
	}
	t.Offsets[path] = pos
	return lines
}

// prune forgets files that have not changed for a long time.
func (t *Tailer) prune(since time.Time) {
	if len(t.Offsets) < 2000 {
		return
	}
	for p := range t.Offsets {
		if st, err := os.Stat(p); err != nil || st.ModTime().Before(since) {
			delete(t.Offsets, p)
		}
	}
}

// ReadFrom returns complete lines of path after offset (used to check a
// session for activity after a limit).
func ReadFrom(path string, offset int64) []Line {
	st, err := os.Stat(path)
	if err != nil || st.Size() <= offset {
		return nil
	}
	t := &Tailer{Offsets: map[string]int64{path: offset}}
	return t.readNew(path, st.Size())
}

func parseTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}
