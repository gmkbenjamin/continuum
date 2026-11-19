package live

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
)

// Output of `claude agents --json` (Claude Code 2.1.322), trimmed.
const agentsJSON = `[
 {"id":"7b3e9a10","pid":65041,"cwd":"/w/billing","kind":"background","startedAt":1793271046424,"sessionId":"7b3e9a10-2c4d-4e5f-8a6b-0c1d2e3f4a5b","name":"soak","status":"waiting","waitingFor":"dialog open","state":"blocked"},
 {"id":"77aa0000","cwd":"/w/other","kind":"background","sessionId":"77aa0000-1111","name":"dormant","state":"blocked"},
 {"pid":68693,"cwd":"/w/web","kind":"interactive","startedAt":1794294991992,"sessionId":"4f2c1d9e-5a6b-4c7d-8e9f-a0b1c2d3e4f5","name":"web","status":"idle"},
 {"pid":5093,"cwd":"/w/docs","kind":"interactive","sessionId":"3c8d2e71","status":"busy"}
]`

func finder(t *testing.T, alive map[int]bool, cmdOut string, cmdErr error) *Finder {
	return &Finder{
		Run:    func(time.Duration, ...string) (string, error) { return cmdOut, cmdErr },
		Alive:  func(pid int) bool { return alive[pid] },
		HelpOK: func(string, []string) bool { return true },
		Procs:  func() ([]proc.Process, error) { return nil, nil },
		Cwd:    func(int) string { return "" },
	}
}

func claudeRules(t *testing.T) *rules.Harness {
	r, err := rules.Load("")
	if err != nil {
		t.Fatal(err)
	}
	return r.Harness("claude")
}

func TestClaudeAgentsJSON(t *testing.T) {
	h := claudeRules(t)
	f := finder(t, nil, agentsJSON, nil)
	q := Query{Bin: "claude", Homes: map[string]string{"claude_home": t.TempDir()}}

	q.SessionID = "7b3e9a10-2c4d-4e5f-8a6b-0c1d2e3f4a5b"
	st := f.Lookup(h, q)
	if !st.Checked || !st.Open || !st.Background || st.Session.ShortID != "7b3e9a10" || st.Session.WaitingFor != "dialog open" {
		t.Fatalf("background: %+v %+v", st, st.Session)
	}
	// Listed, but no process: dormant, so not open (no copy is possible).
	q.SessionID = "77aa0000-1111"
	if st := f.Lookup(h, q); !st.Checked || st.Open || !st.Dormant {
		t.Fatalf("dormant: %+v", st)
	}
	q.SessionID = "4f2c1d9e-5a6b-4c7d-8e9f-a0b1c2d3e4f5"
	if st := f.Lookup(h, q); !st.Open || st.Background || st.Busy || st.Session.PID != 68693 {
		t.Fatalf("interactive idle: %+v", st)
	}
	q.SessionID = "3c8d2e71"
	if st := f.Lookup(h, q); !st.Busy {
		t.Fatalf("busy: %+v", st)
	}
	q.SessionID = "gone"
	if st := f.Lookup(h, q); !st.Checked || st.Open {
		t.Fatalf("closed: %+v", st)
	}
	// Even if a recycled PID is alive, an authoritative `claude agents --json`
	// answer that does not list the session means it is closed.
	fAlive := finder(t, map[int]bool{999: true}, agentsJSON, nil)
	q.SessionID, q.PID = "gone", 999
	if st := fAlive.Lookup(h, q); !st.Checked || st.Open {
		t.Fatalf("closed session with recycled pid: %+v", st)
	}
}

func TestRegistryFallbackAndMovedOn(t *testing.T) {
	h := claudeRules(t)
	home := t.TempDir()
	reg := filepath.Join(home, "sessions")
	os.MkdirAll(reg, 0o700)
	os.WriteFile(filepath.Join(reg, "100.json"), []byte(`{"pid":100,"sessionId":"new-one","cwd":"/w","status":"idle","statusUpdatedAt":1794402575833,"tmux":"main:@1.%7"}`), 0o600)
	os.WriteFile(filepath.Join(reg, "200.json"), []byte(`{"pid":200,"sessionId":"stale","cwd":"/w"}`), 0o600) // crashed
	os.WriteFile(filepath.Join(reg, "100.e3e8.key"), []byte(`secret`), 0o600)                                 // never read
	f := finder(t, map[int]bool{100: true}, "", os.ErrNotExist)                                               // command fails
	q := Query{Bin: "claude", Homes: map[string]string{"claude_home": home}}

	q.SessionID = "new-one"
	st := f.Lookup(h, q)
	if !st.Open || st.Source != "registry" || TmuxPane(st.Session.Tmux) != "%7" || st.Session.StatusSince.IsZero() {
		t.Fatalf("registry: %+v %+v", st, st.Session)
	}
	q.SessionID = "stale"
	if st := f.Lookup(h, q); st.Open {
		t.Fatal("a crashed process's file counted as open")
	}
	// The hook recorded pid 100 for session "old"; pid 100 now holds "new-one".
	q.SessionID, q.PID = "old", 100
	if st := f.Lookup(h, q); !st.MovedOn || st.Open {
		t.Fatalf("moved on: %+v", st)
	}
}

func TestPidOnlyWhenNothingElseAnswers(t *testing.T) {
	r, _ := rules.Load("")
	h := r.Harness("generic") // no live_sessions rules
	f := finder(t, map[int]bool{7: true}, "", os.ErrNotExist)
	if st := f.Lookup(h, Query{SessionID: "x", PID: 7}); !st.Checked || !st.Open {
		t.Fatalf("%+v", st)
	}
	if st := f.Lookup(h, Query{SessionID: "x", PID: 8}); !st.Checked || st.Open {
		t.Fatalf("%+v", st)
	}
	if st := f.Lookup(h, Query{SessionID: "x"}); st.Checked {
		t.Fatalf("%+v", st)
	}
}

