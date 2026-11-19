package ansi

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// Screen is a small terminal emulator: enough of VT100/xterm (cursor
// positioning, erasing, scrolling, the alternate screen) to know what a
// full-screen program shows. Programs that redraw by moving the cursor
// (opencode, most TUI frameworks) leave text in their output stream as
// fragments in drawing order, with a spinner or clock redrawn many times
// after it; the rendered screen holds each line once, where it is shown.
// Colours, character sets and modes it doesn't know are ignored.
type Screen struct {
	cols, rows int
	grid       [][]rune
	x, y       int
	wrapNext   bool // the cursor is past the last column; the next character wraps
	top, bot   int  // scroll region, inclusive
	savedX     int
	savedY     int
	main       [][]rune // the main screen while the alternate one is shown
	pending    []byte   // an incomplete escape sequence or UTF-8 character
}

// NewScreen returns a blank screen of cols by rows.
func NewScreen(cols, rows int) *Screen {
	s := &Screen{}
	s.Resize(cols, rows)
	return s
}

func blankRow(cols int) []rune {
	r := make([]rune, cols)
	for i := range r {
		r[i] = ' '
	}
	return r
}

// Resize changes the size, keeping what fits (the bottom rows when it
// shrinks, as terminals do).
func (s *Screen) Resize(cols, rows int) {
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	if cols == s.cols && rows == s.rows {
		return
	}
	fit := func(g [][]rune) [][]rune {
		if g == nil {
			return nil
		}
		if len(g) > rows {
			g = g[len(g)-rows:]
		}
		out := make([][]rune, rows)
		for i := range out {
			out[i] = blankRow(cols)
			if i < len(g) {
				copy(out[i], g[i])
			}
		}
		return out
	}
	shift := 0
	if len(s.grid) > rows {
		shift = len(s.grid) - rows
	}
	s.grid = fit(s.grid)
	if s.grid == nil {
		s.grid = fit(make([][]rune, 0))
	}
	s.main = fit(s.main)
	s.cols, s.rows = cols, rows
	s.top, s.bot = 0, rows-1
	s.y -= shift
	s.x, s.y = clamp(s.x, 0, cols-1), clamp(s.y, 0, rows-1)
	s.savedX, s.savedY = clamp(s.savedX, 0, cols-1), clamp(s.savedY-shift, 0, rows-1)
	s.wrapNext = false
}

