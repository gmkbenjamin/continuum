package live

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
)

// Grok Build lists its open sessions in active_sessions.json.
func TestGrokActiveSessions(t *testing.T) {
	r, _ := rules.Load("")
	h := r.Harness("grok")
	home := t.TempDir()
	os.WriteFile(filepath.Join(home, "active_sessions.json"), []byte(`[
  {"session_id": "019a0000-0000-7000-8000-000000000001", "pid": `+strconv.Itoa(os.Getpid())+`, "pid_start": 1, "cwd": "/w", "opened_at": "2026-11-13T01:00:00Z"},
  {"session_id": "019a0000-0000-7000-8000-000000000002", "pid": 999999, "pid_start": 1, "cwd": "/w", "opened_at": "2026-11-13T01:00:00Z"}
]`), 0o600)
	f := finder(t, map[int]bool{os.Getpid(): true}, "", nil)
	q := Query{Homes: map[string]string{"grok_home": home}}
	list, ok := f.List(h, q)
	if !ok || len(list) != 1 || list[0].SessionID != "019a0000-0000-7000-8000-000000000001" {
		t.Fatalf("list %+v ok=%v", list, ok)
	}
	q.SessionID = "019a0000-0000-7000-8000-000000000001"
	if st := f.Lookup(h, q); !st.Open || !st.Authoritative {
		t.Fatalf("open session: %+v", st)
	}
	q.SessionID = "019a0000-0000-7000-8000-000000000002" // its process is gone
	if st := f.Lookup(h, q); st.Open || !st.Checked || !st.Authoritative {
		t.Fatalf("closed session: %+v", st)
	}
}

// Windows lists programs by name only: a running claude.exe may be Claude
// Desktop, which may hold the session, so it counts as open.
func TestDesktopGuardWithBareProgramNames(t *testing.T) {
	h := claudeRules(t)
	f := finder(t, nil, "", nil)
	f.Procs = func() ([]proc.Process, error) {
		return []proc.Process{{PID: 7, Args: []string{"claude.exe"}, Line: "claude.exe"}}, nil
	}
	if st := f.Lookup(h, Query{SessionID: "s1", Entrypoint: "claude-desktop"}); !st.Open || st.DesktopApp != "Claude Desktop" {
		t.Fatalf("%+v", st)
	}
}
