package daemon

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/sources"
)

// CheckCodexRecovery verifies that a legacy detached launch was recorded as
// delivered and the thread transcript shows no later turn.
func CheckCodexRecovery(j *core.Job) error {
	if j == nil {
		return errors.New("no resume record for that Codex thread; see continuum list --all --json")
	}
	if j.Harness != "codex" || j.State != core.StateDelivered || j.DeliveredVia != "codex-exec-resume" ||
		j.DeliveredAt.IsZero() || !strings.HasPrefix(j.Result, "started ") ||
		j.ManualResumeOnly || j.Kind == core.KindNeverResume {
		return errors.New("this record does not show an unverified Codex background launch")
	}
	if j.SessionID == "" || sources.CodexSessionFromPath(j.TranscriptPath) != j.SessionID {
		return errors.New("the transcript does not identify this Codex thread")
	}
	started, err := sources.CodexTurnStartedSince(j.TranscriptPath, j.TranscriptSize, j.DeliveredAt)
	if err != nil {
		return fmt.Errorf("cannot verify the Codex transcript: %w", err)
	}
	if started {
		return errors.New("the Codex thread already started a turn after the previous attempt")
	}
	return nil
}

func (d *Daemon) recoverCodex(key string) {
	j := d.Jobs[key] // require the exact key; never match a prefix or "all"
	if !d.Config.HarnessEnabled("codex") {
		d.logf("%s: Codex is disabled", key)
		return
	}
	if err := CheckCodexRecovery(j); err != nil {
		d.logf("%s: recovery declined: %v", key, err)
		return
	}
	if ok, why := d.Scope.Covers(j); !ok {
		d.logf("%s: recovery declined: %s", key, why)
		return
	}
	now := d.Now()
	j.State, j.Result, j.DueAt, j.UpdatedAt = core.StatePending, "", now, now
	j.DeliveredVia, j.DeliveredAt, j.WaitingOpen = "", time.Time{}, false
	d.history("recover", j, nil)
	d.dirty = true
}
