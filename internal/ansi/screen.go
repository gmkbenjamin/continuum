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
	cols, rows     int
	grid           [][]rune
	dim            [][]bool
	x, y           int
	wrapNext       bool // the cursor is past the last column; the next character wraps
	top, bot       int  // scroll region, inclusive
	savedX         int
	savedY         int
	main           [][]rune // the main screen while the alternate one is shown
	mainDim        [][]bool
	faint          bool   // SGR 2 (dim/faint) active
	grayFG         bool   // muted gray foreground (SGR 90, 256-color gray, muted RGB gray)
	bracketedPaste bool   // DECSET ?2004h active
	pending        []byte // an incomplete escape sequence or UTF-8 character
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

func blankDim(cols int) []bool {
	return make([]bool, cols)
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
	fitDim := func(g [][]bool) [][]bool {
		if g == nil {
			return nil
		}
		if len(g) > rows {
			g = g[len(g)-rows:]
		}
		out := make([][]bool, rows)
		for i := range out {
			out[i] = blankDim(cols)
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
	s.dim = fitDim(s.dim)
	if s.dim == nil {
		s.dim = fitDim(make([][]bool, 0))
	}
	s.main = fit(s.main)
	s.mainDim = fitDim(s.mainDim)
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
	isDim := s.faint || s.grayFG
	s.grid[s.y][s.x] = r
	s.dim[s.y][s.x] = isDim
	if w == 2 {
		s.grid[s.y][s.x+1] = wideTail
		s.dim[s.y][s.x+1] = isDim
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
		copy(s.dim[s.top:s.bot], s.dim[s.top+1:s.bot+1])
		s.dim[s.bot] = blankDim(s.cols)
	}
}

func (s *Screen) scrollDown(n int) {
	for ; n > 0; n-- {
		copy(s.grid[s.top+1:s.bot+1], s.grid[s.top:s.bot])
		s.grid[s.top] = blankRow(s.cols)
		copy(s.dim[s.top+1:s.bot+1], s.dim[s.top:s.bot])
		s.dim[s.top] = blankDim(s.cols)
	}
}

func (s *Screen) clearRow(y, from, to int) {
	from, to = clamp(from, 0, s.cols), clamp(to, 0, s.cols)
	for x := from; x < to; x++ {
		s.grid[y][x] = ' '
		s.dim[y][x] = false
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
	if len(raw) > 0 && strings.ContainsRune("<=>", rune(raw[0])) {
		return j + 1, true
	}
	raw = strings.TrimPrefix(raw, "?")
	if final == 'm' && !private {
		s.applySGR(raw)
		s.wrapNext = false
		return j + 1, true
	}
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
				} else if m == 2004 {
					s.bracketedPaste = final == 'h'
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
		drow := s.dim[s.y]
		copy(drow[s.x:], drow[s.x+n:])
		s.clearRow(s.y, s.cols-n, s.cols)
	case '@': // insert blanks
		n := clamp(arg(0, 1), 0, s.cols-s.x)
		row := s.grid[s.y]
		copy(row[s.x+n:], row[s.x:s.cols-n])
		drow := s.dim[s.y]
		copy(drow[s.x+n:], drow[s.x:s.cols-n])
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
		s.mainDim = s.dim
		s.grid = make([][]rune, s.rows)
		s.dim = make([][]bool, s.rows)
		for i := range s.grid {
			s.grid[i] = blankRow(s.cols)
			s.dim[i] = blankDim(s.cols)
		}
	case !on && s.main != nil:
		s.grid, s.main = s.main, nil
		s.dim, s.mainDim = s.mainDim, nil
		if saveCursor {
			s.restoreCursor()
		}
	}
	s.top, s.bot = 0, s.rows-1
}

// BracketedPaste reports whether the program enabled DECSET ?2004h.
func (s *Screen) BracketedPaste() bool { return s.bracketedPaste }

func (s *Screen) applySGR(raw string) {
	fields := strings.Split(raw, ";")
	params := make([]int, len(fields))
	for i, field := range fields {
		p, _, _ := strings.Cut(field, ":")
		params[i], _ = strconv.Atoi(p)
	}
	for i := 0; i < len(params); i++ {
		p := params[i]
		switch {
		case p == 0:
			s.faint, s.grayFG = false, false
		case p == 2:
			s.faint = true
		case p == 22:
			s.faint = false
		case p == 90:
			s.grayFG = true
		case p == 39 || (p >= 30 && p <= 37) || (p >= 91 && p <= 97):
			s.grayFG = false
		case p == 38 || p == 48 || p == 58:
			values := params[i+1:]
			if _, subparams, ok := strings.Cut(fields[i], ":"); ok {
				parts := strings.Split(subparams, ":")
				values = make([]int, len(parts))
				for j, part := range parts {
					values[j], _ = strconv.Atoi(part)
				}
			}
			colon := strings.ContainsRune(fields[i], ':')
			n, gray := colorParameters(values, colon)
			if !colon {
				i += n
			}
			if p == 38 && n > 0 {
				s.grayFG = gray
			}
		}
	}
}

func colorParameters(values []int, colon bool) (int, bool) {
	switch {
	case len(values) >= 2 && values[0] == 5:
		n := values[1]
		return 2, n == 8 || (n >= 238 && n <= 248)
	case len(values) >= 4 && values[0] == 2:
		rgb := values[1:4]
		if colon && len(values) == 5 {
			// Colon syntax may specify a color space before RGB.
			rgb = values[2:5]
		}
		return 4, isMutedGray(rgb[0], rgb[1], rgb[2])
	}
	return 0, false
}

func isMutedGray(r, g, b int) bool {
	lo, hi := min(r, min(g, b)), max(r, max(g, b))
	return hi-lo <= 40 && lo >= 75 && hi <= 170
}

// PromptDraft extracts unsubmitted, non-dim text from the active prompt box
// near the bottom of the screen (e.g. `> ...`, `❯ ...`, `› ...`). Dim or gray
// placeholder hints are ignored so an empty prompt with hints returns "".
func (s *Screen) PromptDraft() (text string, found bool) {
	last := -1
	for y := len(s.grid) - 1; y >= 0; y-- {
		if rowHasText(s.grid[y]) {
			last = y
			break
		}
	}
	if last < 0 {
		return "", false
	}
	minRow := max(0, last-14)
	promptRow, textCol := -1, -1
	for y := last; y >= minRow; y-- {
		if col, ok := promptTextStart(s.grid[y]); ok {
			promptRow, textCol = y, col
			break
		}
	}
	if promptRow < 0 {
		return "", false
	}
	first, firstWrapped := rowNonDimText(s.grid[promptRow], s.dim[promptRow], textCol)
	if first == "" {
		return "", true
	}
	var out strings.Builder
	out.WriteString(first)
	prevWrapped := firstWrapped
	for y := promptRow + 1; y <= last; y++ {
		if !rowHasText(s.grid[y]) || isRuleRow(s.grid[y]) {
			break
		}
		if _, ok := promptTextStart(s.grid[y]); ok {
			break
		}
		if !rowIndentedTo(s.grid[y], textCol) {
			break
		}
		cont, wrapped := rowNonDimText(s.grid[y], s.dim[y], textCol)
		if cont == "" {
			break
		}
		if prevWrapped {
			out.WriteString(cont)
		} else {
			out.WriteByte('\n')
			out.WriteString(cont)
		}
		prevWrapped = wrapped
	}
	return out.String(), true
}

func rowHasText(row []rune) bool {
	for _, r := range row {
		if r != ' ' && r != wideTail {
			return true
		}
	}
	return false
}

func isRuleRow(row []rune) bool {
	run := 0
	for _, r := range row {
		switch r {
		case '─', '━', '═', '╌', '┄', '-', '_':
			run++
			if run >= 4 {
				return true
			}
		case ' ', wideTail, '╭', '╮', '╰', '╯', '┌', '┐', '└', '┘', '├', '┤', '│', '┃':
		default:
			return false
		}
	}
	return false
}

func isBoxBorder(r rune) bool {
	return r == '│' || r == '┃' || r == '▏' || r == '▎'
}

// promptTextStart reports whether row starts with an interactive prompt marker
// (`>`, `❯`, `›`, optionally after up to 2 spaces or a left box border) and
// returns the column where user text begins.
func promptTextStart(row []rune) (int, bool) {
	i := 0
	for i < len(row) && i < 2 && row[i] == ' ' {
		i++
	}
	if i < len(row) && isBoxBorder(row[i]) {
		border := row[i]
		i++
		for i < len(row) && row[i] == ' ' && i < 5 {
			i++
		}
		if border == '┃' || border == '▎' {
			if i < len(row) && row[i] != '>' && row[i] != '❯' && row[i] != '›' {
				return min(i, 2), true
			}
		}
	}
	if i >= len(row) {
		return -1, false
	}
	switch row[i] {
	case '>', '❯', '›':
	default:
		return -1, false
	}
	// Must be followed by space or end of row (not `>>`, `>=`, `->`, etc.).
	if i+1 < len(row) && row[i+1] != ' ' && row[i+1] != '\u00a0' {
		return -1, false
	}
	col := i + 1
	if col < len(row) && (row[col] == ' ' || row[col] == '\u00a0') {
		col++
	}
	return col, true
}

func rowIndentedTo(row []rune, col int) bool {
	if col <= 0 || col > len(row) {
		return false
	}
	for i := 0; i < col; i++ {
		if row[i] == ' ' || row[i] == wideTail || isBoxBorder(row[i]) {
			continue
		}
		return false
	}
	return true
}

// rowNonDimText returns the non-dim text in row starting at col, and whether
// the text reached the right edge without trailing whitespace (mid-word wrap).
func rowNonDimText(row []rune, drow []bool, col int) (string, bool) {
	if col < 0 || col >= len(row) {
		return "", false
	}
	end := len(row)
	// Strip an optional right-side box border (e.g. `│`) and trailing spaces.
	for end > col && (row[end-1] == ' ' || row[end-1] == wideTail) {
		end--
	}
	rightEdge := end == len(row)
	if end > col && isBoxBorder(row[end-1]) {
		end--
		for end > col && (row[end-1] == ' ' || row[end-1] == wideTail) {
			end--
		}
		rightEdge = false
	}
	var b strings.Builder
	dimText := false
	for i := col; i < end; i++ {
		if row[i] == wideTail {
			continue
		}
		if drow[i] {
			dimText = dimText || row[i] != ' '
			continue
		}
		b.WriteRune(row[i])
	}
	text := strings.TrimRight(b.String(), " ")
	if dimText && utf8.RuneCountInString(strings.TrimSpace(text)) == 1 {
		// Some editors draw a block cursor over the first placeholder letter.
		return "", false
	}
	return text, rightEdge && text != ""
}
