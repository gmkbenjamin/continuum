package ansi

import (
	"strings"
	"testing"
)

func lines(s *Screen) []string { return strings.Split(s.String(), "\n") }

func TestScreenCursorDrawing(t *testing.T) {
	s := NewScreen(20, 5)
	// Drawn out of order by moving the cursor, as full-screen programs do.
	s.Write([]byte("\x1b[2J\x1b[3;5Hworld\x1b[3;1Hhey\x1b[1;1Htop\x1b[5;1Hbottom"))
	got := lines(s)
	want := []string{"top", "", "hey world", "", "bottom"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("row %d = %q want %q\n%s", i, got[i], want[i], s.String())
		}
	}
	// Erase to end of line, then redraw part of a row.
	s.Write([]byte("\x1b[3;4H\x1b[Kthere"))
	if l := lines(s)[2]; l != "heythere" {
		t.Fatalf("after erase: %q", l)
	}
}

func TestScreenScrollsAndWraps(t *testing.T) {
	s := NewScreen(5, 3)
	s.Write([]byte("abcdefg\r\nline2\r\nline3"))
	got := lines(s)
	if got[0] != "fg" || got[1] != "line2" || got[2] != "line3" {
		t.Fatalf("got %q", got)
	}
	s.Write([]byte("\r\nline4"))
	if got := lines(s); got[0] != "line2" || got[2] != "line4" {
		t.Fatalf("after scroll %q", got)
	}
}

func TestScreenScrollRegionAndLines(t *testing.T) {
	s := NewScreen(10, 4)
	s.Write([]byte("\x1b[4;1Hstatus\x1b[1;3r\x1b[1;1Ha\r\nb\r\nc\r\nd"))
	got := lines(s)
	if got[0] != "b" || got[1] != "c" || got[2] != "d" || got[3] != "status" {
		t.Fatalf("scroll region: %q", got)
	}
	s.Write([]byte("\x1b[r\x1b[2;1H\x1b[M")) // delete line 2
	if got := lines(s); got[1] != "d" || got[3] != "" || got[2] != "status" {
		t.Fatalf("delete line: %q", got)
	}
}

func TestScreenAlternateScreen(t *testing.T) {
	s := NewScreen(10, 3)
	s.Write([]byte("$ agent"))
	s.Write([]byte("\x1b[?1049h\x1b[H\x1b[2Jfull"))
	if l := lines(s); l[0] != "full" {
		t.Fatalf("alt: %q", l)
	}
	s.Write([]byte("\x1b[?1049l"))
	if l := lines(s); l[0] != "$ agent" {
		t.Fatalf("back: %q", l)
	}
}

// Output arrives in arbitrary chunks: an escape sequence or a character can
// be cut in two.
func TestScreenSplitWrites(t *testing.T) {
	whole := "\x1b[2;3H\x1b[1mlimit ─ reached\x1b[0m\x1b]0;title\x07!"
	for cut := 1; cut < len(whole); cut++ {
		s := NewScreen(30, 3)
		s.Write([]byte(whole[:cut]))
		s.Write([]byte(whole[cut:]))
		if l := lines(s)[1]; l != "  limit ─ reached!" {
			t.Fatalf("cut at %d: %q", cut, l)
		}
	}
}

func TestScreenResizeKeepsTheBottom(t *testing.T) {
	s := NewScreen(10, 4)
	s.Write([]byte("1\r\n2\r\n3\r\n4"))
	s.Resize(6, 2)
	if got := lines(s); len(got) != 2 || got[0] != "3" || got[1] != "4" {
		t.Fatalf("got %q", got)
	}
	s.Write([]byte("\x1b[1;1Hxx")) // still usable
	if lines(s)[0] != "xx" {
		t.Fatal(s.String())
	}
}

// Garbage never panics or grows without bound.
func TestScreenRobust(t *testing.T) {
	s := NewScreen(4, 2)
	s.Write([]byte("\x1b[999;999H\x1b[999P\x1b[999@\x1b[999L\x1b[999M\x1b[0;0r\x1b[5;2r\x1b[99X\x1b8\x1bc\x9b2J\xff\xfe"))
	s.Write([]byte("\x1b[" + strings.Repeat("1;", 400)))
	s.Write([]byte("ok"))
	if len(s.pending) > 4096 {
		t.Fatal("pending grew")
	}
}

// A saved cursor below the bottom after the screen shrank (opencode quits
// its alternate screen after the window got shorter) must not crash.
func TestScreenSavedCursorAfterShrinking(t *testing.T) {
	for _, seq := range []string{"\x1b[?1049h", "\x1b7", "\x1b[s"} {
		s := NewScreen(80, 24)
		s.Write([]byte("\x1b[24;80H" + seq))
		s.Resize(20, 10)
		s.Write([]byte("\x1b[?1049lX\x1b8X\x1b[uX"))
		if !strings.Contains(s.String(), "X") {
			t.Fatalf("%q: %q", seq, s.String())
		}
	}
}

func TestScreenWideCharacters(t *testing.T) {
	s := NewScreen(20, 2)
	s.Write([]byte("限制🙂ok\x1b[1;9Hz"))
	if l := lines(s)[0]; l != "限制🙂okz" {
		t.Fatalf("%q", l)
	}
	s = NewScreen(3, 2) // a wide character that doesn't fit wraps whole
	s.Write([]byte("ab限"))
	if got := lines(s); got[0] != "ab" || got[1] != "限" {
		t.Fatalf("%q", got)
	}
}

// Fuzz writes and resizes: never a panic.
func TestScreenNeverPanics(t *testing.T) {
	seed := uint32(1)
	rnd := func(n int) int { seed = seed*1664525 + 1013904223; return int(seed>>8) % n }
	parts := []string{"\x1b[", ";", "H", "J", "K", "r", "s", "u", "L", "M", "P", "@", "X", "S", "T", "A", "B", "C", "D", "G", "d", "?1049h", "?1049l", "?47h", "\x1b7", "\x1b8", "\x1bM", "\x1bD", "\x1bE", "\x1bc", "\r", "\n", "\b", "\t", "x", "限", "🙂", "́", "9", "99", "0", "\x9b", "\xff"}
	for round := 0; round < 3000; round++ {
		s := NewScreen(1+rnd(30), 1+rnd(12))
		for k := 0; k < 60; k++ {
			if rnd(15) == 0 {
				s.Resize(rnd(30), rnd(12))
			}
			var b strings.Builder
			for j := rnd(8); j >= 0; j-- {
				b.WriteString(parts[rnd(len(parts))])
			}
			s.Write([]byte(b.String()))
		}
		_ = s.String()
	}
}
