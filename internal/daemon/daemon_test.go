package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/deliver"
	"github.com/continuum-dev/continuum/internal/live"
	"github.com/continuum-dev/continuum/internal/mux"
	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/scope"
)

type harnessT struct {
	d         *Daemon
	now       time.Time
	delivered []*core.Job
	fail      error
	claude    string
	codex     string
	live      *live.State
}

func setup(t *testing.T) *harnessT {
	t.Helper()
	home := t.TempDir()
	h := &harnessT{now: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	h.claude = filepath.Join(home, "claude")
	h.codex = filepath.Join(home, "codex")
	os.MkdirAll(filepath.Join(h.claude, "projects", "-work-app"), 0o700)
	os.MkdirAll(filepath.Join(h.codex, "sessions", "2026", "11", "11"), 0o700)
	t.Setenv("CONTINUUM_HOME", filepath.Join(home, "continuum"))
	p := core.GetPaths()
	cfg := `{"claude_home": "` + h.claude + `", "codex_home": "` + h.codex + `", "notify": false, "scan_panes": false, "poll_seconds": 1}`
	os.MkdirAll(p.Home, 0o700)
	os.WriteFile(p.Config, []byte(cfg), 0o600)
	d, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	d.Now = func() time.Time { return h.now }
	d.Local = time.UTC
	d.Logger = nil
	d.NotifyFn = nil
	d.LiveFn = func(j *core.Job) *live.State { return h.live }
	d.Awake = nil
	d.DeliverFn = func(o deliver.Options, j *core.Job) (deliver.Result, error) {
		if h.fail != nil {
			return deliver.Result{}, h.fail
		}
		cp := *j
		h.delivered = append(h.delivered, &cp)
		return deliver.Result{Via: "test", Detail: o.Prompt}, nil
	}
	h.d = d
	return h
}

func (h *harnessT) advance(d time.Duration) {
	h.now = h.now.Add(d)
	h.d.lastScan = time.Time{}
	h.d.Tick()
}

const deadPID = 999999

func limitEvent(h *harnessT, session string, resetIn time.Duration) core.Event {
	return core.Event{
		Harness: "claude", Kind: core.KindUsageLimit, SessionID: session, Cwd: "/work/app",
		At: h.now, ResetAt: h.now.Add(resetIn), Source: "hook:StopFailure", PID: deadPID,
		Location: &core.Location{Kind: "tmux", Target: "%1"},
	}
}

func TestLimitThenResume(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute))
	j := h.d.Jobs["claude:s1"]
	if j == nil || j.State != core.StatePending {
		t.Fatalf("no pending job: %+v", h.d.Jobs)
	}
	// Process gone, so no grace: due one minute after the reset.
	if want := h.now.Add(31 * time.Minute); !j.DueAt.Equal(want) {
		t.Fatalf("due %v want %v", j.DueAt, want)
	}
	h.advance(30 * time.Minute)
	if len(h.delivered) != 0 {
		t.Fatal("delivered before the reset")
	}
	h.advance(2 * time.Minute)
	if len(h.delivered) != 1 || j.State != core.StateDelivered || j.DeliveredVia != "test" {
		t.Fatalf("delivered=%d state=%s", len(h.delivered), j.State)
	}
	h.advance(10 * time.Minute)
	if len(h.delivered) != 1 {
		t.Fatal("delivered twice")
	}
}

func TestGraceCanBeTurnedOff(t *testing.T) {
	h := setup(t)
	h.d.Config.NativeGrace = map[string]int{"claude": 0}
	ev := limitEvent(h, "s1", 30*time.Minute)
	ev.PID = os.Getpid()
	h.d.Handle(ev)
	if want := h.now.Add(31 * time.Minute); !h.d.Jobs["claude:s1"].DueAt.Equal(want) {
		t.Fatalf("due %v want %v", h.d.Jobs["claude:s1"].DueAt, want)
	}
}

