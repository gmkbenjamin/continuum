// Package core holds Continuum's shared types, paths and configuration.
package core

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

var Version = "0.6.0"

// Location says where an agent is running so input can be delivered to it
// without focusing any window.
type Location struct {
	Kind   string            `json:"kind"`             // tmux, screen, zellij, herdr, rex, wezterm, kitty, wrapper
	Target string            `json:"target"`           // pane / block / window / wrapper id
	Socket string            `json:"socket,omitempty"` // server socket or session name
	Extra  map[string]string `json:"extra,omitempty"`
}

func (l *Location) String() string {
	if l == nil || l.Kind == "" {
		return "-"
	}
	s := l.Kind + ":" + l.Target
	if l.Socket != "" {
		s += "@" + l.Socket
	}
	return s
}

// Event kinds.
const (
	KindUsageLimit   = "usage_limit"
	KindTransient    = "transient"
	KindSessionStart = "session_start"
	KindSessionEnd   = "session_end"
	KindActivity     = "activity"      // the session did something after a limit
	KindNativeGaveUp = "native_gaveup" // the agent says it will not resume by itself
	KindNeverResume  = "never_resume"  // needs a human (billing, auth, spend cap)
)

// Event is something an observer saw. Hooks, transcript tailers, the PTY
// wrapper and the pane scanner all produce events; the daemon consumes them.
type Event struct {
	ID             string    `json:"id"`
	Harness        string    `json:"harness"`
	Kind           string    `json:"kind"`
	SessionID      string    `json:"session_id,omitempty"`
	Cwd            string    `json:"cwd,omitempty"`
	TranscriptPath string    `json:"transcript_path,omitempty"`
	Message        string    `json:"message,omitempty"`
	ResetAt        time.Time `json:"reset_at,omitempty"`
	At             time.Time `json:"at"`
	Source         string    `json:"source"`
	Location       *Location `json:"location,omitempty"`
	PID            int       `json:"pid,omitempty"`
	Entrypoint     string    `json:"entrypoint,omitempty"` // cli, desktop, ide, ...
	TranscriptSize int64     `json:"transcript_size,omitempty"`
}

// Job states.
const (
	StatePending   = "pending"
	StateDelivered = "delivered"
	StateSkipped   = "skipped"
	StateFailed    = "failed"
	StateCancelled = "cancelled"
	StateNeedsUser = "needs_user"
	// StateIgnored: outside the scope (not chosen, or excluded). Choosing the
	// session later schedules it again.
	StateIgnored = "not_chosen"
)

