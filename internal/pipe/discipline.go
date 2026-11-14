package pipe

import (
	"unicode/utf8"
)

const (
	// MaxLineBytes caps the plain text of one line. Longer output is split
	// into several lines.
	MaxLineBytes = 64 << 10
	// maxANSILineBytes caps the .ansi form of one line, which can be much
	// longer than the plain text when the colour changes on every character.
	maxANSILineBytes = 4 * MaxLineBytes
	// maxSeqParams caps the parameter bytes of one CSI sequence; a longer
	// sequence is dropped whole.
	maxSeqParams = 256
)

// ClearMarker is the line written where the screen was cleared.
const ClearMarker = "── screen cleared ──"

// clearMarkerANSI is ClearMarker as it appears in .ansi.
const clearMarkerANSI = "\x1b[2m" + ClearMarker + sgrReset

// parser states.
type pstate uint8

const (
	stGround    pstate = iota
	stEsc              // after ESC
	stEscInter         // ESC + intermediate bytes, e.g. charset selection "ESC ( B"
	stCSI              // ESC [
	stString           // OSC, DCS, SOS, PM, APC payload: skipped
	stStringEsc        // ESC inside a string: ST ("ESC \") or a new sequence
)

const (
	bel = 0x07
	can = 0x18
	sub = 0x1a
	esc = 0x1b
)

// discipline is the terminal-output line discipline shared by both files.
// Feed it bytes with write; it calls emit once per finished line with the
// plain and the .ansi form (no trailing newline). The slices are only valid
// during the call.
//
// It is a byte-at-a-time state machine, so its output does not depend on how
// the input is split into writes. It is not safe for concurrent use.
type discipline struct {
	emit func(plain, ansi []byte)

	st pstate
	// pending UTF-8 sequence
	ubuf [utf8.UTFMax]byte
	un   int
	// current CSI sequence
	params   []byte
	inter    bool // intermediate bytes seen
	overflow bool // params too long or malformed: drop the sequence

	// current line
	plain, ansi []byte
	cur         sgrState // rendition the terminal is in
	emitted     sgrState // rendition in effect at the end of ansi

	crPending  bool  // bare-or-not "\r" seen, decided by the next byte
	afterFlush bool  // the line was just flushed by timeout and nothing followed
	homed      bool  // last sequence moved the cursor home (for "ESC[H ESC[J")
	lastMarker bool  // the last line written was a clear marker
	lines      int64 // lines emitted so far
}

func newDiscipline(emit func(plain, ansi []byte)) *discipline {
	return &discipline{emit: emit, params: make([]byte, 0, 32)}
}

// write feeds terminal output.
func (d *discipline) write(p []byte) {
	for _, b := range p {
		d.byte(b)
	}
}

// pending reports whether an unterminated line holds text.
func (d *discipline) pending() bool { return len(d.plain) > 0 }

// flush ends a pending partial line, as after a timeout without output.
func (d *discipline) flush() {
	if len(d.plain) > 0 {
		d.endLine()
		d.afterFlush = true
	}
}

// close ends the input: an incomplete UTF-8 sequence becomes U+FFFD and any
// partial line is flushed.
func (d *discipline) close() {
	if d.un > 0 {
		d.un = 0
		d.text(utf8.RuneError)
	}
	d.st = stGround
	d.flush()
}

func (d *discipline) byte(b byte) {
	switch d.st {
	case stGround:
		d.ground(b)
	case stEsc:
		d.escape(b)
	case stEscInter:
		d.escInter(b)
	case stCSI:
		d.csi(b)
	case stString:
		switch b {
		case esc:
			d.st = stStringEsc
		case bel, can, sub:
			d.st = stGround
		}
	case stStringEsc:
		if b == '\\' {
			d.st = stGround
			return
		}
		d.st = stEsc
		d.escape(b)
	}
}

func (d *discipline) ground(b byte) {
	if d.un > 0 {
		if b&0xc0 == 0x80 {
			d.ubuf[d.un] = b
			d.un++
			if !utf8.FullRune(d.ubuf[:d.un]) {
				return
			}
			r, size := utf8.DecodeRune(d.ubuf[:d.un])
			n := d.un
			d.un = 0
			d.text(r) // RuneError for invalid sequences
			// An invalid prefix consumed one byte; the rest are
			// continuation bytes, each U+FFFD on its own.
			for range n - size {
				d.text(utf8.RuneError)
			}
			return
		}
		d.un = 0
		d.text(utf8.RuneError) // truncated sequence
	}
	switch {
	case b < 0x20 || b == 0x7f:
		d.control(b)
	case b < 0x80:
		d.text(rune(b))
	case b >= 0xc2 && b <= 0xf4:
		d.ubuf[0] = b
		d.un = 1
	default: // stray continuation byte or a byte that never starts UTF-8
		d.text(utf8.RuneError)
	}
}