func TestNativeGraceWhileAgentAlive(t *testing.T) {
	h := setup(t) // default: 5 minutes for Claude
	ev := limitEvent(h, "s1", 30*time.Minute)
	ev.PID = os.Getpid() // "Claude" is still running: it may resume by itself
	h.d.Handle(ev)
	j := h.d.Jobs["claude:s1"]
	if want := h.now.Add(35 * time.Minute); !j.DueAt.Equal(want) {
		t.Fatalf("due %v want %v (reset + 5 min grace)", j.DueAt, want)
	}
	// The agent says it will not resume by itself: drop the grace.
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindNativeGaveUp, SessionID: "s1", At: h.now})
	if want := h.now.Add(31 * time.Minute); !j.DueAt.Equal(want) {
		t.Fatalf("after give-up due %v want %v", j.DueAt, want)
	}
}

func TestBusySinceTheLimitMeansResumed(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute))
	// Claude resumed by itself at the reset and reports busy since then.
	h.live = &live.State{Checked: true, Open: true, Busy: true,
		Session: &live.Session{SessionID: "s1", Status: "busy", StatusSince: h.now.Add(30 * time.Minute)}}
	h.advance(32 * time.Minute)
	if j := h.d.Jobs["claude:s1"]; len(h.delivered) != 0 || j.State != core.StateSkipped {
		t.Fatalf("state %s delivered %d", j.State, len(h.delivered))
	}
}

func TestBusyBeforeTheLimitIsTheStall(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute))
	// "busy" set before the limit hit: that is the stalled turn itself.
	h.live = &live.State{Checked: true, Open: true, Busy: true,
		Session: &live.Session{SessionID: "s1", Status: "busy", StatusSince: h.now.Add(-time.Minute)}}
	h.advance(32 * time.Minute)
	if len(h.delivered) != 1 {
		t.Fatalf("delivered %d, want 1", len(h.delivered))
	}
}

func TestMovedOnIsLeftAlone(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute))
	h.live = &live.State{Checked: true, MovedOn: true}
	h.advance(32 * time.Minute)
	if len(h.delivered) != 0 || h.d.Jobs["claude:s1"].State != core.StateSkipped {
		t.Fatal("resumed a conversation the user had moved on from")
	}
}

func TestLiveTmuxPaneIsUsed(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute))
	h.live = &live.State{Checked: true, Open: true, Session: &live.Session{SessionID: "s1", PID: 4242, Tmux: "main:@3.%12"}}
	h.advance(32 * time.Minute)
	if len(h.delivered) != 1 || h.delivered[0].Location.String() != "tmux:%12" || h.delivered[0].PID != 4242 {
		t.Fatalf("%+v", h.delivered)
	}
}

func TestStillOpenRetriesAndNotifiesOnce(t *testing.T) {
	h := setup(t)
	var notes []string
	cfg := `{"claude_home": "` + h.claude + `", "codex_home": "` + h.codex + `", "scan_panes": false, "poll_seconds": 1}`
	os.WriteFile(h.d.Paths.Config, []byte(cfg), 0o600)
	h.d.reload()
	h.d.NotifyFn = func(title, body string) { notes = append(notes, body) }
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute))
	h.fail = fmt.Errorf("%w; %w", deliver.ErrNoRoute, deliver.ErrStillOpen)
	h.advance(32 * time.Minute)
	h.advance(5 * time.Minute)
	j := h.d.Jobs["claude:s1"]
	if j.State != core.StatePending || j.Attempts < 2 {
		t.Fatalf("state %s attempts %d", j.State, j.Attempts)
	}
	n := 0
	for _, t := range notes {
		if strings.HasPrefix(t, "Waiting for you") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("notes %v", notes)
	}
}

