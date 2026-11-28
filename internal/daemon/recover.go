package daemon

import (
	"errors"
	"fmt"
	"strings"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/sources"
)

// CheckCodexRecovery verifies that a legacy detached launch was recorded as
// delivered and the thread transcript shows no later turn.
func CheckCodexRecovery(j *core.Job) error {
	if j == nil {
		return errors.New("no resume record for that Codex thread; see continuum list --all --json")
	}
	if !isLegacyCodexDelivery(j) {
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

func isLegacyCodexDelivery(j *core.Job) bool {
	return j != nil && j.Harness == "codex" && j.State == core.StateDelivered && j.DeliveredVia == "codex-exec-resume" &&
		!j.DeliveredAt.IsZero() && strings.HasPrefix(j.Result, "started ") &&
		!j.ManualResumeOnly && j.Kind != core.KindNeverResume
}

func (d *Daemon) recoverCodex(sel string) {
	if !d.Config.HarnessEnabled("codex") {
		d.logf("%s: Codex is disabled", sel)
		return
	}
	if sel != "all" {
		if err := d.requeueCodexRecovery(d.Jobs[sel]); err != nil {
			d.logf("%s: recovery declined: %v", sel, err)
		}
		return
	}
	queued := 0
	for _, j := range d.Jobs {
		if !isLegacyCodexDelivery(j) {
			continue
		}
		if err := d.requeueCodexRecovery(j); err != nil {
			d.logf("%s: recovery declined: %v", j.Key, err)
			continue
		}
		queued++
	}
	d.logf("recovery: queued %d Codex resume(s)", queued)
}

func (d *Daemon) requeueCodexRecovery(j *core.Job) error {
	if err := CheckCodexRecovery(j); err != nil {
		return err
	}
	if ok, why := d.Scope.Covers(j); !ok {
		return errors.New(why)
	}
	now := d.Now()
	markJobRequeued(j, now)
	d.history("recover", j, nil)
	d.dirty = true
	return nil
}
