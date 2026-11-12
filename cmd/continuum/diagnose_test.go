package main

import (
	"os"
	"strings"
	"testing"
)

func TestRedactor(t *testing.T) {
	home, _ := os.UserHomeDir()
	red := redactor()
	in := home + "/Documents/app 5e551011-0000-4000-8000-000000000001 done"
	out := red(in)
	if strings.Contains(out, home) || !strings.HasPrefix(out, "~/Documents/app") {
		t.Fatalf("home not shortened: %s", out)
	}
	if strings.Contains(out, "9b20-411d") || !strings.Contains(out, "5e551011…") {
		t.Fatalf("session id not shortened: %s", out)
	}
}

func TestDiagnoseLeavesOutPrompts(t *testing.T) {
	home := sandboxHome(t)
	p := os.Getenv("HOME") + "/.continuum"
	os.MkdirAll(p, 0o700)
	os.WriteFile(p+"/config.json", []byte(`{"resume_prompts": {"claude": "my secret plan"}, "notify": false}`), 0o600)
	os.WriteFile(p+"/history.jsonl", []byte(`{"action":"needs_user","message":"private message text","harness":"claude"}`+"\n"), 0o600)
	out := capture(t, func() { cmdDiagnose([]string{"--lines", "5"}) })
	for _, bad := range []string{"my secret plan", "private message text", home} {
		if strings.Contains(out, bad) {
			t.Errorf("report contains %q", bad)
		}
	}
	if !strings.Contains(out, `resume_prompts: "(set)"`) || !strings.Contains(out, "notify: false") {
		t.Errorf("settings section:\n%s", out)
	}
}
