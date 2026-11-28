package daemon

import (
	"time"

	"github.com/continuum-dev/continuum/internal/core"
)

// JobPhase is the single, unambiguous lifecycle phase of a Job, replacing
// ad-hoc combinations of State + WaitingOpen + StaleCopyPID + TurnFailed.
type JobPhase string

const (
	PhaseNone             JobPhase = ""
	PhaseScheduled        JobPhase = "scheduled"
	PhaseWaitingOpen      JobPhase = "waiting_open"
	PhaseWaitingStaleCopy JobPhase = "waiting_stale_copy"
	PhaseDelivered        JobPhase = "delivered"
	PhaseTurnFailed       JobPhase = "turn_failed"
	PhaseNeedsUser        JobPhase = "needs_user"
	PhaseIgnored          JobPhase = "ignored"
	PhaseSkipped          JobPhase = "skipped"
	PhaseFailed           JobPhase = "failed"
	PhaseCancelled        JobPhase = "cancelled"
)

// JobPhaseOf derives the canonical phase of j.
func JobPhaseOf(j *core.Job) JobPhase {
	if j == nil {
		return PhaseNone
	}
	switch j.State {
	case core.StatePending:
		if j.StaleCopyPID > 0 {
			return PhaseWaitingStaleCopy
		}
		if j.WaitingOpen {
			return PhaseWaitingOpen
		}
		return PhaseScheduled
	case core.StateDelivered:
		if j.TurnFailed != "" {
			return PhaseTurnFailed
		}
		return PhaseDelivered
	case core.StateNeedsUser:
		return PhaseNeedsUser
	case core.StateIgnored:
		return PhaseIgnored
	case core.StateSkipped:
		return PhaseSkipped
	case core.StateFailed:
		return PhaseFailed
	case core.StateCancelled:
		return PhaseCancelled
	default:
		return PhaseNone
	}
}

// Explicit job state transition helpers keep mutually-interacting flags
// (WaitingOpen, StaleCopyPID, WaitingSince, TurnFailed, ResumeSettled)
// consistent whenever a job changes state.

func clearJobWait(j *core.Job) {
	j.WaitingOpen = false
	j.StaleCopyPID = 0
	j.WaitingSince = time.Time{}
}

func markJobPending(j *core.Job, dueAt, now time.Time) {
	clearJobWait(j)
	j.State = core.StatePending
	j.Result = ""
	j.DueAt = dueAt
	j.UpdatedAt = now
}

func markJobResumeNow(j *core.Job, now time.Time) {
	markJobPending(j, now, now)
	j.TurnFailed = ""
	j.TurnFailedAt = time.Time{}
	j.ResumeSettled = false
}

func markJobRequeued(j *core.Job, now time.Time) {
	clearJobWait(j)
	j.State = core.StatePending
	j.Result = ""
	j.DueAt = now
	j.UpdatedAt = now
	j.DeliveredVia = ""
	j.DeliveredAt = time.Time{}
	j.TurnFailed = ""
	j.TurnFailedAt = time.Time{}
	j.ResumeSettled = false
}

// markJobWaitingOpen puts j into PhaseWaitingOpen and reports whether this
// began a new wait period.
func markJobWaitingOpen(j *core.Job, reason string, now time.Time) bool {
	first := !j.WaitingOpen
	if first {
		j.WaitingOpen = true
		j.WaitingSince = now
	}
	j.StaleCopyPID = 0
	j.Result = reason
	j.UpdatedAt = now
	return first
}

// markJobWaitingStaleCopy puts j into PhaseWaitingStaleCopy and reports
// whether this began a new stale-copy wait period.
func markJobWaitingStaleCopy(j *core.Job, pid int, reason string, now time.Time) bool {
	first := j.StaleCopyPID == 0
	if first {
		j.WaitingOpen = true
		j.WaitingSince = now
	}
	j.StaleCopyPID = pid
	j.Result = reason
	j.UpdatedAt = now
	return first
}

func markJobSkipped(j *core.Job, reason string, now time.Time) {
	clearJobWait(j)
	j.State = core.StateSkipped
	j.Result = reason
	j.UpdatedAt = now
}

func markJobFailed(j *core.Job, reason string, now time.Time) {
	clearJobWait(j)
	j.State = core.StateFailed
	j.Result = reason
	j.UpdatedAt = now
}

func markJobNeedsUser(j *core.Job, reason string, manualOnly bool, now time.Time) {
	clearJobWait(j)
	j.State = core.StateNeedsUser
	j.Result = reason
	if manualOnly {
		j.ManualResumeOnly = true
	}
	j.UpdatedAt = now
}

func markJobIgnored(j *core.Job, reason string, now time.Time) {
	clearJobWait(j)
	j.State = core.StateIgnored
	j.Result = reason
	j.UpdatedAt = now
}

func markJobCancelled(j *core.Job, reason string, now time.Time) {
	clearJobWait(j)
	j.State = core.StateCancelled
	j.Result = reason
	j.UpdatedAt = now
}

func markJobDelivered(j *core.Job, via, detail string, now time.Time) {
	clearJobWait(j)
	j.State = core.StateDelivered
	j.TurnFailed = ""
	j.TurnFailedAt = time.Time{}
	j.ResumeSettled = false
	j.DeliveredAt = now
	j.DeliveredVia = via
	j.Result = detail
	j.UpdatedAt = now
}

func markJobTurnFailed(j *core.Job, errText string, at, now time.Time) {
	j.TurnFailed = errText
	j.TurnFailedAt = at
	j.ResumeSettled = true
	j.UpdatedAt = now
}

func markJobSettled(j *core.Job) {
	j.ResumeSettled = true
}

func clearJobTurnFailed(j *core.Job) {
	j.TurnFailed = ""
	j.TurnFailedAt = time.Time{}
}