// Job is a scheduled resume.
type Job struct {
	Key            string    `json:"key"`
	Harness        string    `json:"harness"`
	SessionID      string    `json:"session_id,omitempty"`
	Cwd            string    `json:"cwd,omitempty"`
	TranscriptPath string    `json:"transcript_path,omitempty"`
	TranscriptSize int64     `json:"transcript_size,omitempty"`
	Location       *Location `json:"location,omitempty"`
	PID            int       `json:"pid,omitempty"`
	Entrypoint     string    `json:"entrypoint,omitempty"`
	Kind           string    `json:"kind"`
	Message        string    `json:"message,omitempty"`
	ResetAt        time.Time `json:"reset_at,omitempty"`
	ResetKnown     bool      `json:"reset_known"`
	NativeGaveUp   bool      `json:"native_gave_up,omitempty"`
	LimitAt        time.Time `json:"limit_at"`
	DueAt          time.Time `json:"due_at"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Attempts       int       `json:"attempts"`
	State          string    `json:"state"`
	Result         string    `json:"result,omitempty"`
	Sources        []string  `json:"sources,omitempty"`
	DeliveredAt    time.Time `json:"delivered_at,omitempty"`
	DeliveredVia   string    `json:"delivered_via,omitempty"`
	// WaitingOpen: the session is open somewhere Continuum can't type; it is
	// re-checked every couple of minutes (the user was told once).
	WaitingOpen bool `json:"waiting_open,omitempty"`
}

// Paths are Continuum's files under its home directory.
type Paths struct {
	Home      string
	Config    string
	Rules     string
	Spool     string
	Jobs      string
	Sessions  string
	Offsets   string
	History   string
	Log       string
	Lock      string
	PID       string
	Wrappers  string
	BinRecord string
}

// HomeDir returns $CONTINUUM_HOME or ~/.continuum.
func HomeDir() string {
	if h := os.Getenv("CONTINUUM_HOME"); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		home = os.TempDir()
	}
	return filepath.Join(home, ".continuum")
}

func GetPaths() Paths {
	h := HomeDir()
	return Paths{
		Home:      h,
		Config:    filepath.Join(h, "config.json"),
		Rules:     filepath.Join(h, "rules.json"),
		Spool:     filepath.Join(h, "spool"),
		Jobs:      filepath.Join(h, "jobs.json"),
		Sessions:  filepath.Join(h, "sessions.json"),
		Offsets:   filepath.Join(h, "offsets.json"),
		History:   filepath.Join(h, "history.jsonl"),
		Log:       filepath.Join(h, "daemon.log"),
		Lock:      filepath.Join(h, "daemon.lock"),
		PID:       filepath.Join(h, "daemon.pid"),
		Wrappers:  filepath.Join(h, "wrappers"),
		BinRecord: filepath.Join(h, "bin-path"),
	}
}

func (p Paths) Ensure() error {
	for _, d := range []string{p.Home, p.Spool, p.Wrappers} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	return nil
}

// Config is the user's configuration (~/.continuum/config.json). Every field
// is optional.
type Config struct {
	Enabled           *bool             `json:"enabled,omitempty"`
	DryRun            bool              `json:"dry_run,omitempty"`
	Harnesses         map[string]bool   `json:"harnesses,omitempty"` // enable/disable per harness
	ResumePrompts     map[string]string `json:"resume_prompts,omitempty"`
	DelayAfterReset   *int              `json:"delay_after_reset_seconds,omitempty"`
	NativeGrace       map[string]int    `json:"native_grace_seconds,omitempty"`
	MaxWaitHours      *int              `json:"max_wait_hours,omitempty"`
	CatchUpHours      *int              `json:"catch_up_hours,omitempty"`
	Strategies        []string          `json:"strategies,omitempty"` // order: wrapper, multiplexer, terminal-app, native
	AllowHeadless     *bool             `json:"allow_headless,omitempty"`
	WatchTranscripts  *bool             `json:"watch_transcripts,omitempty"`
	ScanPanes         *bool             `json:"scan_panes,omitempty"`
	Notify            *bool             `json:"notify,omitempty"`
	PollSeconds       int               `json:"poll_seconds,omitempty"`
	ClaudeHome        string            `json:"claude_home,omitempty"`
	CodexHome         string            `json:"codex_home,omitempty"`
	ResetHintsCommand []string          `json:"reset_hints_command,omitempty"`
	Binaries          map[string]string `json:"binaries,omitempty"`
	AutostartDaemon   *bool             `json:"autostart_daemon,omitempty"`
	// AllowCopies lets Continuum continue a session with the agent's CLI even
	// while the session is still open elsewhere (which starts a second copy).
	// Off by default: Continuum only ever continues the original session.
	AllowCopies *bool `json:"allow_copies,omitempty"`
	// KeepAwakeHours: hold off idle sleep while a resume is due within this
	// many hours, and while a resumed session keeps working. 0 turns it off.
	KeepAwakeHours *int `json:"keep_awake_hours,omitempty"`
	// ShellHookAgents replaces the list of agents the shell hook wraps
	// (default: every installed agent whose rules allow it).
	ShellHookAgents []string `json:"shell_hook_agents,omitempty"`
	// LogLevel: error, warn, info (default) or debug. Debug explains every
	// decision in daemon.log, and hooks write one line each to hooks.log.
	LogLevel string `json:"log_level,omitempty"`
}

// Debug reports whether debug logging is on (config or CONTINUUM_DEBUG).
func (c Config) Debug() bool {
	if v := os.Getenv("CONTINUUM_DEBUG"); v != "" && v != "0" && v != "false" {
		return true
	}
	return c.LogLevel == "debug" || os.Getenv("CONTINUUM_LOG_LEVEL") == "debug"
}

func LoadConfig(path string) (Config, error) {
	var c Config
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return c, nil
		}
		return c, err
	}
	err = json.Unmarshal(data, &c)
	return c, err
}

func b(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func i(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func (c Config) IsEnabled() bool          { return b(c.Enabled, true) }
func (c Config) HeadlessAllowed() bool    { return b(c.AllowHeadless, true) }
func (c Config) TranscriptsWatched() bool { return b(c.WatchTranscripts, true) }
func (c Config) PanesScanned() bool       { return b(c.ScanPanes, true) }
func (c Config) NotifyEnabled() bool      { return b(c.Notify, true) }
func (c Config) AutostartEnabled() bool   { return b(c.AutostartDaemon, true) }
func (c Config) CopiesAllowed() bool      { return b(c.AllowCopies, false) }
func (c Config) KeepAwake() time.Duration { return time.Duration(i(c.KeepAwakeHours, 8)) * time.Hour }
func (c Config) MaxWait() time.Duration   { return time.Duration(i(c.MaxWaitHours, 24*8)) * time.Hour }
func (c Config) CatchUp() time.Duration   { return time.Duration(i(c.CatchUpHours, 12)) * time.Hour }
func (c Config) HarnessEnabled(h string) bool {
	if v, ok := c.Harnesses[h]; ok {
		return v
	}
	return true
}

func (c Config) StrategyOrder() []string {
	if len(c.Strategies) > 0 {
		return c.Strategies
	}
	return []string{"wrapper", "multiplexer", "terminal-app", "native"}
}

func (c Config) Poll() time.Duration {
	if c.PollSeconds > 0 {
		return time.Duration(c.PollSeconds) * time.Second
	}
	return 5 * time.Second
}

func (c Config) ClaudeDir() string {
	if c.ClaudeHome != "" {
		return c.ClaudeHome
	}
	if v := os.Getenv("CLAUDE_CONFIG_DIR"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

func (c Config) CodexDir() string {
	if c.CodexHome != "" {
		return c.CodexHome
	}
	if v := os.Getenv("CODEX_HOME"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

// NewID returns a short random id.
func NewID() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return hex.EncodeToString(buf[:])
}

// WriteFileAtomic writes data to path via a temp file and rename.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	_ = os.Chmod(tmp, perm)
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// SpoolEvent drops an event into the spool directory for the daemon. It is
// safe to call from many processes at once.
func SpoolEvent(p Paths, e Event) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	name := filepath.Join(p.Spool, e.At.UTC().Format("20060102T150405.000000000")+"-"+e.ID+".json")
	return WriteFileAtomic(name, data, 0o600)
}
