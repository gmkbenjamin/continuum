package deliver

import (
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/opencode"
	"github.com/continuum-dev/continuum/internal/rules"
)

// nativeDriver isolates agent-specific native delivery paths (shared servers
// and confirmed detached launches) from the generic native command runner.
type nativeDriver interface {
	// PreCommand runs an agent's direct API/server delivery before falling
	// back to CLI commands. handled=false continues to NativeCommands.
	PreCommand(o Options, job *core.Job) (res Result, handled bool, err error)
	// StartDetached handles detached launches that confirm turn start before
	// returning. handled=false uses generic proc.StartDetached.
	StartDetached(nc rules.NativeCommand, argv []string, dir string, env []string, logDir string, job *core.Job) (detail string, handled bool, err error)
}

type defaultNativeDriver struct{}

func (defaultNativeDriver) PreCommand(Options, *core.Job) (Result, bool, error) {
	return Result{}, false, nil
}
func (defaultNativeDriver) StartDetached(rules.NativeCommand, []string, string, []string, string, *core.Job) (string, bool, error) {
	return "", false, nil
}

type opencodeNativeDriver struct{ defaultNativeDriver }

func (opencodeNativeDriver) PreCommand(o Options, job *core.Job) (Result, bool, error) {
	if job.Location != nil && job.Location.Kind == opencode.LocationKind {
		res, err := viaOpenCode(o, job)
		return res, true, err
	}
	return Result{}, false, nil
}

type codexNativeDriver struct{ defaultNativeDriver }

func (codexNativeDriver) PreCommand(o Options, job *core.Job) (Result, bool, error) {
	if job.SessionID == "" {
		return Result{}, false, nil
	}
	res, err := resumeCodexServer(o, job)
	if err == errSkip {
		return Result{}, false, nil
	}
	return res, true, err
}

func (codexNativeDriver) StartDetached(nc rules.NativeCommand, argv []string, dir string, env []string, logDir string, job *core.Job) (string, bool, error) {
	if nc.Name == "codex-exec-resume" {
		detail, err := startCodexExec(argv, dir, env, logDir, job.SessionID, 10*time.Second)
		return detail, true, err
	}
	return "", false, nil
}

func nativeDriverFor(harness string) nativeDriver {
	switch harness {
	case "opencode":
		return opencodeNativeDriver{}
	case "codex":
		return codexNativeDriver{}
	default:
		return defaultNativeDriver{}
	}
}
