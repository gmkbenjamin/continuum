package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/continuum-dev/continuum/internal/hooks"
)

func TestUpdateRemovedOurHooksTheyComeBack(t *testing.T) {
	h := setup(t)
	bin := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(bin, []byte("#!/bin/sh\necho '--bg --resume --print attach agents'\n"), 0o755)
	cfg := `{"claude_home": "` + h.claude + `", "codex_home": "` + h.codex + `", "binaries": {"claude": "` + bin + `"}, "scan_panes": false}`
	os.WriteFile(h.d.Paths.Config, []byte(cfg), 0o600)
	h.d.reload()
	var notes []string
	h.d.NotifyFn = func(title, body string) { notes = append(notes, title+": "+body) }
	h.d.Compat = LoadCompat(h.d.Paths)

	settings := filepath.Join(h.claude, "settings.json")
	os.WriteFile(settings, []byte(`{"theme": "dark"}`), 0o600) // an update rewrote it
	rec, _ := json.Marshal(Install{Bin: "/opt/continuum", Hooks: []string{"claude"}})
	os.WriteFile(InstallPath(h.d.Paths), rec, 0o600)

	h.d.checkCompat(true)
	var target hooks.Target
	for _, tg := range hooks.Targets(h.d.Config) {
		if tg.Harness == "claude" {
			target = tg
		}
	}
	if ok, why := hooks.Installed(target); !ok {
		t.Fatalf("hooks not restored: %s", why)
	}
	data, _ := os.ReadFile(settings)
	if !strings.Contains(string(data), `"theme": "dark"`) {
		t.Fatalf("other settings lost:\n%s", data)
	}
	if len(notes) == 0 || !strings.Contains(notes[0], "re-added its hooks") {
		t.Fatalf("user not told: %v", notes)
	}
	// Nothing changed since: no repeat work or notifications.
	notes = nil
	h.d.checkCompat(false)
	if len(notes) != 0 {
		t.Fatalf("repeated: %v", notes)
	}
}

func TestLearnsThatClaudesListCoversDesktop(t *testing.T) {
	h := setup(t)
	h.d.Compat = LoadCompat(h.d.Paths)
	ch := h.d.Rules.Harness("claude")
	if h.d.trustRegistry(ch)["Claude Desktop"] {
		t.Fatal("trusted before anything was seen")
	}
	reg := filepath.Join(h.claude, "sessions")
	os.MkdirAll(reg, 0o700)
	entry := `{"pid":` + itoa(int64(os.Getpid())) + `,"sessionId":"d1","entrypoint":"claude-desktop","version":"9.9.9","kind":"interactive"}`
	os.WriteFile(filepath.Join(reg, "1.json"), []byte(entry), 0o600)
	h.d.observeDesktop()
	dc := h.d.Compat.Desktop["Claude Desktop"]
	if dc == nil || dc.CoveredVersion == "" {
		t.Fatalf("not learned: %+v", dc)
	}
	// Trusted for the version it was seen with (or, without a version
	// folder to compare, for two weeks).
	if bundledVersion(ch.DesktopApps[0], h.d.homes()) == dc.CoveredVersion || bundledVersion(ch.DesktopApps[0], h.d.homes()) == "" {
		if !h.d.trustRegistry(ch)["Claude Desktop"] {
			t.Fatal("not trusted after being seen")
		}
	}
	// The app updates to a version not yet seen: distrust until seen again.
	dc.CoveredVersion = "0.0.1"
	if cur := bundledVersion(ch.DesktopApps[0], h.d.homes()); cur != "" && h.d.trustRegistry(ch)["Claude Desktop"] {
		t.Fatal("still trusted after the app updated")
	}
}
