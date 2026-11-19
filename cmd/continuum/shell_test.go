package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestShellBlockRoundTrip(t *testing.T) {
	block, err := shellBlock("zsh", "/opt/it's/continuum")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block, `'/opt/it'\''s/continuum'`) {
		t.Fatalf("path not quoted:\n%s", block)
	}
	for _, orig := range []string{"", "export A=1\n", "export A=1", "a\n\nb\n"} {
		once := replaceBlock(orig, block)
		twice := replaceBlock(once, block)
		if once != twice {
			t.Fatalf("not idempotent for %q", orig)
		}
		back := replaceBlock(once, "")
		if strings.TrimRight(back, "\n") != strings.TrimRight(orig, "\n") {
			t.Fatalf("remove: %q -> %q", orig, back)
		}
	}
	// A block in the middle of a file is replaced in place.
	mid := "a\n" + block + "b\n"
	if got := replaceBlock(mid, block); got != mid {
		t.Fatalf("middle:\n%s", got)
	}
}

func TestInstallShellFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ZDOTDIR", "")
	t.Setenv("SHELL", "/bin/zsh")
	rc := filepath.Join(home, ".zshrc")
	os.WriteFile(rc, []byte("alias ll='ls -l'\n"), 0o600)
	installShell("/x/continuum", false)
	installShell("/x/continuum", false)
	data, _ := os.ReadFile(rc)
	if strings.Count(string(data), blockStart) != 1 || !strings.HasPrefix(string(data), "alias ll='ls -l'\n") {
		t.Fatalf("zshrc:\n%s", data)
	}
	if st, _ := os.Stat(rc); st.Mode().Perm() != 0o600 {
		t.Fatalf("permissions changed: %v", st.Mode())
	}
	if _, err := os.Stat(filepath.Join(home, ".bashrc")); err == nil {
		t.Fatal("created a .bashrc that did not exist")
	}
	uninstallShell(false)
	data, _ = os.ReadFile(rc)
	if string(data) != "alias ll='ls -l'\n" {
		t.Fatalf("after uninstall: %q", data)
	}
}

func TestShellFunctions(t *testing.T) {
	out, err := shellFunctions("zsh", "/x/continuum", []hookAgent{{"claude", "claude"}, {"cursor", "cursor-agent"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"function claude { __continuum_run claude claude", "function cursor-agent { __continuum_run cursor cursor-agent"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if _, err := shellFunctions("fish", "/x", []hookAgent{{"gemini", "gemini"}}); err != nil {
		t.Fatal(err)
	}
}

// Cursor installs its CLI as `agent`, a link into its cursor-agent folder.
// That name is wrapped only while it is Cursor's.
func TestCursorAgentAliasWrappedOnlyWhenCursors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CONTINUUM_HOME", filepath.Join(home, ".continuum"))
	bin := filepath.Join(home, "bin")
	install := filepath.Join(home, ".local", "share", "cursor-agent", "versions", "2026.11.17")
	os.MkdirAll(bin, 0o755)
	os.MkdirAll(install, 0o755)
	os.WriteFile(filepath.Join(install, "cursor-agent"), []byte("#!/bin/sh\n"), 0o755)
	t.Setenv("PATH", bin)

	other := filepath.Join(home, "other-agent")
	os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755)
	os.Symlink(other, filepath.Join(bin, "agent"))
	if has(shellHookAgents(), "agent") {
		t.Fatal("another program named agent wrapped")
	}
	os.Remove(filepath.Join(bin, "agent"))
	os.Symlink(filepath.Join(install, "cursor-agent"), filepath.Join(bin, "agent"))
	os.Symlink(filepath.Join(install, "cursor-agent"), filepath.Join(bin, "cursor-agent"))
	got := shellHookAgents()
	if !has(got, "agent") || !has(got, "cursor-agent") {
		t.Fatalf("Cursor's agent command not wrapped: %+v", got)
	}
	for _, a := range got {
		if a.Command == "agent" && a.Harness != "cursor" {
			t.Fatalf("agent wrapped as %s", a.Harness)
		}
	}
}

func has(list []hookAgent, cmd string) bool {
	for _, a := range list {
		if a.Command == cmd {
			return true
		}
	}
	return false
}
