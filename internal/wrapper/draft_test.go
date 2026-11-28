package wrapper

import (
	"bytes"
	"testing"
)

func TestDraftBufferEditingAndCursorInMiddle(t *testing.T) {
	var d draftBuffer
	// Type "when click on baned reset", move left 8, insert "k" -> cursor between "bank" and "ed".
	if d.Feed([]byte("when click on baned reset")) {
		t.Fatal("typing should not submit")
	}
	for range 8 { // move left past "ed reset" -> after "ban"
		d.Feed([]byte("\x1b[D"))
	}
	d.Feed([]byte("k"))
	if got, want := d.Text(), "when click on banked reset"; got != want {
		t.Fatalf("Text() = %q, want %q", got, want)
	}
	if d.Uncertain() {
		t.Fatal("normal typing/arrows should not mark uncertain")
	}

	// Focus events must not disturb the draft or mark uncertain.
	d.Feed([]byte("\x1b[O\x1b[I"))
	if d.Uncertain() || d.Text() != "when click on banked reset" {
		t.Fatalf("focus event corrupted draft: uncertain=%v text=%q", d.Uncertain(), d.Text())
	}

	// Ctrl+K kills to end of line; Ctrl+A + Ctrl+D deletes first char; Ctrl+W kills previous word.
	d.Feed([]byte("\x0b")) // kills "ed reset"
	if got := d.Text(); got != "when click on bank" {
		t.Fatalf("after Ctrl+K: %q", got)
	}
	d.Feed([]byte("\x17")) // kills "bank"
	if got := d.Text(); got != "when click on " {
		t.Fatalf("after Ctrl+W: %q", got)
	}
	d.Feed([]byte("\x15")) // Ctrl+U kills to start of line
	if got := d.Text(); got != "" {
		t.Fatalf("after Ctrl+U: %q", got)
	}
}

func TestDraftBufferBracketedPasteAndMultiline(t *testing.T) {
	var d draftBuffer
	// Bracketed paste split across reads with newlines must NOT submit.
	if d.Feed([]byte("\x1b[200~line one\r\nli")) {
		t.Fatal("paste chunk 1 should not submit")
	}
	if d.Feed([]byte("ne two\x1b[20")) {
		t.Fatal("paste chunk 2 should not submit")
	}
	if d.Feed([]byte("1~")) {
		t.Fatal("paste end should not submit")
	}
	if got, want := d.Text(), "line one\nline two"; got != want {
		t.Fatalf("pasted Text() = %q, want %q", got, want)
	}

	// Shift+Enter and Alt+Enter insert newlines without submitting.
	if d.Feed([]byte("\x1b[13;2u")) {
		t.Fatal("Shift+Enter should not submit")
	}
	d.Feed([]byte("line three"))
	if d.Feed([]byte("\x1b\r")) {
		t.Fatal("Alt+Enter should not submit")
	}
	d.Feed([]byte("line four"))
	want := "line one\nline two\nline three\nline four"
	if got := d.Text(); got != want {
		t.Fatalf("multiline Text() = %q, want %q", got, want)
	}

	// Plain Enter submits and resets.
	if !d.Feed([]byte("\r")) {
		t.Fatal("Enter should submit")
	}
	if d.Text() != "" || d.Uncertain() {
		t.Fatalf("after submit: text=%q uncertain=%v", d.Text(), d.Uncertain())
	}
}

func TestDraftBufferUncertainKeys(t *testing.T) {
	for _, seq := range [][]byte{[]byte("\x1b[A"), []byte("\x1b[B"), []byte("\t"), []byte{0x1b}} {
		var d draftBuffer
		d.Feed([]byte("hi"))
		d.Feed(seq)
		if !d.Uncertain() && len(d.pending) == 0 {
			t.Errorf("sequence %q should mark draft uncertain", seq)
		}
	}
}

func TestFormatDraftForWrite(t *testing.T) {
	for _, tc := range []struct {
		draft          string
		bracketedPaste bool
		want           string
	}{
		{"simple draft", false, "simple draft"},
		{"line 1\nline 2", true, "\x1b[200~line 1\nline 2\x1b[201~"},
		{"col 1\tcol 2", true, "\x1b[200~col 1\tcol 2\x1b[201~"},
		{"line 1\nline 2", false, ""},
		{"col 1\tcol 2", false, ""},
	} {
		if got := formatDraftForWrite(tc.draft, tc.bracketedPaste); !bytes.Equal(got, []byte(tc.want)) {
			t.Errorf("draft %q, bracketed paste %v: got %q, want %q", tc.draft, tc.bracketedPaste, got, tc.want)
		}
	}
}

func TestDraftBufferIgnoresTerminalReplies(t *testing.T) {
	var d draftBuffer
	d.Feed([]byte("draft"))
	for _, chunk := range []string{
		"\x1b", "]11;rgb:1e1e/1e1e/2e2e\x1b", "\\\x1b[1;2R\x1b[I",
		"\x1bP>|" + string(bytes.Repeat([]byte{'x'}, 100)), "\x1b\\",
	} {
		if d.Feed([]byte(chunk)) || d.userInput || d.Uncertain() || d.Text() != "draft" {
			t.Fatalf("reply %q disrupted draft tracking: %+v", chunk, d)
		}
	}
	d.Feed([]byte("!"))
	if d.Text() != "draft!" || !d.userInput {
		t.Fatalf("ordinary input after replies: %+v", d)
	}
}

func TestDraftBufferMultilineEnterAndSplitCRLF(t *testing.T) {
	var d draftBuffer
	for _, chunk := range []string{"\x1b[200~first\r", "\nsecond\x1b[201~", "\nthird", "\x1b[13;2:1ufourth"} {
		if d.Feed([]byte(chunk)) {
			t.Fatalf("Unexpected submit in %q", chunk)
		}
	}
	if d.Text() != "first\nsecond\nthird\nfourth" || d.Uncertain() {
		t.Fatalf("multiline draft: %+v", d)
	}
	if !d.Feed([]byte("\x1b[13;1:1u")) || d.Text() != "" {
		t.Fatalf("Kitty Enter did not submit: %+v", d)
	}
}