func TestKeepAwake(t *testing.T) {
	h := setup(t)
	if on, _ := h.d.wantAwake(); on {
		t.Fatal("awake with nothing to do")
	}
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute))
	if on, why := h.d.wantAwake(); !on || !strings.Contains(why, "resume is due") {
		t.Fatal("not kept awake for a pending resume")
	}
	h.advance(32 * time.Minute)
	if on, _ := h.d.wantAwake(); !on {
		t.Fatal("not kept awake right after resuming")
	}
	// The resumed session keeps writing its transcript: stay awake.
	h.now = h.now.Add(30 * time.Minute)
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindActivity, SessionID: "s1", At: h.now.Add(-2 * time.Minute)})
	if on, why := h.d.wantAwake(); !on || !strings.Contains(why, "still working") {
		t.Fatalf("not kept awake while working: %v %s", on, why)
	}
	// Quiet for a while: let the computer sleep.
	h.now = h.now.Add(20 * time.Minute)
	if on, _ := h.d.wantAwake(); on {
		t.Fatal("kept awake after the session went quiet")
	}
	// A resume far beyond keep_awake_hours does not hold the computer awake.
	h.d.Handle(limitEvent(h, "s2", 30*time.Hour))
	if on, _ := h.d.wantAwake(); on {
		t.Fatal("kept awake for a resume 30 hours away")
	}
}

func TestActivityCancels(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute))
	h.now = h.now.Add(31 * time.Minute)
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindActivity, SessionID: "s1", At: h.now})
	h.advance(5 * time.Minute)
	if len(h.delivered) != 0 || h.d.Jobs["claude:s1"].State != core.StateSkipped {
		t.Fatalf("state %s delivered %d", h.d.Jobs["claude:s1"].State, len(h.delivered))
	}
}

func TestTranscriptActivityCheckedAtDueTime(t *testing.T) {
	h := setup(t)
	path := filepath.Join(h.claude, "projects", "-work-app", "s1.jsonl")
	limit := `{"type":"assistant","sessionId":"s1","cwd":"/work/app","timestamp":"` + h.now.Format(time.RFC3339) + `","isApiErrorMessage":true,"error":"rate_limit","message":{"content":[{"type":"text","text":"You've hit your session limit · resets 9:30am (UTC)"}]}}` + "\n"
	os.WriteFile(path, []byte(limit), 0o600)
	ev := limitEvent(h, "s1", 30*time.Minute)
	ev.TranscriptPath = path
	ev.TranscriptSize = int64(len(limit))
	h.d.Handle(ev)
	// Claude resumed itself (its own message lands in the transcript), but the
	// daemon has not scanned yet.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"type":"user","sessionId":"s1","timestamp":"` + h.now.Add(30*time.Minute).Format(time.RFC3339) + `","message":{"content":"Your claude.ai usage limit has reset. Continue."}}` + "\n")
	f.Close()
	h.d.Config.WatchTranscripts = new(bool) // only the due-time check may see it
	h.advance(32 * time.Minute)
	if len(h.delivered) != 0 {
		t.Fatal("resumed a session that had already continued")
	}
	if s := h.d.Jobs["claude:s1"].State; s != core.StateSkipped {
		t.Fatalf("state %s", s)
	}
}

func TestDuplicateObserversMerge(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute))
	ev := limitEvent(h, "s1", 30*time.Minute)
	ev.Source = "transcript"
	ev.At = h.now.Add(2 * time.Second)
	h.d.Handle(ev)
	if len(h.d.Jobs) != 1 || len(h.d.Jobs["claude:s1"].Sources) != 2 {
		t.Fatalf("jobs %+v", h.d.Jobs)
	}
}

func TestWrapperAndTranscriptAreOneJob(t *testing.T) {
	h := setup(t)
	// The wrapper sees the message on screen: it knows the pane, not the session.
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindUsageLimit, Cwd: "/work/app", At: h.now,
		ResetAt: h.now.Add(time.Hour), Source: "wrapper", Location: &core.Location{Kind: "wrapper", Target: "w1"}})
	// The transcript knows the session, not the pane.
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindUsageLimit, SessionID: "s9", Cwd: "/work/app", At: h.now.Add(time.Second),
		ResetAt: h.now.Add(time.Hour), Source: "transcript", PID: deadPID})
	if len(h.d.Jobs) != 1 {
		t.Fatalf("want 1 job, got %d: %+v", len(h.d.Jobs), h.d.Jobs)
	}
	j := h.d.Jobs["claude:s9"]
	if j == nil || j.Location == nil || j.Location.Kind != "wrapper" || j.SessionID != "s9" {
		t.Fatalf("merged job %+v", j)
	}
}

