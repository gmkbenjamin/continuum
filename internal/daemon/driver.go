package daemon

import (
	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/opencode"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
	"github.com/continuum-dev/continuum/internal/sources"
	"github.com/continuum-dev/continuum/internal/store"
)

// AgentDriver isolates agent-specific lifecycle and routing quirks that cannot
// be expressed in rules.json alone, keeping the scheduler generic.
type AgentDriver interface {
	// IgnoreScreenEvents reports whether screen-scraped events (wrapper/pane)
	// should be ignored because another authoritative channel owns this agent.
	IgnoreScreenEvents(cfg core.Config) bool
	// IgnoreTranscript reports whether a transcript/session should never be
	// scheduled (for example, Antigravity subagent conversations).
	IgnoreTranscript(path string) bool
	// SanitizeSession clears unreliable route/PID hints on a saved session
	// (for example, Codex's shared app-server inheriting a launcher's pane).
	SanitizeSession(s *store.Session) (loc *core.Location, pid int)
	// SanitizeJob clears unreliable route/PID hints on a job before delivery.
	SanitizeJob(j *core.Job, sess *store.Session)
	// ManagesOwnLifecycle reports whether the job's lifecycle is owned by an
	// external agent server (e.g. OpenCode v2 API) rather than local panes.
	ManagesOwnLifecycle(j *core.Job) bool
	// AllowUniqueWrapperBinding reports whether an unlinked session may be
	// bound to a uniquely matching live wrapper in its working directory.
	AllowUniqueWrapperBinding(h *rules.Harness) bool
	// SkipPaneScan reports whether the tmux pane scanner skips this agent
	// because hooks/transcripts observe it more precisely.
	SkipPaneScan() bool
	// ModelSpecificLimits reports whether an answer on another model should
	// never lift a limit on this agent's model early.
	ModelSpecificLimits() bool
	// AlwaysExactPIDs reports whether hooks always report the exact agent PID.
	AlwaysExactPIDs() bool
}

type defaultDriver struct{}

func (defaultDriver) IgnoreScreenEvents(core.Config) bool { return false }
func (defaultDriver) IgnoreTranscript(string) bool        { return false }
func (defaultDriver) SanitizeSession(s *store.Session) (*core.Location, int) {
	if s == nil {
		return nil, 0
	}
	return s.Location, s.PID
}
func (defaultDriver) SanitizeJob(*core.Job, *store.Session) {}
func (defaultDriver) ManagesOwnLifecycle(*core.Job) bool    { return false }
func (defaultDriver) AllowUniqueWrapperBinding(h *rules.Harness) bool {
	if h == nil || h.LiveSessions == nil {
		return true
	}
	// Harnesses with an authoritative live session list (command or registry)
	// report open sessions directly and should not fall back to folder binding.
	return len(h.LiveSessions.Command) == 0 && h.LiveSessions.RegistryDir == "" && h.LiveSessions.RegistryFile == ""
}
func (defaultDriver) SkipPaneScan() bool        { return false }
func (defaultDriver) ModelSpecificLimits() bool { return false }
func (defaultDriver) AlwaysExactPIDs() bool     { return false }

type claudeDriver struct{ defaultDriver }

func (claudeDriver) SkipPaneScan() bool    { return true }
func (claudeDriver) AlwaysExactPIDs() bool { return true }

type codexDriver struct{ defaultDriver }

func (codexDriver) SkipPaneScan() bool { return true }

func (codexDriver) SanitizeSession(s *store.Session) (*core.Location, int) {
	if s == nil {
		return nil, 0
	}
	if s.PID > 0 && live.CodexAppServer(proc.Args(s.PID)) {
		return nil, 0
	}
	return s.Location, s.PID
}

func (codexDriver) SanitizeJob(j *core.Job, sess *store.Session) {
	if j == nil {
		return
	}
	if sess != nil && sess.PID > 0 && live.CodexAppServer(proc.Args(sess.PID)) && j.Location.String() == sess.Location.String() {
		j.Location = nil
	}
	if j.PID > 0 && live.CodexAppServer(proc.Args(j.PID)) {
		j.Location, j.PID = nil, 0
	}
}

type antigravityDriver struct{ defaultDriver }

func (antigravityDriver) IgnoreTranscript(path string) bool {
	return sources.IsAntigravitySubagent(path)
}
func (antigravityDriver) ModelSpecificLimits() bool { return true }

type opencodeDriver struct{ defaultDriver }

func (opencodeDriver) IgnoreScreenEvents(cfg core.Config) bool {
	return cfg.OpenCodeServer != ""
}

func (opencodeDriver) ManagesOwnLifecycle(j *core.Job) bool {
	return j != nil && j.Location != nil && j.Location.Kind == opencode.LocationKind
}

func (opencodeDriver) AllowUniqueWrapperBinding(*rules.Harness) bool {
	return false
}

// DriverFor returns the agent-specific driver for harness.
func DriverFor(harness string) AgentDriver {
	switch harness {
	case "claude":
		return claudeDriver{}
	case "codex":
		return codexDriver{}
	case "antigravity":
		return antigravityDriver{}
	case "opencode":
		return opencodeDriver{}
	default:
		return defaultDriver{}
	}
}