// restoreCursor moves to the saved position, kept on the screen.
func (s *Screen) restoreCursor() {
	s.x, s.y = clamp(s.savedX, 0, s.cols-1), clamp(s.savedY, 0, s.rows-1)
	s.wrapNext = false
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// String is the screen's text, one line per row, trailing spaces removed.
func (s *Screen) String() string {
	var b strings.Builder
	for i, row := range s.grid {
		if i > 0 {
			b.WriteByte('\n')
		}
		line := strings.ReplaceAll(string(row), string(wideTail), "")
		b.WriteString(strings.TrimRight(line, " "))
	}
	return b.String()
}

// Write feeds terminal output to the screen.
func (s *Screen) Write(p []byte) {
	data := p
	if len(s.pending) > 0 {
		data = append(s.pending, p...)
		s.pending = nil
	}
	for i := 0; i < len(data); {
		c := data[i]
		switch {
		case c == 0x1b:
			n, ok := s.escape(data[i:])
			if !ok {
				s.keep(data[i:])
				return
			}
			i += n
		case c == '\r':
			s.x, s.wrapNext = 0, false
			i++
		case c == '\n' || c == 0x0b || c == 0x0c:
			s.lineFeed()
			i++
		case c == '\b':
			if s.x > 0 {
				s.x--
			}
			s.wrapNext = false
			i++
		case c == '\t':
			s.x = clamp((s.x/8+1)*8, 0, s.cols-1)
			i++
		case c < 0x20 || c == 0x7f:
			i++
		default:
			if !utf8.FullRune(data[i:]) {
				s.keep(data[i:])
				return
			}
			r, size := utf8.DecodeRune(data[i:])
			if r == utf8.RuneError && size <= 1 {
				i++
				continue
			}
			s.put(r)
			i += size
		}
	}
}

// keep holds an incomplete sequence for the next Write, within reason: a
// sequence that never ends is dropped rather than grown forever.
func (s *Screen) keep(rest []byte) {
	if len(rest) < 4096 {
		s.pending = append([]byte(nil), rest...)
	}
}

func (s *Screen) put(r rune) {
	if isCombining(r) {
		return // drawn on the previous cell; matching doesn't need it
	}
	w := 1
	if isWide(r) && s.cols > 1 {
		w = 2
	}
	if s.wrapNext || s.x+w > s.cols {
		s.x = 0
		s.lineFeed()
		s.wrapNext = false
	}
	s.grid[s.y][s.x] = r
	if w == 2 {
		s.grid[s.y][s.x+1] = wideTail
	}
	if s.x+w >= s.cols {
		s.x = s.cols - 1
		s.wrapNext = true
	} else {
		s.x += w
	}
}

// wideTail fills the second column of a wide character; String leaves it out.
const wideTail = rune(0)

// isWide reports characters terminals draw two columns wide (East Asian wide
// and full-width forms, most emoji).
func isWide(r rune) bool {
	switch {
	case r < 0x1100:
		return false
	case r <= 0x115F, r >= 0x2E80 && r <= 0x303E, r >= 0x3041 && r <= 0x33FF,
		r >= 0x3400 && r <= 0x4DBF, r >= 0x4E00 && r <= 0x9FFF, r >= 0xA000 && r <= 0xA4CF,
		r >= 0xAC00 && r <= 0xD7A3, r >= 0xF900 && r <= 0xFAFF, r >= 0xFE30 && r <= 0xFE4F,
		r >= 0xFF00 && r <= 0xFF60, r >= 0xFFE0 && r <= 0xFFE6,
		r >= 0x1F300 && r <= 0x1F64F, r >= 0x1F900 && r <= 0x1F9FF, r >= 0x1FA70 && r <= 0x1FAFF,
		r >= 0x20000 && r <= 0x3FFFD:
		return true
	}
	return false
}

// isCombining reports zero-width marks and joiners.
func isCombining(r rune) bool {
	return r >= 0x0300 && r <= 0x036F || r == 0x200B || r == 0x200C || r == 0x200D ||
		r >= 0xFE00 && r <= 0xFE0F || r >= 0x20D0 && r <= 0x20FF
}

func (s *Screen) lineFeed() {
	s.wrapNext = false
	if s.y == s.bot {
		s.scrollUp(1)
	} else if s.y < s.rows-1 {
		s.y++
	}
}

func (s *Screen) scrollUp(n int) {
	for ; n > 0; n-- {
		copy(s.grid[s.top:s.bot], s.grid[s.top+1:s.bot+1])
		s.grid[s.bot] = blankRow(s.cols)
	}
}

func (s *Screen) scrollDown(n int) {
	for ; n > 0; n-- {
		copy(s.grid[s.top+1:s.bot+1], s.grid[s.top:s.bot])
		s.grid[s.top] = blankRow(s.cols)
	}
}

func (s *Screen) clearRow(y, from, to int) {
	from, to = clamp(from, 0, s.cols), clamp(to, 0, s.cols)
	for x := from; x < to; x++ {
		s.grid[y][x] = ' '
	}
}

// escape handles ESC sequences; it returns the bytes used, or false when the
// sequence is not complete yet.
func (s *Screen) escape(d []byte) (int, bool) {
	if len(d) < 2 {
		return 0, false
	}
	switch d[1] {
	case '[':
		n, ok := s.csi(d[2:])
		return 2 + n, ok
	case ']', 'P', '_', '^', 'X': // OSC, DCS, APC, PM, SOS: until BEL or ST
		for j := 2; j < len(d); j++ {
			if d[j] == 0x07 {
				return j + 1, true
			}
			if d[j] == 0x1b && j+1 < len(d) && d[j+1] == '\\' {
				return j + 2, true
			}
		}
		return 0, false
	case '(', ')', '*', '+', '-', '.', '/', '#', '%', ' ': // charset and similar: one more byte
		if len(d) < 3 {
			return 0, false
		}
		return 3, true
	case '7':
		s.savedX, s.savedY = s.x, s.y
	case '8':
		s.restoreCursor()
	case 'D':
		s.lineFeed()
	case 'E':
		s.x = 0
		s.lineFeed()
	case 'M':
		s.wrapNext = false
		if s.y == s.top {
			s.scrollDown(1)
		} else if s.y > 0 {
			s.y--
		}
	case 'c':
		cols, rows := s.cols, s.rows
		*s = Screen{}
		s.Resize(cols, rows)
	}
	return 2, true
}

// csi handles a control sequence after "ESC [".
func (s *Screen) csi(d []byte) (int, bool) {
	j := 0
	for j < len(d) && (d[j] < 0x40 || d[j] > 0x7e) {
		j++
	}
	if j >= len(d) {
		if j > 256 {
			return j, true // not a sequence after all: drop it
		}
		return 0, false
	}
	final := d[j]
	raw := string(d[:j])
	private := strings.HasPrefix(raw, "?")
	raw = strings.TrimLeft(raw, "?<=>")
	var params []int
	for _, f := range strings.Split(raw, ";") {
		if k := strings.IndexByte(f, ':'); k >= 0 {
			f = f[:k]
		}
		n, _ := strconv.Atoi(strings.TrimSpace(f))
		params = append(params, n)
	}
	arg := func(i, def int) int {
		if i < len(params) && params[i] > 0 {
			return params[i]
		}
		return def
	}
	if private {
		if final == 'h' || final == 'l' {
			for _, m := range params {
				if m == 1049 || m == 1047 || m == 47 {
					s.altScreen(final == 'h', m == 1049)
				}
			}
		}
		return j + 1, true
	}
	switch final {
	case 'A':
		s.y = clamp(s.y-arg(0, 1), 0, s.rows-1)
	case 'B', 'e':
		s.y = clamp(s.y+arg(0, 1), 0, s.rows-1)
	case 'C', 'a':
		s.x = clamp(s.x+arg(0, 1), 0, s.cols-1)
	case 'D':
		s.x = clamp(s.x-arg(0, 1), 0, s.cols-1)
	case 'E':
		s.x, s.y = 0, clamp(s.y+arg(0, 1), 0, s.rows-1)
	case 'F':
		s.x, s.y = 0, clamp(s.y-arg(0, 1), 0, s.rows-1)
	case 'G', '`':
		s.x = clamp(arg(0, 1)-1, 0, s.cols-1)
	case 'd':
		s.y = clamp(arg(0, 1)-1, 0, s.rows-1)
	case 'H', 'f':
		s.y, s.x = clamp(arg(0, 1)-1, 0, s.rows-1), clamp(arg(1, 1)-1, 0, s.cols-1)
	case 'J':
		switch arg(0, 0) {
		case 0:
			s.clearRow(s.y, s.x, s.cols)
			for y := s.y + 1; y < s.rows; y++ {
				s.clearRow(y, 0, s.cols)
			}
		case 1:
			for y := 0; y < s.y; y++ {
				s.clearRow(y, 0, s.cols)
			}
			s.clearRow(s.y, 0, s.x+1)
		default:
			for y := 0; y < s.rows; y++ {
				s.clearRow(y, 0, s.cols)
			}
		}
	case 'K':
		switch arg(0, 0) {
		case 0:
			s.clearRow(s.y, s.x, s.cols)
		case 1:
			s.clearRow(s.y, 0, s.x+1)
		default:
			s.clearRow(s.y, 0, s.cols)
		}
	case 'X':
		s.clearRow(s.y, s.x, s.x+arg(0, 1))
	case 'P': // delete characters
		n := clamp(arg(0, 1), 0, s.cols-s.x)
		row := s.grid[s.y]
		copy(row[s.x:], row[s.x+n:])
		s.clearRow(s.y, s.cols-n, s.cols)
	case '@': // insert blanks
		n := clamp(arg(0, 1), 0, s.cols-s.x)
		row := s.grid[s.y]
		copy(row[s.x+n:], row[s.x:s.cols-n])
		s.clearRow(s.y, s.x, s.x+n)
	case 'L', 'M': // insert or delete lines, within the scroll region
		if s.y >= s.top && s.y <= s.bot {
			top := s.top
			s.top = s.y
			if final == 'L' {
				s.scrollDown(clamp(arg(0, 1), 0, s.bot-s.y+1))
			} else {
				s.scrollUp(clamp(arg(0, 1), 0, s.bot-s.y+1))
			}
			s.top = top
		}
	case 'S':
		s.scrollUp(clamp(arg(0, 1), 0, s.rows))
	case 'T':
		s.scrollDown(clamp(arg(0, 1), 0, s.rows))
	case 'r':
		top, bot := arg(0, 1)-1, arg(1, s.rows)-1
		if top < bot && bot < s.rows {
			s.top, s.bot = top, bot
		} else {
			s.top, s.bot = 0, s.rows-1
		}
		s.x, s.y = 0, 0
	case 's':
		s.savedX, s.savedY = s.x, s.y
	case 'u':
		s.restoreCursor()
	}
	s.wrapNext = false
	return j + 1, true
}

func (s *Screen) altScreen(on, saveCursor bool) {
	switch {
	case on && s.main == nil:
		if saveCursor {
			s.savedX, s.savedY = s.x, s.y
		}
		s.main = s.grid
		s.grid = make([][]rune, s.rows)
		for i := range s.grid {
			s.grid[i] = blankRow(s.cols)
		}
	case !on && s.main != nil:
		s.grid, s.main = s.main, nil
		if saveCursor {
			s.restoreCursor()
		}
	}
	s.top, s.bot = 0, s.rows-1
}