func TestTwoPanesSameDirStaySeparate(t *testing.T) {
	h := setup(t)
	for _, w := range []string{"w1", "w2"} {
		h.d.Handle(core.Event{Harness: "generic", Kind: core.KindUsageLimit, Cwd: "/work/app", At: h.now,
			ResetAt: h.now.Add(time.Hour), Source: "wrapper", Location: &core.Location{Kind: "wrapper", Target: w}})
	}
	if len(h.d.Jobs) != 2 {
		t.Fatalf("want 2 jobs, got %d", len(h.d.Jobs))
	}
}

func TestNeverResume(t *testing.T) {
	h := setup(t)
	ev := limitEvent(h, "s1", 0)
	ev.Kind = core.KindNeverResume
	h.d.Handle(ev)
	h.advance(time.Hour)
	if len(h.delivered) != 0 || h.d.Jobs["claude:s1"].State != core.StateNeedsUser {
		t.Fatal("billing problems must not be resumed")
	}
}

func TestTransientAndUnknownBackoff(t *testing.T) {
	h := setup(t)
	h.d.Handle(core.Event{Harness: "codex", Kind: core.KindTransient, SessionID: "c1", At: h.now, Source: "rollout"})
	if j := h.d.Jobs["codex:c1"]; !j.DueAt.Equal(h.now.Add(2 * time.Minute)) {
		t.Fatalf("transient due %v", j.DueAt)
	}
	h.d.Handle(core.Event{Harness: "codex", Kind: core.KindUsageLimit, SessionID: "c2", At: h.now, Source: "rollout"})
	if j := h.d.Jobs["codex:c2"]; j.ResetKnown || !j.DueAt.Equal(h.now.Add(15*time.Minute)) {
		t.Fatalf("unknown reset due %v", j.DueAt)
	}
}

func TestRetryThenFail(t *testing.T) {
	h := setup(t)
	h.fail = errors.New("pane gone")
	h.d.Handle(limitEvent(h, "s1", 0))
	for i := 0; i < 20; i++ {
		h.advance(time.Hour)
	}
	j := h.d.Jobs["claude:s1"]
	if j.State != core.StateFailed || j.Attempts != h.d.Rules.Defaults.MaxAttempts {
		t.Fatalf("state %s attempts %d", j.State, j.Attempts)
	}
}

func TestLimitAgainAfterResumeCountsAttempts(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 0))
	h.advance(2 * time.Minute)
	if h.d.Jobs["claude:s1"].State != core.StateDelivered {
		t.Fatal("not delivered")
	}
	// The resume hit the limit again a minute later (the reset was not real yet).
	h.now = h.now.Add(time.Minute)
	h.d.Handle(limitEvent(h, "s1", 10*time.Minute))
	j := h.d.Jobs["claude:s1"]
	if j.State != core.StatePending || j.Attempts != 1 {
		t.Fatalf("state %s attempts %d", j.State, j.Attempts)
	}
}

func TestMaxWait(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 10*24*time.Hour))
	if j := h.d.Jobs["claude:s1"]; j.State != core.StateNeedsUser {
		t.Fatalf("state %s", j.State)
	}
}

func TestOldEventsIgnored(t *testing.T) {
	h := setup(t)
	ev := limitEvent(h, "s1", time.Hour)
	ev.At = h.now.Add(-13 * time.Hour)
	h.d.Handle(ev)
	if len(h.d.Jobs) != 0 {
		t.Fatal("event older than the catch-up window created a job")
	}
}

