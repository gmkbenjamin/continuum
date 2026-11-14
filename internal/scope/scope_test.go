package scope

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/continuum-dev/continuum/internal/core"
)

func TestCovers(t *testing.T) {
	a := &core.Job{Harness: "claude", SessionID: "4f2c1d9e-5a6b-4c7d", Cwd: "/w/rc/app"}
	b := &core.Job{Harness: "codex", SessionID: "019a-77", Cwd: "/w/shop"}
	c := &core.Job{Harness: "claude", Location: &core.Location{Kind: "wrapper", Target: "w1"}, Cwd: "/x"}

	all := Scope{Mode: ModeAll}
	for _, j := range []*core.Job{a, b, c} {
		if ok, _ := all.Covers(j); !ok {
			t.Fatal("mode all must cover everything")
		}
	}
	all.Excluded, _ = Add(nil, Entry{Dir: "/w/shop"})
	if ok, why := all.Covers(b); ok || why == "" {
		t.Fatal("excluded directory still covered")
	}

	ch := Scope{Mode: ModeChosen}
	if ok, _ := ch.Covers(a); ok {
		t.Fatal("nothing chosen yet")
	}
	ch.Chosen, _ = Add(ch.Chosen, Entry{Session: "4f2c1d9e"}) // id prefix
	ch.Chosen, _ = Add(ch.Chosen, Entry{Wrapper: "w1"})
	if ok, _ := ch.Covers(a); !ok {
		t.Fatal("chosen session not covered")
	}
	if ok, _ := ch.Covers(c); !ok {
		t.Fatal("chosen wrapper not covered")
	}
	if ok, _ := ch.Covers(b); ok {
		t.Fatal("unchosen session covered")
	}
	ch.Chosen, _ = Add(ch.Chosen, Entry{Dir: "/w"})
	if ok, _ := ch.Covers(b); !ok {
		t.Fatal("session under a chosen directory not covered")
	}
	if ok, _ := ch.Covers(&core.Job{SessionID: "z", Cwd: "/www"}); ok {
		t.Fatal("/www is not inside /w")
	}
	var n int
	ch.Chosen, n = Remove(ch.Chosen, Entry{Dir: "/w"}, false)
	if n != 1 || len(ch.Chosen) != 2 {
		t.Fatalf("remove: %d %v", n, ch.Chosen)
	}
	if _, changed := Add(ch.Chosen, Entry{Session: "4f2c1d9e"}); changed {
		t.Fatal("duplicate added")
	}
}

// A folder chosen or excluded through a symlink (or /tmp on macOS) matches
// the physical path the agent records.
func TestFolderMatchesThroughSymlinks(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real", "proj")
	os.MkdirAll(real, 0o700)
	link := filepath.Join(base, "link")
	if err := os.Symlink(filepath.Join(base, "real"), link); err != nil {
		t.Skip(err)
	}
	phys, _ := filepath.EvalSymlinks(real)
	e := Entry{Dir: filepath.Join(link, "proj")}
	if !e.Matches(&core.Job{Cwd: filepath.Join(phys, "sub")}) {
		t.Fatalf("%s doesn't match %s", e.Dir, phys)
	}
	if e.Matches(&core.Job{Cwd: filepath.Join(base, "real", "other")}) {
		t.Fatal("matched a sibling folder")
	}
}

// `continuum include 1` (a typo, or a session no longer listed) must not
// drop every exclusion whose id starts with 1: those sessions would be
// resumed again.
func TestRemoveNeedsMoreThanACharacter(t *testing.T) {
	list := []Entry{{Session: "1a2b3c4d-0000"}, {Session: "1f00aa00-0000"}}
	if out, n := Remove(list, Entry{Session: "1"}, false); n != 0 || len(out) != 2 {
		t.Fatalf("removed %d: %+v", n, out)
	}
	if out, n := Remove(list, Entry{Session: "1a2b"}, false); n != 1 || len(out) != 1 {
		t.Fatalf("by prefix: removed %d: %+v", n, out)
	}
}

func TestRemovePrefixRespectsHarness(t *testing.T) {
	list := []Entry{
		{Harness: "claude", Session: "1a2b3c4d-0000"},
		{Harness: "codex", Session: "1a2b9999-0000"},
	}
	out, n := Remove(list, Entry{Harness: "claude", Session: "1a2b"}, false)
	if n != 1 || len(out) != 1 || out[0].Harness != "codex" {
		t.Fatalf("removed %d entries, remaining %+v", n, out)
	}
}

func TestTildeAndConcurrentUpdate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := tilde(filepath.Join(home, "..dot")); got != filepath.Join("~", "..dot") {
		t.Fatalf("tilde(..dot) = %q", got)
	}

	t.Setenv("CONTINUUM_HOME", t.TempDir())
	p := core.GetPaths()
	const workers = 10
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := Update(p, func(sc Scope) (Scope, bool, error) {
				var changed bool
				sc.Chosen, changed = Add(sc.Chosen, Entry{Harness: "claude", Session: fmt.Sprintf("sess-%02d", i)})
				return sc, changed, nil
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Update failed: %v", err)
	}
	sc, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(sc.Chosen) != workers {
		t.Fatalf("lost concurrent scope updates: got %d of %d (%+v)", len(sc.Chosen), workers, sc.Chosen)
	}
}
