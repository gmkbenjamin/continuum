package rules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func load(t *testing.T) *Rules {
	t.Helper()
	r, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestClassifyKnownMessages(t *testing.T) {
	r := load(t)
	cases := []struct{ harness, text, kind string }{
		{"claude", "You've hit your session limit · resets 10am (Australia/Sydney)", "usage_limit"},
		{"claude", "You’ve hit your weekly limit · resets Nov 14, 6pm", "usage_limit"},
		{"claude", "You've hit your 5-hour limit · resets 11pm (Australia/Sydney)", "usage_limit"},
		{"claude", "You've hit your Opus 4.5 limit · resets 6pm", "usage_limit"},
		{"claude", "You've hit your stride. The rate limit docs say", ""},
		{"claude", "You've hit your Opus limit · resets 6pm", "usage_limit"},
		{"claude", "Claude usage limit reached. Your limit will reset at 5pm.", "usage_limit"},
		{"claude", "  ⎿  API Error: 529 {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}", "transient"},
		{"codex", "■ You've hit your usage limit. Upgrade to Pro (https://chatgpt.com/explore/pro), or try again at 5:53 PM.", "usage_limit"},
		{"codex", "error: usage_limit_exceeded", "usage_limit"},
		{"generic", "Error: rate limit exceeded, quota resets in 10 minutes", "usage_limit"},
		{"generic", "Quota reached. Resets in 1h5m.", "usage_limit"},
	}
	for _, c := range cases {
		m := r.Harness(c.harness).Classify(c.text, 40)
		if c.kind == "" { // must not match
			if m != nil {
				t.Errorf("%s: Classify(%q) = %+v, want no match", c.harness, c.text, m)
			}
			continue
		}
		if m == nil || m.Kind != c.kind {
			t.Errorf("%s: Classify(%q) = %+v, want %s", c.harness, c.text, m, c.kind)
		}
	}
}

func TestClassifyOnlyLooksAtTheBottom(t *testing.T) {
	r := load(t)
	h := r.Harness("claude")
	// The agent discussed limits long ago; the screen now shows normal work.
	var b strings.Builder
	b.WriteString("Earlier we saw: You've hit your session limit · resets 10am\n")
	for i := 0; i < 60; i++ {
		b.WriteString("● Editing file number something\n")
	}
	if m := h.Classify(b.String(), 40); m != nil {
		t.Fatalf("matched old text: %+v", m)
	}
}

func TestNoFalsePositives(t *testing.T) {
	r := load(t)
	for _, name := range r.HarnessNames() {
		h := r.Harness(name)
		for _, s := range []string{
			"● I'll add rate limiting to the API handler.",
			"Tests passed: 42",
			"Your claude.ai usage limit has reset. Continue the task you were working on.",
			"> continue",
			"> has it been keep trying to continue sessions even though usage limit hit? check logs, if no logs, implement debug/diagnostic logs.",
			"❯ did we hit the rate limit?",
		} {
			if m := h.Classify(s, 40); m != nil {
				t.Errorf("%s matched %q as %s", name, s, m.Kind)
			}
		}
	}
}

func TestNeverResumeAndNative(t *testing.T) {
	r := load(t)
	c := r.Harness("claude")
	if !c.NeverResume("You're out of usage credits · add funds to continue") {
		t.Error("out of credits should need the user")
	}
	if c.NeverResume("You've hit your session limit · resets 10am") {
		t.Error("ordinary limit should not need the user")
	}
	if !c.NativeWaiting("Usage limit reached · continuing automatically at 10:00 · esc to cancel") {
		t.Error("native wait not recognised")
	}
	if !c.NativeGaveUp("Claude Code exited during the wait, so the task will not resume on its own when the usage limit resets") {
		t.Error("native give-up not recognised")
	}
}

func TestProcessNames(t *testing.T) {
	r := load(t)
	if !r.Harness("claude").IsHarnessProcess("claude") || !r.Harness("codex").IsHarnessProcess(`C:\bin\codex.exe`) {
		t.Error("process names not matched")
	}
	if !r.Harness("claude").IsHarnessProcess("2.1.322") {
		t.Error("tmux reports the native Claude Code binary by its version")
	}
	if r.Harness("codex").IsHarnessProcess("2.1.322") {
		t.Error("version-named binary is not codex")
	}
	if r.Harness("claude").IsHarnessProcess("zsh") {
		t.Error("zsh is not claude")
	}
}

func TestOverrideMerge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.json")
	override := `{
	  "defaults": {"delay_after_reset_seconds": 5},
	  "harnesses": {
	    "claude": {"resume_prompt": "keep going"},
	    "aider": {"display_name": "Aider", "process_names": ["aider"],
	              "patterns": [{"kind": "usage_limit", "regex": "(?i)quota gone"}]}
	  },
	  "multiplexers": {"kitty": null}
	}`
	if err := os.WriteFile(path, []byte(override), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if r.Defaults.DelayAfterResetSeconds != 5 || r.Defaults.MaxAttempts != 8 {
		t.Errorf("defaults not merged: %+v", r.Defaults)
	}
	c := r.Harness("claude")
	if c.ResumePrompt != "keep going" || len(c.Patterns) == 0 {
		t.Errorf("claude not merged: prompt=%q patterns=%d", c.ResumePrompt, len(c.Patterns))
	}
	if m := r.Harness("aider").Classify("Error: quota gone", 10); m == nil {
		t.Error("new harness from override not used")
	}
	if _, ok := r.Multiplexers["kitty"]; ok {
		t.Error("null should remove a multiplexer")
	}
}

func TestBrokenOverrideFallsBack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.json")
	os.WriteFile(path, []byte(`{"harnesses": {"claude": {"patterns": [{"kind":"usage_limit","regex":"(unclosed"}]}}}`), 0o600)
	r, err := Load(path)
	if err == nil {
		t.Fatal("expected a warning")
	}
	if r == nil || r.Harness("claude").Classify("You've hit your session limit", 5) == nil {
		t.Fatal("built-in rules should still work")
	}
}

