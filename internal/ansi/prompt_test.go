package ansi

import "testing"

func TestScreenBracketedPasteAndPromptDraft(t *testing.T) {
	s := NewScreen(80, 20)
	if s.BracketedPaste() {
		t.Fatal("expected bracketed paste off initially")
	}
	s.Write([]byte("\x1b[?2004h"))
	if !s.BracketedPaste() {
		t.Fatal("expected bracketed paste on after ?2004h")
	}
	s.Write([]byte("\x1b[?2004l"))
	if s.BracketedPaste() {
		t.Fatal("expected bracketed paste off after ?2004l")
	}

	// Empty prompt with dim placeholder text (Claude/Codex style) must return "".
	s = NewScreen(80, 12)
	s.Write([]byte("────────────────────────\r\n❯ \x1b[2mTry \"refactor auth\"\x1b[0m\r\n────────────────────────\r\n? for shortcuts"))
	if got, _ := s.PromptDraft(); got != "" {
		t.Fatalf("dim placeholder detected as draft: %q", got)
	}

	// Gray (SGR 90 / 256-color) placeholder must also return "".
	s = NewScreen(80, 12)
	s.Write([]byte("› \x1b[90mAsk Codex anything\x1b[0m\r\n  gpt-6.2"))
	if got, _ := s.PromptDraft(); got != "" {
		t.Fatalf("gray SGR 90 placeholder detected as draft: %q", got)
	}
	s = NewScreen(80, 12)
	s.Write([]byte("› \x1b[38;5;242mSummarize commits\x1b[0m"))
	if got, _ := s.PromptDraft(); got != "" {
		t.Fatalf("256-color gray placeholder detected as draft: %q", got)
	}

	// Wrapped Antigravity-style user draft above footer must be extracted cleanly.
	s = NewScreen(80, 14)
	s.Write([]byte(
		"────────────────────────────────────────────────────────────────────────────────\r\n" +
			"> when click on banked reset, take user to the page where they can use the\r\n" +
			"  banked reset.\r\n" +
			"────────────────────────────────────────────────────────────────────────────────\r\n" +
			"? for shortcuts\r\n" +
			"~/Documents/projects/resetcalendar",
	))
	want := "when click on banked reset, take user to the page where they can use the\nbanked reset."
	if got, _ := s.PromptDraft(); got != want {
		t.Fatalf("PromptDraft() = %q, want %q", got, want)
	}

	// Empty active prompt below an older prompt must return "".
	s = NewScreen(80, 14)
	s.Write([]byte(
		"> old submitted prompt\r\n" +
			"⚠ Individual quota reached.\r\n" +
			"────────────────────────────────────────\r\n" +
			"> \r\n" +
			"────────────────────────────────────────\r\n" +
			"? for shortcuts",
	))
	if got, _ := s.PromptDraft(); got != "" {
		t.Fatalf("empty active prompt returned old prompt: %q", got)
	}
}

func TestPromptDraftStyles(t *testing.T) {
	for _, tc := range []struct{ row, want string }{
		{"❯\u00a0\x1b[48;2;11;12;13m\x1b[58;5;22m\x1b[>4;2mreal text", "real text"},
		{"❯ \x1b[38;2;255;255;255;1mreal text", "real text"},
		{"❯ \x1b[38:2::100:116:139mPlaceholder", ""},
		{"❯ \x1b[7mP\x1b[27;2mlaceholder", ""},
		{"┃ actual draft", "actual draft"},
	} {
		s := NewScreen(60, 10)
		s.Write([]byte(tc.row))
		if got, found := s.PromptDraft(); !found || got != tc.want {
			t.Errorf("%q: draft %q, found %t; want %q", tc.row, got, found, tc.want)
		}
	}
}
