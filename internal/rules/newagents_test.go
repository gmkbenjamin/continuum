package rules

import "testing"

func TestNewAgentsWording(t *testing.T) {
	r, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ harness, text, want string }{
		{"antigravity", "You have exhausted your quota on this model.", "usage_limit"},
		{"antigravity", "Error: individual quota reached", "usage_limit"},
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
