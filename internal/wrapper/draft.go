package wrapper

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"
)

// draftBuffer tracks unsubmitted text the user typed or pasted into the
// wrapped agent's input box since the last Enter submit.
//
// For normal typing, cursor movement, deletions, and bracketed pastes it holds
// the exact draft string (independent of any placeholder text the agent draws).
// When keys that let the agent mutate the input box itself are pressed (Up/Down
// history recall, Tab completion, bare Escape, Ctrl+C), uncertain is set so
// delivery falls back to reading the non-dim text in the rendered prompt box.
type draftBuffer struct {
	runes     []rune
	cursor    int
	inPaste   bool
	uncertain bool
	pending   []byte
	reply     byte // terminal string response being discarded
	replyESC  bool
	pasteCR   bool
	userInput bool
}

var (
	pasteStart = []byte("\x1b[200~")
	pasteEnd   = []byte("\x1b[201~")
)

func (d *draftBuffer) Reset() {
	d.runes = nil
	d.cursor = 0
	d.inPaste = false
	d.uncertain = false
	d.pending = nil
	d.pasteCR = false
}

func (d *draftBuffer) SetText(s string) {
	d.Reset()
	d.runes = []rune(s)
	d.cursor = len(d.runes)
}

func (d *draftBuffer) Text() string    { return string(d.runes) }
func (d *draftBuffer) Uncertain() bool { return d.uncertain }

// Feed processes raw stdin bytes forwarded to the child PTY and reports
// whether a real (non-pasted, non-escaped) Enter submit occurred.
func (d *draftBuffer) Feed(p []byte) (submitted bool) {
	d.userInput = false
	data := p
	if len(d.pending) > 0 {
		data = append(append([]byte(nil), d.pending...), p...)
		d.pending = nil
	}
	for i := 0; i < len(data); {
		if d.reply != 0 {
			b := data[i]
			if (d.reply == ']' && b == 0x07) || (d.replyESC && b == '\\') {
				d.reply, d.replyESC = 0, false
			} else {
				d.replyESC = b == 0x1b
			}
			i++
			continue
		}
		if d.inPaste {
			d.userInput = true
			if end := bytes.Index(data[i:], pasteEnd); end >= 0 {
				d.insertPasted(data[i : i+end])
				d.inPaste = false
				d.pasteCR = false
				i += end + len(pasteEnd)
				continue
			}
			// Keep a trailing prefix of pasteEnd across read boundaries.
			keep := suffixPrefixLen(data[i:], pasteEnd)
			body := data[i : len(data)-keep]
			if rem := trailingIncompleteUTF8(body); rem > 0 {
				keep += rem
				body = data[i : len(data)-keep]
			}
			d.insertPasted(body)
			if keep > 0 && keep < 64 {
				d.pending = append([]byte(nil), data[len(data)-keep:]...)
			}
			return submitted
		}

		b := data[i]
		switch {
		case b == 0x1b:
			if i+1 == len(data) {
				// An Escape by itself can also be the first byte of a reply.
				d.pending = append(d.pending, b)
				return submitted
			}
			n, complete, enter := d.consumeEscape(data[i:])
			submitted = submitted || enter
			if !complete {
				if len(data[i:]) < 64 {
					d.pending = append([]byte(nil), data[i:]...)
				}
				return submitted
			}
			i += n
		case b == '\r':
			d.userInput = true
			submitted = d.enter() || submitted
			i++
		case b == '\n':
			d.userInput = true
			d.insertRune('\n')
			i++
		case b == 0x03: // Ctrl+C: often clears the input box in TUIs
			d.runes = nil
			d.cursor = 0
			d.uncertain = true
			i++
		case b == 0x7f || b == 0x08: // Backspace
			if d.cursor > 0 {
				d.runes = append(d.runes[:d.cursor-1], d.runes[d.cursor:]...)
				d.cursor--
			}
			i++
		case b == 0x04: // Ctrl+D (delete forward)
			if d.cursor < len(d.runes) {
				d.runes = append(d.runes[:d.cursor], d.runes[d.cursor+1:]...)
			}
			i++
		case b == 0x01: // Ctrl+A (line start)
			d.cursor = d.lineStart()
			i++
		case b == 0x05: // Ctrl+E (line end)
			d.cursor = d.lineEnd()
			i++
		case b == 0x02: // Ctrl+B (left)
			if d.cursor > 0 {
				d.cursor--
			}
			i++
		case b == 0x06: // Ctrl+F (right)
			if d.cursor < len(d.runes) {
				d.cursor++
			}
			i++
		case b == 0x15: // Ctrl+U (kill to line start)
			start := d.lineStart()
			d.runes = append(d.runes[:start], d.runes[d.cursor:]...)
			d.cursor = start
			i++
		case b == 0x0b: // Ctrl+K (kill to line end)
			end := d.lineEnd()
			d.runes = append(d.runes[:d.cursor], d.runes[end:]...)
			i++
		case b == 0x17: // Ctrl+W (kill word backward)
			d.killWordBack()
			i++
		case b == '\t': // Tab completion may mutate text
			d.uncertain = true
			i++
		case b < 0x20:
			d.uncertain = true
			i++
		default:
			if !utf8.FullRune(data[i:]) {
				if len(data[i:]) < 64 {
					d.pending = append([]byte(nil), data[i:]...)
				}
				return submitted
			}
			r, size := utf8.DecodeRune(data[i:])
			if r != utf8.RuneError || size > 1 {
				d.insertRune(r)
			}
			i += size
		}
		if b != 0x1b {
			d.userInput = true
		}
	}
	return submitted
}

