package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/deliver"
)

func TestOpenCodeSharedServerResume(t *testing.T) {
	h := setup(t)
	h.d.DeliverFn = deliver.Deliver
	t.Setenv("OPENCODE_PASSWORD", "test-password")
	const id = "ses_test"
	const path = "/api/session/" + id
	at := h.now.UnixMilli()
	session := fmt.Sprintf(`{"id":%q,"time":{"updated":%d},"location":{"directory":"/work/test"}}`, id, at)
	var mu sync.Mutex
	active, queued, progressed, moved, failReceipt := true, false, false, false, false
	resumes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if user, pw, ok := r.BasicAuth(); !ok || user != "opencode" || pw != "test-password" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/session":
			fmt.Fprintf(w, `{"data":[{"id":"ses_deleted","time":{"updated":%d}},%s]}`, at, session)
		case "/api/session/active":
			if active {
				fmt.Fprintf(w, `{"data":{%q:{"type":"running"}}}`, id)
			} else {
				fmt.Fprint(w, `{"data":{}}`)
			}
		case path:
			details := session
			if moved {
				details = fmt.Sprintf(`{"id":%q,"location":{"directory":"/excluded"}}`, id)
			}
			fmt.Fprintf(w, `{"data":%s}`, details)
		case path + "/message":
			if progressed {
				fmt.Fprintf(w, `{"data":[{"id":"msg_next","type":"idle","outcome":"succeeded","time":{"created":%d}}]}`, at+5000)
			} else {
				fmt.Fprintf(w, `{"data":[{"id":"msg_failure","type":"idle","outcome":"failed","time":{"created":%d},"error":{"type":"provider.rate-limit","message":"Quota exhausted. Try again in 10 seconds.","status":429,"retryAfterMs":3000}}]}`, at)
			}
		case path + "/inbox":
			if queued {
				fmt.Fprint(w, `{"data":[{"id":"msg_queued"}]}`)
			} else {
				fmt.Fprint(w, `{"data":[]}`)
			}
		case path + "/prompt":
			var req struct{ Text, Delivery string }
			if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil ||
				req.Text == "" || req.Delivery != "queue" {
				t.Errorf("invalid OpenCode prompt: method=%s input=%+v", r.Method, req)
			}
			resumes++
			if failReceipt {
				http.Error(w, "receipt lost", http.StatusBadGateway)
			} else {
				fmt.Fprintf(w, `{"data":{"id":"msg_resume","sessionID":%q}}`, id)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	h.d.Config.OpenCodeServer = server.URL
	if err := h.d.pollOpenCode(h.now); err == nil || len(h.d.Jobs) != 0 {
		t.Fatalf("expected a deleted-session error without scheduling a native retry: %v, jobs=%v", err, h.d.Jobs)
	}
	mu.Lock()
	active = false
	mu.Unlock()
	if err := h.d.pollOpenCode(h.now); err == nil {
		t.Fatal("expected an error for the deleted session")
	}
	job := h.d.Jobs["opencode:"+id]
	if job == nil || job.State != core.StatePending || !job.ResetAt.Equal(h.now.Add(10*time.Second)) {
		t.Fatalf("expected a scheduled resume after reset, got %+v", job)
	}
	h.now = job.DueAt

	mu.Lock()
	active = true
	mu.Unlock()
	h.d.runJob(job)
	if job.State != core.StatePending || job.Attempts != 0 || !job.DueAt.After(h.now) {
		t.Fatalf("busy server session was not deferred: %+v", job)
	}

	mu.Lock()
	active, queued = false, true
	mu.Unlock()
	h.d.runJob(job)
	if job.State != core.StateSkipped {
		t.Fatalf("queued user work was not respected: %+v", job)
	}

	mu.Lock()
	queued, progressed = false, true
	mu.Unlock()
	job.State = core.StatePending
	h.d.runJob(job)
	if job.State != core.StateSkipped {
		t.Fatalf("a newer turn was not respected: %+v", job)
	}

	mu.Lock()
	progressed, moved = false, true
	mu.Unlock()
	job.State = core.StatePending
	h.d.runJob(job)
	if job.State != core.StateSkipped {
		t.Fatalf("a moved session was not respected: %+v", job)
	}

	mu.Lock()
	moved = false
	mu.Unlock()
	job.State = core.StatePending
	h.d.runJob(job)
	if job.State != core.StateDelivered || job.DeliveredVia != "opencode-api" {
		t.Fatalf("server resume did not reach the existing session: %+v", job)
	}

	mu.Lock()
	failReceipt = true
	mu.Unlock()
	job.State = core.StatePending
	h.d.runJob(job)
	if job.State != core.StateNeedsUser {
		t.Fatalf("an uncertain send would be retried: %+v", job)
	}
	h.d.control("resume-now " + job.Key)
	if job.State != core.StateNeedsUser {
		t.Fatalf("an explicit retry requeued an uncertain send: %+v", job)
	}
	mu.Lock()
	defer mu.Unlock()
	if resumes != 2 {
		t.Fatalf("got %d sends, want one confirmed and one unconfirmed", resumes)
	}
}

func TestOpenCodeSessionsDoNotShareQuotaResets(t *testing.T) {
	h := setup(t)
	first := core.Event{
		Harness: "opencode", Kind: core.KindUsageLimit, SessionID: "ses_test",
		At: h.now, ResetAt: h.now.Add(2 * time.Hour), Source: "opencode-api",
		Location: &core.Location{Kind: "opencode-api", Target: "ses_test"},
	}
	h.d.Handle(first)
	second := first
	second.SessionID, second.ResetAt = "ses_other", time.Time{}
	second.Location = &core.Location{Kind: "opencode-api", Target: second.SessionID}
	h.d.Handle(second)
	if j := h.d.Jobs["opencode:ses_other"]; j == nil || j.ResetKnown {
		t.Fatalf("another session's reset was used: %+v", j)
	}
}
