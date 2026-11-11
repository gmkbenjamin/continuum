//go:build realagents

package deliver

import (
	"os"
	"testing"

	"github.com/continuum-dev/continuum/internal/proc"
	"github.com/continuum-dev/continuum/internal/rules"
)

// CONTINUUM_ATTACH_PROBE=<short id> go test -tags realagents -run AttachProbe ./internal/deliver
// Opens a real, RUNNING Claude background session through `claude attach` in a
// hidden terminal, checks Continuum recognises the input box (or refuses a
// menu), types nothing, closes. Only use it on a session whose process is
// running: attaching to a dormant one restarts it.
func TestRealAttachProbe(t *testing.T) {
	id := os.Getenv("CONTINUUM_ATTACH_PROBE")
	if id == "" {
		t.Skip("set CONTINUUM_ATTACH_PROBE to a background session id")
	}
	r, _ := rules.Load("")
	h := r.Harness("claude")
	bin, err := proc.LookPath("claude")
	if err != nil {
		t.Skip(err)
	}
	err = typeViaPTY(r, h, []string{bin, "attach", id}, "", CleanEnv(os.Environ()), "")
	t.Logf("probe result: %v", err)
	if err != nil {
		t.Fatal(err)
	}
}