// control executes a C0 control (also inside sequences, as terminals do).
func (d *discipline) control(b byte) {
	switch b {
	case '\n':
		d.newline()
	case '\r':
		d.crPending = true
	case '\t':
		d.text('\t')
	case esc:
		d.st = stEsc
	}
}

func (d *discipline) escape(b byte) {
	switch {
	case b == '[':
		d.st = stCSI
		d.params = d.params[:0]
		d.inter, d.overflow = false, false
	case b == ']' || b == 'P' || b == 'X' || b == '^' || b == '_':
		d.st = stString
	case b == 'c': // RIS, full reset
		d.st = stGround
		d.cur = sgrState{}
		d.clear()
	case b == esc:
		// stay: a new escape replaces the unfinished one
	case b == can || b == sub:
		d.st = stGround
	case b < 0x20:
		d.control(b)
	case b <= 0x2f:
		d.st = stEscInter
	case b <= 0x7e:
		d.st = stGround // two-byte escape (DECSC, RI, keypad modes, …): dropped
		d.homed = false
	case b == 0x7f:
	default:
		d.st = stGround
		d.ground(b)
	}
}

func (d *discipline) escInter(b byte) {
	switch {
	case b == esc:
		d.st = stEsc
	case b == can || b == sub:
		d.st = stGround
	case b < 0x20:
		d.control(b)
	case b <= 0x2f:
	case b <= 0x7e:
		d.st = stGround
	case b == 0x7f:
	default:
		d.st = stGround
		d.ground(b)
	}
}

func (d *discipline) csi(b byte) {
	switch {
	case b >= 0x30 && b <= 0x3f:
		if d.inter || len(d.params) >= maxSeqParams {
			d.overflow = true
			return
		}
		d.params = append(d.params, b)
	case b >= 0x20 && b <= 0x2f:
		d.inter = true
	case b >= 0x40 && b <= 0x7e:
		d.st = stGround
		if !d.overflow {
			d.csiFinal(b)
		}
	case b == esc:
		d.st = stEsc
	case b == can || b == sub:
		d.st = stGround
	case b < 0x20:
		d.control(b)
	case b == 0x7f:
	default:
		d.st = stGround
		d.ground(b)
	}
}

func (d *discipline) csiFinal(final byte) {
	private := len(d.params) > 0 && d.params[0] >= '<' && d.params[0] <= '?'
	if d.inter || private {
		d.homed = false
		return
	}
	switch final {
	case 'm':
		d.cur.apply(d.params)
	case 'J':
		switch p := string(d.params); {
		case p == "2" || p == "3":
			d.clear()
		case (p == "" || p == "0") && d.homed:
			d.clear()
		}
		d.homed = false
	case 'H', 'f':
		switch string(d.params) {
		case "", "1", "1;1", ";", "1;", ";1":
			d.homed = true
		default:
			d.homed = false
		}
	default:
		d.homed = false
	}
}

// text appends one character to the current line.
func (d *discipline) text(r rune) {
	d.homed = false
	d.afterFlush = false
	if d.crPending {
		d.rewind()
	}
	n := utf8.RuneLen(r)
	if len(d.plain)+n > MaxLineBytes || len(d.ansi)+n+maxSGRBytes > maxANSILineBytes {
		d.endLine()
	}
	if d.cur != d.emitted {
		d.ansi = appendTransition(d.ansi, d.emitted, d.cur)
		d.emitted = d.cur
	}
	d.plain = utf8.AppendRune(d.plain, r)
	d.ansi = utf8.AppendRune(d.ansi, r)
}

// rewind is a bare carriage return taking effect: the line restarts.
func (d *discipline) rewind() {
	d.plain = d.plain[:0]
	d.ansi = d.ansi[:0]
	d.emitted = sgrState{}
	d.crPending = false
}

func (d *discipline) newline() {
	d.homed = false
	d.crPending = false
	if len(d.plain) == 0 && d.afterFlush {
		d.afterFlush = false // the flushed line already ended
		return
	}
	d.afterFlush = false
	d.endLine()
}

func (d *discipline) clear() {
	d.homed = false
	d.afterFlush = false
	if len(d.plain) > 0 {
		d.endLine()
	}
	d.rewind()
	if d.lines == 0 || d.lastMarker {
		return
	}
	d.emit([]byte(ClearMarker), []byte(clearMarkerANSI))
	d.lines++
	d.lastMarker = true
}

func (d *discipline) endLine() {
	if !d.emitted.isZero() {
		d.ansi = append(d.ansi, sgrReset...)
	}
	d.emit(d.plain, d.ansi)
	d.plain = d.plain[:0]
	d.ansi = d.ansi[:0]
	d.emitted = sgrState{}
	d.lines++
	d.lastMarker = false
}