func TestBaselineDoesNotReviveHistory(t *testing.T) {
	h := setup(t)
	path := filepath.Join(h.claude, "projects", "-work-app", "old.jsonl")
	line := func(at time.Time) string {
		return `{"type":"assistant","sessionId":"old","cwd":"/work/app","timestamp":"` + at.Format(time.RFC3339) + `","isApiErrorMessage":true,"error":"rate_limit","message":{"content":[{"type":"text","text":"You've hit your session limit · resets 11am (UTC)"}]}}` + "\n"
	}
	os.WriteFile(path, []byte(line(h.now.Add(-time.Hour))), 0o600)
	h.advance(time.Second) // first scan: baseline only
	if len(h.d.Jobs) != 0 {
		t.Fatalf("history revived: %+v", h.d.Jobs)
	}
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(line(h.now))
	f.Close()
	h.advance(time.Second)
	j := h.d.Jobs["claude:old"]
	if j == nil || j.State != core.StatePending || !j.ResetAt.Equal(time.Date(2026, 11, 11, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("new limit not picked up: %+v", j)
	}
}

func TestCodexRolloutEndToEnd(t *testing.T) {
	h := setup(t)
	h.advance(time.Second) // baseline
	path := filepath.Join(h.codex, "sessions", "2026", "11", "11", "rollout-2026-11-11T09-00-00-01a1a5e1-093f-7213-9184-4ca49bda5024.jsonl")
	reset := h.now.Add(2 * time.Hour).Unix()
	content := `{"timestamp":"` + h.now.Format(time.RFC3339) + `","type":"session_meta","payload":{"id":"01a1a5e1-093f-7213-9184-4ca49bda5024","cwd":"/work/api","originator":"Codex Desktop"}}
{"timestamp":"` + h.now.Format(time.RFC3339) + `","type":"event_msg","payload":{"type":"token_count","rate_limits":{"primary":{"used_percent":100,"window_minutes":300,"resets_at":` + itoa(reset) + `}}}}
{"timestamp":"` + h.now.Format(time.RFC3339) + `","type":"event_msg","payload":{"type":"task_complete","error":{"message":"You've hit your usage limit. Try again at 11:00 AM.","codex_error_info":"usage_limit_exceeded"}}}
`
	os.WriteFile(path, []byte(content), 0o600)
	h.advance(time.Second)
	j := h.d.Jobs["codex:01a1a5e1-093f-7213-9184-4ca49bda5024"]
	if j == nil || j.Entrypoint != "Codex Desktop" || j.ResetAt.Unix() != reset || j.Cwd != "/work/api" {
		t.Fatalf("job %+v", j)
	}
	h.advance(2*time.Hour + 2*time.Minute)
	if len(h.delivered) != 1 {
		t.Fatalf("delivered %d", len(h.delivered))
	}
}

func TestControl(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", time.Hour))
	h.d.Handle(limitEvent(h, "s2", time.Hour))
	h.d.Handle(core.Event{Kind: "control", Message: "cancel s1"})
	if h.d.Jobs["claude:s1"].State != core.StateCancelled || h.d.Jobs["claude:s2"].State != core.StatePending {
		t.Fatal("cancel s1")
	}
	h.d.Handle(core.Event{Kind: "control", Message: "resume-now s2"})
	h.advance(time.Second)
	if len(h.delivered) != 1 || h.delivered[0].SessionID != "s2" {
		t.Fatalf("resume-now: %+v", h.delivered)
	}
}

func TestDisabledHarness(t *testing.T) {
	h := setup(t)
	h.d.Config.Harnesses = map[string]bool{"claude": false}
	h.d.Handle(limitEvent(h, "s1", time.Hour))
	if len(h.d.Jobs) != 0 {
		t.Fatal("disabled harness scheduled")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestGenericWrapperGuessMergesWithHook(t *testing.T) {
	for _, hookFirst := range []bool{false, true} {
		h := setup(t)
		wrap := core.Event{Harness: "generic", Kind: core.KindUsageLimit, Cwd: "/w", At: h.now,
			ResetAt: h.now.Add(time.Minute), Source: "wrapper", Location: &core.Location{Kind: "wrapper", Target: "w1"}}
		hook := core.Event{Harness: "codex", Kind: core.KindUsageLimit, SessionID: "t1", Cwd: "/w", At: h.now.Add(time.Second),
			ResetAt: h.now.Add(time.Minute), Source: "hook:Stop", Location: &core.Location{Kind: "wrapper", Target: "w1"}}
		if hookFirst {
			h.d.Handle(hook)
			h.d.Handle(wrap)
		} else {
			h.d.Handle(wrap)
			h.d.Handle(hook)
		}
		if len(h.d.Jobs) != 1 {
			t.Fatalf("hookFirst=%v: want 1 job, got %+v", hookFirst, h.d.Jobs)
		}
		j := h.d.Jobs["codex:t1"]
		if j == nil || j.Harness != "codex" {
			t.Fatalf("hookFirst=%v: job %+v", hookFirst, h.d.Jobs)
		}
		h.advance(3 * time.Minute)
		if len(h.delivered) != 1 {
			t.Fatalf("hookFirst=%v: delivered %d times", hookFirst, len(h.delivered))
		}
	}
}

func TestNeverTypeTwiceIntoOnePane(t *testing.T) {
	h := setup(t)
	loc := &core.Location{Kind: "tmux", Target: "%4"}
	// Two jobs that slipped past merging (different harness guesses, no shared session).
	h.d.Jobs["a"] = &core.Job{Key: "a", Harness: "claude", SessionID: "s1", Kind: core.KindUsageLimit, Location: loc, LimitAt: h.now, DueAt: h.now, State: core.StatePending}
	h.d.Jobs["b"] = &core.Job{Key: "b", Harness: "aider", Kind: core.KindUsageLimit, Location: loc, LimitAt: h.now, DueAt: h.now.Add(time.Second), State: core.StatePending}
	h.advance(5 * time.Second)
	if len(h.delivered) != 1 || h.d.Jobs["b"].State != core.StateSkipped {
		t.Fatalf("delivered %d, b=%s", len(h.delivered), h.d.Jobs["b"].State)
	}
}

func TestStaleMessageAfterResumeIsNotRetriedAtOnce(t *testing.T) {
	h := setup(t)
	ev := limitEvent(h, "s1", time.Minute)
	h.d.Handle(ev)
	h.advance(3 * time.Minute)
	if len(h.delivered) != 1 {
		t.Fatal("not delivered")
	}
	// The same message (same reset time) shows up again after the resume.
	again := ev
	again.At = h.now.Add(10 * time.Second)
	h.d.Handle(again)
	j := h.d.Jobs["claude:s1"]
	if j.State != core.StatePending || j.ResetKnown || j.DueAt.Sub(again.At) < 10*time.Minute {
		t.Fatalf("stale message retried too soon: state=%s due in %v", j.State, j.DueAt.Sub(again.At))
	}
	// The agent keeps working: the stale job is dropped.
	h.d.Handle(core.Event{Harness: "claude", Kind: core.KindActivity, SessionID: "s1", At: again.At.Add(time.Minute)})
	if j.State != core.StateSkipped {
		t.Fatalf("state %s", j.State)
	}
}

func TestActivityAtSameLocationCancels(t *testing.T) {
	h := setup(t)
	loc := &core.Location{Kind: "wrapper", Target: "w1"}
	h.d.Handle(core.Event{Harness: "generic", Kind: core.KindUsageLimit, At: h.now, Source: "wrapper", Location: loc})
	h.d.Handle(core.Event{Harness: "codex", Kind: core.KindActivity, SessionID: "t1", At: h.now.Add(time.Minute), Location: loc})
	for _, j := range h.d.Jobs {
		if j.State != core.StateSkipped {
			t.Fatalf("job %s still %s", j.Key, j.State)
		}
	}
}

func TestPaneScanFindsOtherAgents(t *testing.T) {
	if _, err := proc.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	h := setup(t)
	orig := mux.Runner
	defer func() { mux.Runner = orig }()
	mux.Runner = func(_ time.Duration, _ string, argv ...string) (string, error) {
		joined := strings.Join(argv, " ")
		switch {
		case strings.Contains(joined, "list-panes"):
			return "%1\tzsh\n%2\taider\n%3\tclaude\n", nil
		case strings.Contains(joined, "capture-pane") && strings.Contains(joined, "%2"):
			return "> fix the tests\nError: rate limit exceeded. Quota resets in 30 minutes.\n> ", nil
		case strings.Contains(joined, "capture-pane"):
			return "You've hit your session limit · resets 10am\n", nil // claude: transcripts cover it
		}
		return "", nil
	}
	h.d.scanPanes()
	if len(h.d.Jobs) != 1 {
		t.Fatalf("want 1 job (aider pane), got %+v", h.d.Jobs)
	}
	for _, j := range h.d.Jobs {
		if j.Location.Target != "%2" || !j.ResetKnown || !j.ResetAt.Equal(h.now.Add(30*time.Minute)) {
			t.Fatalf("job %+v", j)
		}
	}
	// The same message on the next scan is not a new limit.
	h.d.scanPanes()
	if len(h.d.Jobs) != 1 {
		t.Fatalf("rescan duplicated: %d jobs", len(h.d.Jobs))
	}
	for _, j := range h.d.Jobs {
		if len(j.Sources) != 1 {
			t.Fatalf("rescan merged again: %v", j.Sources)
		}
	}
}

func TestChosenSessionsOnly(t *testing.T) {
	h := setup(t)
	sc := scope.Scope{Mode: scope.ModeChosen}
	sc.Chosen, _ = scope.Add(nil, scope.Entry{Session: "s1"})
	if err := sc.Save(h.d.Paths); err != nil {
		t.Fatal(err)
	}
	h.d.reload()
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute))
	ev2 := limitEvent(h, "s2", 30*time.Minute)
	ev2.Location = &core.Location{Kind: "tmux", Target: "%2"}
	h.d.Handle(ev2)
	if h.d.Jobs["claude:s2"].State != core.StateIgnored {
		t.Fatalf("s2 %s", h.d.Jobs["claude:s2"].State)
	}
	h.advance(32 * time.Minute)
	if len(h.delivered) != 1 || h.delivered[0].SessionID != "s1" {
		t.Fatalf("delivered %+v", h.delivered)
	}
	// Choosing s2 after it stalled schedules it (its reset has passed: now).
	sc.Chosen, _ = scope.Add(sc.Chosen, scope.Entry{Session: "s2"})
	sc.Save(h.d.Paths)
	h.advance(2 * time.Second)
	if len(h.delivered) != 2 || h.delivered[1].SessionID != "s2" {
		t.Fatalf("delivered %d", len(h.delivered))
	}
}

func TestExcludedAndUnchosenLater(t *testing.T) {
	h := setup(t)
	h.d.Handle(limitEvent(h, "s1", 30*time.Minute)) // mode all
	if h.d.Jobs["claude:s1"].State != core.StatePending {
		t.Fatal("not scheduled in mode all")
	}
	sc := scope.Scope{Mode: scope.ModeAll}
	sc.Excluded, _ = scope.Add(nil, scope.Entry{Dir: "/work"})
	sc.Save(h.d.Paths)
	h.advance(32 * time.Minute)
	if len(h.delivered) != 0 || h.d.Jobs["claude:s1"].State != core.StateIgnored {
		t.Fatalf("excluded session resumed: %s", h.d.Jobs["claude:s1"].State)
	}
}

func TestGraceChangeAppliesToWaitingSessions(t *testing.T) {
	h := setup(t)
	ev := limitEvent(h, "s1", 30*time.Minute)
	ev.PID = os.Getpid()
	h.d.Handle(ev)
	h.d.Tick() // learns the current settings
	j := h.d.Jobs["claude:s1"]
	if want := h.now.Add(35 * time.Minute); !j.DueAt.Equal(want) {
		t.Fatalf("due %v want %v", j.DueAt, want)
	}
	cfg := `{"claude_home": "` + h.claude + `", "codex_home": "` + h.codex + `", "notify": false, "scan_panes": false, "poll_seconds": 1, "native_grace_seconds": {"claude": 0}}`
	os.WriteFile(h.d.Paths.Config, []byte(cfg), 0o600)
	h.d.Tick()
	if want := h.now.Add(31 * time.Minute); !j.DueAt.Equal(want) {
		t.Fatalf("after turning grace off: due %v want %v", j.DueAt, want)
	}
}