func (d *draftBuffer) enter() bool {
	if d.cursor > 0 && d.runes[d.cursor-1] == '\\' {
		// Some editors interpret Backslash+Enter as a newline.
		d.uncertain = true
		return false
	}
	d.Reset()
	return true
}

func (d *draftBuffer) insertRune(r rune) {
	d.runes = append(d.runes, 0)
	copy(d.runes[d.cursor+1:], d.runes[d.cursor:])
	d.runes[d.cursor] = r
	d.cursor++
}

func (d *draftBuffer) insertPasted(b []byte) {
	for i := 0; i < len(b); {
		if d.pasteCR {
			d.pasteCR = false
			if b[i] == '\n' {
				i++
				continue
			}
		}
		if b[i] == '\r' {
			d.pasteCR = true
			i++
			d.insertRune('\n')
			continue
		}
		if b[i] == '\n' {
			d.insertRune('\n')
			i++
			continue
		}
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size <= 1 {
			i++
			continue
		}
		if r >= 0x20 && r != 0x7f || r == '\t' {
			d.insertRune(r)
		}
		i += size
	}
}

func (d *draftBuffer) lineStart() int {
	pos := d.cursor
	for pos > 0 && d.runes[pos-1] != '\n' {
		pos--
	}
	return pos
}

func (d *draftBuffer) lineEnd() int {
	pos := d.cursor
	for pos < len(d.runes) && d.runes[pos] != '\n' {
		pos++
	}
	return pos
}

func (d *draftBuffer) wordLeft() int {
	pos := d.cursor
	for pos > 0 && unicode.IsSpace(d.runes[pos-1]) {
		pos--
	}
	for pos > 0 && !unicode.IsSpace(d.runes[pos-1]) {
		pos--
	}
	return pos
}

func (d *draftBuffer) wordRight() int {
	pos := d.cursor
	for pos < len(d.runes) && unicode.IsSpace(d.runes[pos]) {
		pos++
	}
	for pos < len(d.runes) && !unicode.IsSpace(d.runes[pos]) {
		pos++
	}
	return pos
}

func (d *draftBuffer) killWordBack() {
	start := d.wordLeft()
	d.runes = append(d.runes[:start], d.runes[d.cursor:]...)
	d.cursor = start
}

