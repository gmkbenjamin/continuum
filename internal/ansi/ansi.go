// Package ansi turns raw terminal output into plain text good enough for
// pattern matching. Cursor movement is approximated: moves to another row
// become newlines, moves to the right become spaces.
package ansi

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Strip removes escape sequences from terminal output.
func Strip(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b:
			i = escape(s, i, &b)
		case c == 0x9b: // 8-bit CSI
			i = csi(s, i+1, &b)
		case c == '\r':
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
				continue
			}
			b.WriteByte('\n')
			i++
		case c == '\n' || c == '\t':
			b.WriteByte(c)
			i++
		case c < 0x20 || c == 0x7f:
			i++
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			if r == utf8.RuneError && size <= 1 {
				i++
				continue
			}
			b.WriteString(s[i : i+size])
			i += size
		}
	}
	return b.String()
}

func escape(s string, i int, b *strings.Builder) int {
	if i+1 >= len(s) {
		return len(s)
	}
	switch s[i+1] {
	case '[':
		return csi(s, i+2, b)
	case ']', 'P', '_', '^', 'X': // OSC, DCS, APC, PM, SOS: until BEL or ST
		j := i + 2
		for j < len(s) {
			if s[j] == 0x07 {
				return j + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
			j++
		}
		return len(s)
	case '(', ')', '*', '+', '#', '%':
		return min(i+3, len(s))
	case 'E', 'D', 'M':
		b.WriteByte('\n')
		return i + 2
	default:
		return i + 2
	}
}

func csi(s string, j int, b *strings.Builder) int {
	start := j
	for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
		j++
	}
	if j >= len(s) {
		return len(s)
	}
	params := s[start:j]
	switch s[j] {
	case 'C': // cursor forward: usually stands in for spaces
		n := 1
		if v, err := strconv.Atoi(params); err == nil && v > 0 {
			n = v
		}
		if n > 200 {
			n = 200
		}
		b.WriteString(strings.Repeat(" ", n))
	case 'A', 'B', 'E', 'F', 'H', 'f', 'd':
		b.WriteByte('\n')
	case 'J':
		if params == "2" || params == "3" {
			b.WriteByte('\n')
		}
	}
	return j + 1
}

// Ring keeps the last N bytes of text.
type Ring struct {
	max int
	buf []byte
}

func NewRing(max int) *Ring { return &Ring{max: max} }

func (r *Ring) Write(p []byte) {
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.max {
		cut := len(r.buf) - r.max
		// Do not split a UTF-8 sequence.
		for cut < len(r.buf) && !utf8.RuneStart(r.buf[cut]) {
			cut++
		}
		r.buf = append([]byte(nil), r.buf[cut:]...)
	}
}

func (r *Ring) String() string { return string(r.buf) }
