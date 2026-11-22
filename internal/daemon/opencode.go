package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/opencode"
	"github.com/continuum-dev/continuum/internal/resettime"
)

// pollOpenCode watches final outcomes in the recent top-level sessions on
// the configured shared server. A running session may still be retrying.
func (d *Daemon) pollOpenCode(now time.Time) error {
	client, err := opencode.New(d.Config.OpenCodeServer)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	sessions, err := client.Sessions(ctx)
	if err != nil || len(sessions) == 0 {
		return err
	}
	active, err := client.Active(ctx)
	if err != nil {
		return err
	}
	cutoff := now.Add(-d.Config.CatchUp()).UnixMilli()
	var scanErr error
	for _, session := range sessions {
		if _, busy := active[session.ID]; busy || session.ParentID != "" ||
			session.Time.Archived != 0 || session.Time.Updated < cutoff {
			continue
		}
		last, err := client.Latest(ctx, session.ID)
		if err != nil {
			if scanErr == nil {
				scanErr = fmt.Errorf("session %s: %w", session.ID, err)
			}
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if last.ID == "" || last.Time.Created < cutoff {
			continue
		}
		at := time.UnixMilli(last.Time.Created)
		ev := core.Event{
			Harness: "opencode", SessionID: session.ID, Cwd: session.Location.Directory,
			At: at, Source: opencode.LocationKind, Entrypoint: "shared-server",
			Location: &core.Location{
				Kind: opencode.LocationKind, Target: session.ID, Socket: client.Endpoint(),
				Extra: map[string]string{"failed_message": last.ID},
			},
		}
		j := d.Jobs[jobKey(ev)]
		if j != nil && !at.After(j.LimitAt) {
			continue
		}
		if last.Failed() {
			ev.Kind, ev.Message = d.openCodeFailure(last.Error), last.Error.Message
			if ev.Kind == core.KindUsageLimit {
				ev.ResetAt, _ = resettime.Parse(ev.Message, at, d.Local)
				if delay := last.Error.RetryAfterMS; delay > 0 && delay <= (30*24*time.Hour).Milliseconds() {
					headerReset := at.Add(time.Duration(delay) * time.Millisecond)
					if headerReset.After(ev.ResetAt) {
						ev.ResetAt = headerReset
					}
				}
			}
		}
		if ev.Kind == "" {
			if j == nil || j.State != core.StatePending {
				continue
			}
			ev.Kind = core.KindActivity
		}
		d.Handle(ev)
	}
	return scanErr
}

func (d *Daemon) openCodeFailure(f *opencode.Failure) string {
	h := d.Rules.Harness("opencode")
	if h.NeverResume(f.Message) || f.Status == 401 || f.Status == 402 || f.Status == 403 {
		return core.KindNeverResume
	}
	if f.Status == 429 && f.RetryAfterMS > 0 {
		return core.KindUsageLimit
	}
	if m := h.Classify(f.Message, 4); m != nil {
		return m.Kind
	}
	switch {
	case f.Status == 429 || f.Type == "provider.rate-limit":
		if f.RetryAfterMS > 0 {
			return core.KindUsageLimit
		}
		return core.KindTransient
	case f.Status == 500 || f.Status == 502 || f.Status == 503 || f.Status == 504 || f.Status == 529:
		return core.KindTransient
	}
	return ""
}
