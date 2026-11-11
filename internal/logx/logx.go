// Package logx is Continuum's logger: four levels, a timestamped format for
// the log file and a short, readable one for a terminal.
//
//	error  something failed and needs attention
//	warn   something unexpected; Continuum carried on safely
//	info   what Continuum did (scheduled, resumed, skipped, ...): the default
//	debug  why: every event, check and delivery route, for troubleshooting
package logx

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Level int

const (
	Debug Level = iota
	Info
	Warn
	Error
)

func (l Level) String() string {
	return [...]string{"debug", "info", "warn", "error"}[l]
}

// ParseLevel accepts debug, info, warn(ing), error; anything else is info.
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug", "trace", "verbose":
		return Debug
	case "warn", "warning":
		return Warn
	case "error":
		return Error
	}
	return Info
}

// LevelFromEnv returns the level set by CONTINUUM_DEBUG / CONTINUUM_LOG_LEVEL,
// or def.
func LevelFromEnv(def Level) Level {
	if v := os.Getenv("CONTINUUM_DEBUG"); v != "" && v != "0" && v != "false" {
		return Debug
	}
	if v := os.Getenv("CONTINUUM_LOG_LEVEL"); v != "" {
		return ParseLevel(v)
	}
	return def
}

// Sink is one output.
type Sink struct {
	W     io.Writer
	Level Level
	Human bool // short terminal format instead of the log-file format
	Color bool
}

type Logger struct {
	mu    sync.Mutex
	sinks []Sink
	now   func() time.Time
}

func New(sinks ...Sink) *Logger { return &Logger{sinks: sinks, now: time.Now} }

// Enabled reports whether any sink takes messages at level l.
func (lg *Logger) Enabled(l Level) bool {
	if lg == nil {
		return false
	}
	for _, s := range lg.sinks {
		if l >= s.Level {
			return true
		}
	}
	return false
}

func (lg *Logger) log(l Level, format string, args ...any) {
	if lg == nil {
		return
	}
	msg := fmt.Sprintf(format, args...)
	t := lg.now()
	lg.mu.Lock()
	defer lg.mu.Unlock()
	for _, s := range lg.sinks {
		if l < s.Level {
			continue
		}
		if s.Human {
			tag := ""
			switch l {
			case Debug:
				tag = "debug "
			case Warn:
				tag = "warning: "
			case Error:
				tag = "error: "
			}
			line := t.Format("15:04:05") + "  " + tag + msg
			if s.Color {
				switch l {
				case Debug:
					line = "\x1b[2m" + line + "\x1b[0m"
				case Warn:
					line = "\x1b[33m" + line + "\x1b[0m"
				case Error:
					line = "\x1b[31m" + line + "\x1b[0m"
				}
			}
			fmt.Fprintln(s.W, line)
		} else {
			fmt.Fprintf(s.W, "%s %-5s %s\n", t.Format("2006-01-02 15:04:05.000"), strings.ToUpper(l.String()), msg)
		}
	}
}

func (lg *Logger) Debugf(format string, args ...any) { lg.log(Debug, format, args...) }
func (lg *Logger) Infof(format string, args ...any)  { lg.log(Info, format, args...) }
func (lg *Logger) Warnf(format string, args ...any)  { lg.log(Warn, format, args...) }
func (lg *Logger) Errorf(format string, args ...any) { lg.log(Error, format, args...) }

// File is an append-only log file that rotates to <name>.1 at maxBytes, so
// debug logging can't fill the disk.
type File struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	f        *os.File
	size     int64
}

func OpenFile(path string, maxBytes int64) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lf := &File{path: path, maxBytes: maxBytes}
	return lf, lf.open()
}

func (lf *File) open() error {
	f, err := os.OpenFile(lf.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, _ := f.Stat()
	lf.f, lf.size = f, 0
	if st != nil {
		lf.size = st.Size()
	}
	return nil
}

func (lf *File) Write(p []byte) (int, error) {
	lf.mu.Lock()
	defer lf.mu.Unlock()
	if lf.maxBytes > 0 && lf.size+int64(len(p)) > lf.maxBytes {
		lf.f.Close()
		_ = os.Rename(lf.path, lf.path+".1")
		if err := lf.open(); err != nil {
			return 0, err
		}
	}
	n, err := lf.f.Write(p)
	lf.size += int64(n)
	return n, err
}

func (lf *File) Close() error { return lf.f.Close() }
