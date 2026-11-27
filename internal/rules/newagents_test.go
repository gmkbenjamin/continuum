package rules

import (
	"strings"
	"testing"
)

func TestNewAgentsWording(t *testing.T) {
	r, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ harness, text, want string }{
		{"antigravity", "You have exhausted your quota on this model.", "usage_limit"},
		{"antigravity", "Error: individual quota reached", "usage_limit"},
		{"antigravity", "⚠ Third-party quota reached. Please switch to Gemini models or upgrade your subscription to increase your limits. Resets in 2h30m50s.", "usage_limit"},
		{"antigravity", "MODEL_CAPACITY_EXHAUSTED: try again", "transient"},
		{"cursor", "You've hit your usage limit", "usage_limit"},
		{"cursor", "You've reached your monthly usage limit", "usage_limit"},
		{"grok", "You've hit the rate limit for your plan. Upgrade your account or try again later.", "usage_limit"},
		{"grok", "Grok is temporarily overloaded. Please try again in a moment. (HTTP 529)", "transient"},
	}
	for _, c := range cases {
		m := r.Harness(c.harness).Classify(c.text, 4)
		if m == nil || m.Kind != c.want {
			t.Errorf("%s %q: %+v, want %s", c.harness, c.text, m, c.want)
		}
	}
	for _, c := range []struct{ harness, text string }{
		{"antigravity", "Your AI credits balance is too low to continue."},
		{"grok", "Your team sign-in was rejected. Run `grok login` to sign in again."},
	} {
		if !r.Harness(c.harness).NeverResume(c.text) {
			t.Errorf("%s %q should need the user", c.harness, c.text)
		}
	}
	if r.Harness("antigravity").NeverResume("⚠ Third-party quota reached. Please switch to Gemini models or upgrade your subscription to increase your limits. Resets in 2h30m50s.") {
		t.Error("antigravity third-party quota reached should be resumable")
	}
	if !r.Harness("claude").IgnoredEntrypoint("sdk-cli") || r.Harness("claude").IgnoredEntrypoint("cli") {
		t.Error("claude print-mode entrypoints")
	}
}

// A prompt character or a box corner alone doesn't prove the agent is on
// screen: a shell prompt shows them too.
func TestStrongUIMarkers(t *testing.T) {
	r, _ := Load("")
	h := r.Harness("claude")
	if h.HasStrongUIMarker("~/work main\n❯ \n╭─") {
		t.Error("a shell prompt counted as Claude")
	}
	if !h.HasStrongUIMarker("╭──╮\n│ > │\n╰──╯\n  ? for shortcuts") {
		t.Error("Claude's input box not recognised")
	}
}

// A menu drawn earlier, then pushed out of the tail by redraws, is still open.
func TestBlockingDialogAnywhere(t *testing.T) {
	r, _ := Load("")
	text := " Do you want to proceed?\n ❯ 1. Yes\n   2. No\n"
	for i := 0; i < 20; i++ {
		text += "⠋ waiting\n"
	}
	if r.BlockingDialog(text) != "" {
		t.Skip("the tail check sees it too")
	}
	if r.BlockingDialogAnywhere(text) == "" {
		t.Fatal("menu not seen")
	}
}

func TestMuseCode(t *testing.T) {
	r, _ := Load("")
	h := r.Harness("muse")
	if h == nil {
		t.Fatal("no muse harness")
	}
	// `muse` is a launcher; the running program is muse-bin-<version>
	// (tmux shows it cut short).
	for _, name := range []string{"muse", "muse-bin-1.4.24-R9131.1", "muse-bin-1.4.24"} {
		if !h.IsHarnessProcess(name) {
			t.Errorf("%s not recognised", name)
		}
	}
	if h.IsHarnessProcess("museum") {
		t.Error("museum taken for Muse Code")
	}
	if !h.MatchesCommandLine("/Users/u/.local/bin/muse-bin-1.4.24-R9131.1 --provider meta") {
		t.Error("command line not recognised")
	}
	for args, want := range map[string]bool{"": true, "resume --last": true, "--model x": true, "exec fix": false, "sessions --json": false, "--version": false, "usage": false} {
		var a []string
		if args != "" {
			a = strings.Fields(args)
		}
		if got := h.Wrap.Wants(a); got != want {
			t.Errorf("wrap %q: %v want %v", args, got, want)
		}
	}
	if !h.HasStrongUIMarker("── Voice input (alt(option)+v to start) ──\n❯\n  meta · ~/w · Auto-review") {
		t.Error("Muse Code's input box not recognised")
	}
	if m := h.Classify("Usage exhausted (last reported)", 4); m == nil || m.Kind != "usage_limit" {
		t.Errorf("usage exhausted: %+v", m)
	}
	// Warnings shown while it keeps working are not stalls.
	for _, s := range []string{"You've used 90% of your weekly quota (resets at 5:00 PM)", "At this pace your weekly quota runs out in about 2 hours (resets at 5:00 PM)", "Usage nearly exhausted", "Usage running low"} {
		if m := h.Classify(s, 4); m != nil {
			t.Errorf("warning %q taken for %s", s, m.Kind)
		}
	}
	if !h.NeverResume("not logged in: run `muse login` first") {
		t.Error("login error should need the user")
	}
}

func TestCursorAgentAlias(t *testing.T) {
	r, _ := Load("")
	h := r.Harness("cursor")
	if len(h.CommandAliases) == 0 || h.CommandAliases[0].Command != "agent" {
		t.Fatalf("aliases %+v", h.CommandAliases)
	}
	a := h.CommandAliases[0]
	if !a.Matches("/Users/u/.local/share/cursor-agent/versions/2026.11.17-f532c45/cursor-agent") {
		t.Error("Cursor's install not matched")
	}
	if !a.Matches(`C:\Users\u\AppData\Local\cursor-agent\agent.cmd`) {
		t.Error("Windows path not matched")
	}
	if a.Matches("/usr/local/bin/agent") || a.Matches("/opt/other/agent") {
		t.Error("another program named agent matched")
	}
	// `agent ls` opens a session picker that resumes a chat: wrapped.
	if !h.Wrap.Wants([]string{"ls"}) || h.Wrap.Wants([]string{"-p", "hi"}) || h.Wrap.Wants([]string{"models"}) {
		t.Error("cursor wrap rules")
	}
}