func TestCodexProcessScan(t *testing.T) {
	r, _ := rules.Load("")
	h := r.Harness("codex")
	f := finder(t, nil, "", nil)
	f.Procs = func() ([]proc.Process, error) {
		return []proc.Process{
			{PID: 1, Args: []string{"/x/codex", "app-server", "daemon"}, Line: "/x/codex app-server daemon"},
			{PID: 2, Args: []string{"node", "/x/bin/codex.js", "resume", "t-123"}, Line: "node /x/bin/codex.js resume t-123"},
			{PID: 3, Args: []string{"/x/codex"}, Line: "/x/codex"},
			{PID: 4, Args: []string{"/x/codex", "exec", "hi"}, Line: "/x/codex exec hi"},
		}, nil
	}
	f.Cwd = func(pid int) string { return map[int]string{1: "/w/a", 3: "/w/b", 4: "/w/c"}[pid] }

	if st := f.Lookup(h, Query{SessionID: "t-123"}); !st.Open || st.Session.PID != 2 {
		t.Fatalf("by id: %+v", st)
	}
	if st := f.Lookup(h, Query{SessionID: "t-9", Cwd: "/w/b"}); !st.Open || st.Session.PID != 3 {
		t.Fatalf("by dir: %+v", st)
	}
	// The app server and `codex exec` are not the TUI holding a session.
	// (On Windows a process's folder is unknown, so any may hold it.)
	for _, dir := range []string{"/w/a", "/w/c", "/w/z"} {
		if runtime.GOOS == "windows" {
			break
		}
		if st := f.Lookup(h, Query{SessionID: "t-9", Cwd: dir}); !st.Checked || st.Open {
			t.Fatalf("%s: %+v", dir, st)
		}
	}
}

func TestDesktopAppHoldsSessions(t *testing.T) {
	h := claudeRules(t)
	app := proc.Process{PID: 9, Args: []string{"/Applications/Claude.app/Contents/MacOS/Claude"}, Line: "/Applications/Claude.app/Contents/MacOS/Claude"}
	f := finder(t, nil, `[]`, nil) // Claude's own list: nothing open
	f.Procs = func() ([]proc.Process, error) { return []proc.Process{app}, nil }
	q := Query{SessionID: "d1", Bin: "claude", Entrypoint: "claude-desktop", Homes: map[string]string{"claude_home": t.TempDir()}}

	st := f.Lookup(h, q)
	if !st.Open || !st.OnlyDesktop || st.DesktopApp != "Claude Desktop" {
		t.Fatalf("desktop session with the app running: %+v", st)
	}
	// Once Claude's own list is known to cover Desktop sessions, trust it.
	q.TrustRegistry = map[string]bool{"Claude Desktop": true}
	if st := f.Lookup(h, q); st.Open {
		t.Fatalf("trusted list says closed: %+v", st)
	}
	q.TrustRegistry = nil
	// Unknown origin (an update dropped the field): still guarded.
	q.Entrypoint = ""
	if st := f.Lookup(h, q); !st.Open {
		t.Fatal("unknown-origin session not guarded while the app runs")
	}
	// A CLI session is not affected by the app running.
	q.Entrypoint = "cli"
	if st := f.Lookup(h, q); st.Open {
		t.Fatal("CLI session treated as held by the desktop app")
	}
	// App not running: closed.
	f.Procs = func() ([]proc.Process, error) { return nil, nil }
	q.Entrypoint = "claude-desktop"
	if st := f.Lookup(h, q); st.Open {
		t.Fatal("desktop session open with the app closed")
	}
}

// `muse sessions --json` (Muse Code 1.4.24): an object, no pid or folder.
func TestMuseSessionsJSON(t *testing.T) {
	r, err := rules.Load("")
	if err != nil {
		t.Fatal(err)
	}
	h := r.Harness("muse")
	out := `{"sessions":[{"sessionId":"01a1f3be-64e4-7933-a259-43a984103466","name":"brown-declination","workspaceLabel":"musews","openedBy":"tui","binaryVersion":null,"attachable":false}]}`
	f := finder(t, nil, out, nil)
	st := f.Lookup(h, Query{SessionID: "01a1f3be-64e4-7933-a259-43a984103466", Bin: "/u/.local/bin/muse"})
	if !st.Checked || !st.Open || !st.Authoritative || st.Session.Name != "brown-declination" || st.Session.Entrypoint != "tui" {
		t.Fatalf("open session: %+v %+v", st, st.Session)
	}
	st = f.Lookup(h, Query{SessionID: "gone", Bin: "/u/.local/bin/muse"})
	if !st.Checked || st.Open || !st.Authoritative {
		t.Fatalf("closed session: %+v", st)
	}
	// Nothing running: an empty list is still an answer.
	st = finder(t, nil, "note: something\n{\"sessions\":[]}", nil).Lookup(h, Query{SessionID: "x", Bin: "muse"})
	if !st.Checked || st.Open {
		t.Fatalf("empty list: %+v", st)
	}
	// Not the expected shape: no answer, rather than "closed".
	st = finder(t, nil, `{"error":"registry locked"}`, nil).Lookup(h, Query{SessionID: "x", Bin: "muse"})
	if st.Checked && st.Source == "command" {
		t.Fatalf("bad output taken as an answer: %+v", st)
	}
}