func TestBlockingDialog(t *testing.T) {
	r := load(t)
	blocked := map[string]string{
		"codex update menu": `  Update available · 0.167.0 → 0.167.1
  › 1. Update now (runs ` + "`sh -c ...`" + `)
    2. Skip
    3. Skip until next version
  enter continue · esc skip`,
		"claude approval": ` Bash command
   rm -rf build
 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and don't ask again for rm commands
   3. No, and tell Claude what to do differently (esc)`,
		"claude trust": ` Do you trust the files in this folder?
 ❯ 1. Yes, proceed
   2. No, exit`,
		"codex approval": `  Allow command?
  $ git push --force
  › 1. Yes (y)
    2. No (n)`,
		"shell confirm": `Overwrite existing file? [y/N]`,
	}
	for name, screen := range blocked {
		if r.BlockingDialog(screen) == "" {
			t.Errorf("%s: dialog not detected", name)
		}
	}
	idle := map[string]string{
		"claude idle after limit": `● I'll refactor the parser next.
  ⎿  You've hit your session limit · resets 10am (Australia/Sydney)
╭──────────────────────────────────────────────────────────────╮
│ ❯ Try "write a test for <filepath>"                            │
╰──────────────────────────────────────────────────────────────╯
  ⏵⏵ auto mode on (shift+tab to cycle)`,
		"codex idle after limit": `■ You've hit your usage limit. Try again at 5:53 PM.

› Ask Codex to do anything

  100% context left · ? for shortcuts`,
	}
	for name, screen := range idle {
		if d := r.BlockingDialog(screen); d != "" {
			t.Errorf("%s: false dialog %q", name, d)
		}
	}
}

// A menu without numbers, or with the cursor off the first option, is only
// recognisable across lines; a reply that answers numbered questions with
// yes and no is not a menu.
func TestBlockingDialogAcrossLines(t *testing.T) {
	r := load(t)
	for name, screen := range map[string]string{
		"yes/no menu":        " Apply the patch?\n ❯ Yes\n   No",
		"cursor on option 2": "   1. Yes\n ❯ 2. No, keep it",
		"redrawn, wrapper":   "\n\n  › Yes, continue\n\n    No, stop\n",
		"cursor on option 3": "   1. Yes\n   2. Yes, always\n ❯ 3. No",
	} {
		if r.BlockingDialog(screen) == "" || r.BlockingDialogAnywhere(screen) == "" {
			t.Errorf("%s: menu not detected", name)
		}
	}
	reply := "Answers:\n1. Yes, the cache is per user.\n2. No, it is never shared.\n╭───╮\n│ > │\n╰───╯"
	if d := r.BlockingDialog(reply); d != "" {
		t.Errorf("a reply answering yes and no: false dialog %q", d)
	}
}