// consumeEscape parses an escape sequence starting at data[0] == 0x1b.
func (d *draftBuffer) consumeEscape(data []byte) (consumed int, complete, submitted bool) {
	switch data[1] {
	case ']', 'P', '_', '^', 'X': // OSC, DCS, APC, PM, SOS
		d.reply = data[1]
		return 2, true, false
	case '[': // CSI
		if bytes.HasPrefix(data, pasteStart) {
			d.inPaste = true
			d.userInput = true
			return len(pasteStart), true, false
		}
		if bytes.HasPrefix(pasteStart, data) {
			return 0, false, false
		}
		j := 2
		for j < len(data) && (data[j] < 0x40 || data[j] > 0x7e) {
			j++
		}
		if j >= len(data) {
			if j > 64 {
				d.uncertain = true
				return j, true, false
			}
			return 0, false, false
		}
		seq := string(data[:j+1])
		final := data[j]
		switch {
		case seq == "\x1b[I" || seq == "\x1b[O", final == 'R' || final == 'c' || final == 'n',
			final == 'u' && strings.HasPrefix(seq, "\x1b[?"):
			return j + 1, true, false // terminal focus, status, capability or position reply
		case seq == "\x1b[M": // X10 mouse, followed by three coordinate bytes
			if len(data) < j+4 {
				return 0, false, false
			}
			d.uncertain, d.userInput = true, true
			return j + 4, true, false
		}
		d.userInput = true
		// Kitty may attach an event type to the keyboard modifier.
		if final == 'u' {
			fields := strings.Split(seq[2:j], ";")
			if len(fields) > 1 {
				mod, event, _ := strings.Cut(fields[1], ":")
				if event == "3" {
					return j + 1, true, false // release
				}
				seq = "\x1b[" + fields[0] + ";" + mod + "u"
			}
		}
		switch {
		case seq == "\x1b[13u" || seq == "\x1b[13;1u":
			return j + 1, true, d.enter()
		case seq == "\x1b[D":
			if d.cursor > 0 {
				d.cursor--
			}
		case seq == "\x1b[C":
			if d.cursor < len(d.runes) {
				d.cursor++
			}
		case seq == "\x1b[H" || seq == "\x1b[1~" || seq == "\x1b[7~":
			d.cursor = d.lineStart()
		case seq == "\x1b[F" || seq == "\x1b[4~" || seq == "\x1b[8~":
			d.cursor = d.lineEnd()
		case seq == "\x1b[3~":
			if d.cursor < len(d.runes) {
				d.runes = append(d.runes[:d.cursor], d.runes[d.cursor+1:]...)
			}
		case seq == "\x1b[1;5D" || seq == "\x1b[1;3D" || seq == "\x1b[1;9D":
			d.cursor = d.wordLeft()
		case seq == "\x1b[1;5C" || seq == "\x1b[1;3C" || seq == "\x1b[1;9C":
			d.cursor = d.wordRight()
		case seq == "\x1b[13;2u" || seq == "\x1b[27;2;13~" || seq == "\x1b[13;3u" || seq == "\x1b[13;5u":
			// Shift+Enter / Alt+Enter / Ctrl+Enter: newline inside draft.
			d.insertRune('\n')
		case seq == "\x1b[A" || seq == "\x1b[B":
			// Up/Down may recall command history or move across lines.
			d.uncertain = true
		default:
			d.uncertain = true
		}
		return j + 1, true, false

	case 'O': // SS3 (application cursor/Home/End)
		if len(data) < 3 {
			return 0, false, false
		}
		d.userInput = true
		switch data[2] {
		case 'D':
			if d.cursor > 0 {
				d.cursor--
			}
		case 'C':
			if d.cursor < len(d.runes) {
				d.cursor++
			}
		case 'H':
			d.cursor = d.lineStart()
		case 'F':
			d.cursor = d.lineEnd()
		default:
			d.uncertain = true
		}
		return 3, true, false

	case '\r', '\n': // Alt+Enter: newline inside draft
		d.insertRune('\n')
	case 0x7f, 0x08: // Alt+Backspace: kill word backward
		d.killWordBack()
	case 'b', 'B': // Alt+B: word left
		d.cursor = d.wordLeft()
	case 'f', 'F': // Alt+F: word right
		d.cursor = d.wordRight()
	default:
		d.uncertain = true
	}
	d.userInput = true
	return 2, true, false
}

func suffixPrefixLen(b, needle []byte) int {
	maxN := min(len(b), len(needle)-1)
	for n := maxN; n > 0; n-- {
		if bytes.Equal(b[len(b)-n:], needle[:n]) {
			return n
		}
	}
	return 0
}

func trailingIncompleteUTF8(b []byte) int {
	for n := 1; n <= 3 && n <= len(b); n++ {
		if utf8.RuneStart(b[len(b)-n]) {
			if !utf8.FullRune(b[len(b)-n:]) {
				return n
			}
			return 0
		}
	}
	return 0
}

// formatDraftForWrite prepares saved draft text to be restored into the input
// box without accidentally submitting it. Multiline and tabbed drafts require
// bracketed paste so the child interprets the control characters as text.
func formatDraftForWrite(text string, bracketedPaste bool) []byte {
	if !strings.ContainsAny(text, "\n\t") {
		return []byte(text)
	}
	if !bracketedPaste {
		return nil
	}
	out := make([]byte, 0, len(pasteStart)+len(text)+len(pasteEnd))
	out = append(out, pasteStart...)
	out = append(out, text...)
	return append(out, pasteEnd...)
}
