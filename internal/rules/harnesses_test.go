package rules

import "testing"

func TestEveryHarnessIsUsable(t *testing.T) {
	r, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if n := len(r.HarnessNames()); n < 25 {
		t.Fatalf("only %d harnesses", n)
	}
	for _, name := range r.HarnessNames() {
		h := r.Harness(name)
		if h.DisplayName == "" {
			t.Errorf("%s: no display name", name)
		}
		if len(h.Patterns) == 0 {
			t.Errorf("%s: no limit patterns", name)
		}
		if name != "generic" && len(h.ProcessNames) == 0 && len(h.ProcessArgPatterns) == 0 {
			t.Errorf("%s: cannot be recognised in a pane", name)
		}
		// Every harness must at least recognise the common wording (Claude
		// and Codex use their own exact messages, from their transcripts).
		if name != "claude" && name != "codex" && h.Classify("Error: quota exceeded. Try again in 2 hours.", 5) == nil {
			t.Errorf("%s: common usage-limit wording not recognised", name)
		}
		if !h.NeverResume("Your credit balance is too low to continue") {
			t.Errorf("%s: billing wording not treated as needing the user", name)
		}
	}
}

func TestAgentSpecificWording(t *testing.T) {
	r, _ := Load("")
	cases := []struct{ harness, text, kind string }{
		{"gemini", "Quota exceeded for quota metric 'Gemini 2.5 Pro Requests' and limit 'Requests per day'", "usage_limit"},
		{"gemini", "You have exhausted your daily quota on this model.", "usage_limit"},
		{"gemini", "[API Error: RESOURCE_EXHAUSTED]", "usage_limit"},
		{"aider", "litellm.RateLimitError: AnthropicException - rate limited", "transient"},
		{"copilot", "You've reached your monthly limit of premium requests.", "usage_limit"},
		{"cursor", "You've hit your usage limit for this billing period", "usage_limit"},
		{"opencode", "429 Too Many Requests: rate limit", "transient"},
		{"grok", "You've reached your usage limit. Your limit resets at 4:00 PM.", "usage_limit"},
		{"qwen", "Free tier quota exceeded, please try again tomorrow", "usage_limit"},
	}
	for _, c := range cases {
		m := r.Harness(c.harness).Classify(c.text, 5)
		if m == nil || m.Kind != c.kind {
			t.Errorf("%s: %q -> %+v, want %s", c.harness, c.text, m, c.kind)
		}
	}
	// Ordinary output mentioning limits is not a stall.
	for _, text := range []string{"I added a rate limiter to the API client.", "Tests passed: 42"} {
		if m := r.Harness("gemini").Classify(text, 5); m != nil {
			t.Errorf("false positive on %q: %+v", text, m)
		}
	}
}

func TestShellWrapDecisions(t *testing.T) {
	r, _ := Load("")
	cases := []struct {
		harness string
		args    []string
		want    bool
	}{
		{"gemini", nil, true},
		{"gemini", []string{"-p", "hi"}, false},
		{"gemini", []string{"-i", "start here"}, true},
		{"gemini", []string{"mcp", "list"}, false},
		{"opencode", nil, true},
		{"opencode", []string{"run", "fix it"}, false},
		{"opencode", []string{"mini"}, true},
		{"opencode", []string{"--session", "abc"}, true},
		{"grok", []string{"fix the bug"}, true},
		{"grok", []string{"-p", "x"}, false},
		{"grok", []string{"sessions"}, false},
		{"goose", nil, true},
		{"goose", []string{"session"}, true},
		{"goose", []string{"run", "-t", "x"}, false},
		{"kiro", []string{"chat"}, true},
		{"kiro", []string{"login"}, false},
		{"plandex", []string{"tell", "x"}, false},
		{"aider", []string{"--model", "sonnet"}, true},
		{"aider", []string{"-m", "fix"}, false},
		{"codex", []string{"resume", "--last"}, true},
		{"codex", []string{"exec", "x"}, false},
	}
	for _, c := range cases {
		if got := r.Harness(c.harness).Wrap.Wants(c.args); got != c.want {
			t.Errorf("%s %v: wrap=%v, want %v", c.harness, c.args, got, c.want)
		}
	}
}

func TestInterpreterAgents(t *testing.T) {
	r, _ := Load("")
	for _, n := range []string{"node", "python3.12", "bun", "/usr/bin/python3"} {
		if !r.IsInterpreter(n) {
			t.Errorf("%s not an interpreter", n)
		}
	}
	if r.IsInterpreter("nodemon") || r.IsInterpreter("zsh") {
		t.Error("false interpreter")
	}
	lines := map[string]string{
		"gemini":  "node /opt/homebrew/lib/node_modules/@google/gemini-cli/dist/index.js",
		"copilot": "node /usr/local/lib/node_modules/@github/copilot/index.js",
		"amp":     "node /Users/x/.npm-global/lib/node_modules/@sourcegraph/amp/dist/main.js",
		"aider":   "/Users/x/.local/share/uv/tools/aider-chat/bin/python /Users/x/.local/bin/aider --model sonnet",
		"pi":      "node /usr/lib/node_modules/@mariozechner/pi-coding-agent/dist/cli.js",
	}
	for want, line := range lines {
		if !r.Harness(want).MatchesCommandLine(line) {
			t.Errorf("%s not recognised from %q", want, line)
		}
	}
	if r.Harness("gemini").MatchesCommandLine("node server.js") {
		t.Error("gemini matched an unrelated node process")
	}
}

func TestAmbiguousCommandsStayOutOfTheShellHook(t *testing.T) {
	r, _ := Load("")
	for _, name := range []string{"amp", "goose", "pi", "droid", "crush", "vibe", "continue", "rovodev"} {
		if r.Harness(name).InShellHook() {
			t.Errorf("%s would shadow other tools of the same name", name)
		}
	}
	for _, name := range []string{"claude", "codex", "gemini", "opencode", "grok", "cursor", "copilot", "aider"} {
		if !r.Harness(name).InShellHook() {
			t.Errorf("%s missing from the shell hook", name)
		}
	}
}

func TestMoreMenusAreRefused(t *testing.T) {
	r, _ := Load("")
	menus := []string{
		// Grok's folder-trust screen: a single "y" would pick "Yes, proceed".
		"Grok Build may run or modify contents in this directory,\n posing security risks.\n   Yes, proceed                 y\n   No, quit                     n",
		// Claude's folder-trust screen.
		" ❯ No, exit\n   Yes, I trust this folder\n Enter to confirm · Esc to cancel",
		"Allow this command?\n  allow  a\n  deny   d",
	}
	for _, m := range menus {
		if r.BlockingDialog(m) == "" {
			t.Errorf("menu not refused:\n%s", m)
		}
	}
	// An ordinary idle agent screen is not a menu.
	for _, ok := range []string{
		"You have exhausted your daily quota. Please try again in 3s.\n\n> Type your message or @path/to/file",
		"Yes, the tests pass now.\n> ",
		"No errors found.\n› ",
	} {
		if d := r.BlockingDialog(ok); d != "" {
			t.Errorf("false menu %q in:\n%s", d, ok)
		}
	}
}
