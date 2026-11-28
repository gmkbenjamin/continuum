package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/continuum-dev/continuum/internal/core"
	"github.com/continuum-dev/continuum/internal/hooks"
	"github.com/continuum-dev/continuum/internal/rules"
)

func TestConfirm(t *testing.T) {
	old := stdinIsTerminal
	t.Cleanup(func() { stdinIsTerminal = old })

	stdinIsTerminal = func() bool { return false }
	if err := confirm("go?", false, strings.NewReader("y\n")); err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("no terminal: %v", err)
	}
	if err := confirm("go?", true, nil); err != nil {
		t.Fatalf("--yes: %v", err)
	}
	stdinIsTerminal = func() bool { return true }
	for in, want := range map[string]error{"y\n": nil, "YES\n": nil, "\n": errDeclined, "n\n": errDeclined, "maybe\n": errDeclined, "": errDeclined} {
		if err := confirm("go?", false, strings.NewReader(in)); !errors.Is(err, want) && !(err == nil && want == nil) {
			t.Errorf("%q: got %v want %v", in, err, want)
		}
	}
}

func sandboxHome(t *testing.T) string {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows takes the home folder from here
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("ZDOTDIR", "")
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("CONTINUUM_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, ".claude"))
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("GROK_HOME", filepath.Join(home, ".grok"))
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o700)
	os.MkdirAll(filepath.Join(home, ".local", "bin"), 0o700)
	// Stand-ins for the agents: tests never run the real ones (they would
	// write files of their own, and depend on what is installed here).
	stubs := filepath.Join(home, "stub-bin")
	os.MkdirAll(stubs, 0o700)
	for _, name := range []string{"claude", "codex"} {
		os.WriteFile(filepath.Join(stubs, name), []byte("#!/bin/sh\ncase \"$*\" in *--version*) echo '0.0.0 (test stand-in)';; *agents*) echo '[]';; esac\nexit 0\n"), 0o755)
	}
	t.Setenv("PATH", filepath.Join(home, ".local", "bin")+string(os.PathListSeparator)+stubs+string(os.PathListSeparator)+"/usr/bin:/bin:/usr/sbin:/sbin")
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte("{\n  \"model\": \"opus\"\n}\n"), 0o600)
	os.WriteFile(filepath.Join(home, ".zshrc"), []byte("export EDITOR=vim\n"), 0o600)
	return home
}

func whats(items []planItem) map[string]planItem {
	m := map[string]planItem{}
	for _, it := range items {
		m[it.What+"|"+filepath.ToSlash(tildePath(it.Path))] = it // one form on every OS
	}
	return m
}

func TestInstallPlanListsEverything(t *testing.T) {
	home := sandboxHome(t)
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	r, _ := rules.Load("")
	items := installPlanItems(installOptions{Copy: true, Shell: true, Service: true}, p, cfg, r)
	m := whats(items)
	if runtime.GOOS != "windows" { // Windows gets continuum.exe and PowerShell profile hook (no ~/.local/bin symlink)
		for _, key := range []string{"Continuum program|~/.continuum/bin/continuum", "command link|~/.local/bin/continuum", "zsh shell hook|~/.zshrc"} {
			if _, ok := m[key]; !ok {
				t.Errorf("plan is missing %s; has %v", key, keys(m))
			}
		}
	} else {
		if _, ok := m["Continuum program|~/.continuum/bin/continuum.exe"]; !ok {
			t.Errorf("windows plan missing continuum.exe; has %v", keys(m))
		}
		foundPS := false
		for k := range m {
			if strings.HasPrefix(k, "pwsh shell hook|") || strings.HasPrefix(k, "powershell shell hook|") {
				foundPS = true
			}
		}
		if !foundPS {
			t.Errorf("windows plan missing PowerShell shell hook; has %v", keys(m))
		}
	}
	for _, key := range []string{
		"Continuum folder|~/.continuum",
		"Claude Code hooks|~/.claude/settings.json",
		"Codex hooks|~/.codex/hooks.json",
	} {
		it, ok := m[key]
		if !ok {
			t.Errorf("plan is missing %s; has %v", key, keys(m))
			continue
		}
		if it.Purpose == "" {
			t.Errorf("%s has no purpose", key)
		}
	}
	if m["Claude Code hooks|~/.claude/settings.json"].Action != "change" || m["Codex hooks|~/.codex/hooks.json"].Action != "create" {
		t.Errorf("actions: %+v", items)
	}
	var out bytes.Buffer
	printPlan(&out, "Continuum will make these changes:", items, installFooter)
	if !strings.Contains(out.String(), "login item") || !strings.Contains(out.String(), "sends nothing anywhere") {
		t.Errorf("printed plan:\n%s", out.String())
	}
	_ = home
}

func TestUninstallPlan(t *testing.T) {
	home := sandboxHome(t)
	p := core.GetPaths()
	cfg, _ := core.LoadConfig(p.Config)
	if items := uninstallPlanItems(p, cfg, false); len(items) != 0 {
		t.Fatalf("nothing installed, but plan: %+v", items)
	}
	// Install the parts that need no running processes.
	for _, tg := range hooks.Targets(cfg) {
		hooks.Install(tg, "/x/.continuum/bin/continuum", false)
	}
	installShell("/x/.continuum/bin/continuum", false)
	os.MkdirAll(p.Home, 0o700)
	os.WriteFile(filepath.Join(p.Home, "jobs.json"), []byte("{}"), 0o600)

	m := whats(uninstallPlanItems(p, cfg, false))
	wantUninstall := []string{"Claude Code hooks|~/.claude/settings.json", "Codex hooks|~/.codex/hooks.json"}
	if runtime.GOOS != "windows" {
		wantUninstall = append(wantUninstall, "zsh shell hook|~/.zshrc")
	}
	for _, key := range wantUninstall {
		if _, ok := m[key]; !ok {
			t.Errorf("uninstall plan is missing %s; has %v", key, keys(m))
		}
	}
	if m["Continuum folder|~/.continuum"].Action != "keep" {
		t.Errorf("folder should be kept without --purge")
	}
	if runtime.GOOS != "windows" {
		n := 0
		for k := range m {
			if strings.HasPrefix(k, "zsh shell hook") {
				n++
			}
		}
		if n != 1 {
			t.Errorf(".zshrc listed %d times", n)
		}
	}
	purge := whats(uninstallPlanItems(p, cfg, true))
	if purge["Continuum folder|~/.continuum"].Action != "remove" {
		t.Errorf("--purge should remove the folder")
	}
	backups := 0
	for k := range purge {
		if strings.HasPrefix(k, "settings backup|") {
			backups++
		}
	}
	wantBackups := 2 // settings.json and .zshrc (hooks.json was created, not changed)
	if runtime.GOOS == "windows" {
		wantBackups = 1 // PowerShell profile was created fresh, not changed
	}
	if backups != wantBackups {
		t.Errorf("backups listed: %d (%v)", backups, keys(purge))
	}
	_ = home
}

func keys(m map[string]planItem) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}
