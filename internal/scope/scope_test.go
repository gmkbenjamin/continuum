package scope

import (
	"os"
	"path/filepath"
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
