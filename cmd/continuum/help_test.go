package main

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func capture(t *testing.T, f func()) string {
	t.Helper()
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	f()
	w.Close()
	os.Stdout = old
	out, _ := io.ReadAll(r)
	return string(out)
}

// Every command in main's switch has help, and every flag a command parses
// shows up in its help.
func TestEveryCommandHasHelp(t *testing.T) {
	t.Setenv("CONTINUUM_HOME", t.TempDir())
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	body = body[strings.Index(body, "func main()"):]
	body = body[:strings.Index(body, "\n}\n")]
	for _, m := range regexp.MustCompile(`case "([a-z][a-z-]*)"`).FindAllStringSubmatch(body, -1) {
		if findHelp(m[1]) == nil {
			t.Errorf("command %q has no help entry", m[1])
		}
	}
	all := ""
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if !strings.HasSuffix(f, "_test.go") {
			b, _ := os.ReadFile(f)
			all += string(b)
		}
	}
	flagsOf := map[string][]string{}
	for _, m := range regexp.MustCompile(`(?s)newFlagSet\("([a-z-]+)"\)(.*?)parseFlags`).FindAllStringSubmatch(all, -1) {
		for _, f := range regexp.MustCompile(`fs\.\w+\("([a-z-]+)"`).FindAllStringSubmatch(m[2], -1) {
			flagsOf[m[1]] = append(flagsOf[m[1]], f[1])
		}
	}
	if len(flagsOf["install"]) < 5 {
		t.Fatalf("flag scan broken: %v", flagsOf)
	}
	for _, e := range helpEntries {
		out := capture(t, func() { helpFor(e.Name) })
		if !strings.Contains(out, "continuum "+e.Name+" - ") || !strings.Contains(out, "-h, --help") {
			t.Errorf("%s: help incomplete:\n%s", e.Name, out)
		}
		for _, f := range flagsOf[e.Name] {
			opt := "--" + f
			if len(f) == 1 {
				opt = "-" + f
			}
			if !strings.Contains(out, opt) {
				t.Errorf("%s: option --%s missing from help", e.Name, f)
			}
		}
		if flagsOf[e.Name] != nil && !flagCommands[e.Name] {
			t.Errorf("%s parses options but is not in flagCommands", e.Name)
		}
	}
	top := usageText()
	for _, e := range helpEntries {
		if !e.Hidden && !strings.Contains(top, "  "+e.Name+" ") {
			t.Errorf("%s missing from the command list", e.Name)
		}
	}
}

func TestWantsHelp(t *testing.T) {
	cases := []struct {
		cmd  string
		args []string
		want bool
	}{
		{"install", []string{"-h"}, true},
		{"install", []string{"--dry-run", "--help"}, true},
		{"run", []string{"-h"}, true},
		{"run", []string{"--", "claude", "-h"}, false},
		{"run", []string{"claude", "-h"}, false},
		{"run", []string{"--harness", "x", "--", "aider", "--help"}, false},
		{"detect", []string{"some text"}, false},
		{"choose", []string{"api"}, false},
	}
	for _, c := range cases {
		if got := wantsHelp(c.cmd, c.args); got != c.want {
			t.Errorf("%s %v: %v", c.cmd, c.args, got)
		}
	}
}
