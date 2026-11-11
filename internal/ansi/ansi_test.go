package ansi

import (
	"strings"
	"testing"
)

func TestStrip(t *testing.T) {
	in := "\x1b[?25l\x1b[2K\x1b[1G\x1b[38;5;174mYou've\x1b[1Chit\x1b[1Cyour\x1b[1Csession\x1b[1Climit\x1b[39m · resets 10am\x1b]0;title\x07\r\n\x1b[2Cnext"
	got := Strip(in)
	if !strings.Contains(got, "You've hit your session limit · resets 10am") {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(got, "title") || strings.Contains(got, "\x1b") {
		t.Fatalf("escape left in %q", got)
	}
	if !strings.Contains(got, "\n  next") {
		t.Fatalf("line structure lost: %q", got)
	}
}

func TestRingKeepsTail(t *testing.T) {
	r := NewRing(10)
	r.Write([]byte("hello "))
	r.Write([]byte("wonderful world"))
	if s := r.String(); s != "rful world" {
		t.Fatalf("got %q", s)
	}
	r = NewRing(5)
	r.Write([]byte("ab·cd·")) // multi-byte middle dot
	if s := r.String(); !strings.HasSuffix(s, "·") || strings.ContainsRune(s, '�') {
		t.Fatalf("split rune: %q", s)
	}
}
