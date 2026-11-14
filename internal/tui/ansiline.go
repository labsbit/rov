package tui

import (
	"strings"
)

// This file renders one log line of the output pane. Lines come from the
// .ansi log, where every escape sequence except SGR (colour, bold, …) was
// removed and every line is self-contained. The functions here are pure, so
// the pane can cache their results per width.

const (
	sgrReset = "\x1b[0m"
	tabWidth = 8
)

// sgrState is the SGR in effect at a point of a line, as the sequences that
// set it since the last full reset. Replaying them restores the state, which
// is how a wrapped row and the text after a search match get their colours
// back.
type sgrState struct{ seqs []string }

// apply records an SGR sequence ("\x1b[…m").
func (s *sgrState) apply(seq string) {
	params := seq[2 : len(seq)-1]
	toks := strings.Split(params, ";")
	var keep []string
	for i := 0; i < len(toks); i++ {
		switch t := toks[i]; t {
		case "", "0":
			s.seqs = s.seqs[:0]
			keep = keep[:0]
		case "38", "48", "58":
			// Extended colour: its arguments are not resets.
			n := 0
			if i+1 < len(toks) {
				switch toks[i+1] {
				case "5":
					n = 2
				case "2":
					n = 4
				}
			}
			end := min(i+1+n, len(toks))
			keep = append(keep, toks[i:end]...)
			i = end - 1
		default:
			keep = append(keep, t)
		}
	}
	if len(keep) > 0 {
		s.seqs = append(s.seqs, "\x1b["+strings.Join(keep, ";")+"m")
	}
}

func (s *sgrState) prefix() string { return strings.Join(s.seqs, "") }
func (s *sgrState) active() bool   { return len(s.seqs) > 0 }

// isSGR reports whether an escape sequence is a CSI ending in 'm'.
func isSGR(seq string) bool {
	return len(seq) >= 3 && seq[0] == 0x1b && seq[1] == '[' && seq[len(seq)-1] == 'm' && !strings.ContainsAny(seq[2:len(seq)-1], "<=>?")
}

// layoutLine cuts an ANSI line into display rows of at most width cells,
// measured with c (the renderer's width method). Trailing blanks are
// dropped first: a TUI's output padded to its 220-column pane would wrap
// into empty rows. With wrap every other cell is kept (hard wrap at the
// cell boundary, wide characters move to the next row whole; one wider
// than the row becomes "…"); without it the first row is returned, ending
// in "…" when the line was longer. Tabs expand to the next multiple of 8,
// other control characters and non-SGR escapes are dropped. Each row
// starts with the SGR in effect and ends with a reset, so rows can be drawn
// independently.
func layoutLine(s string, width int, wrap bool, c cells) []string {
	if width <= 0 {
		return []string{""}
	}
	s = trimTrailingBlanks(s)
	rows, truncated := cutRows(s, width, wrap, c)
	if truncated {
		rows, _ = cutRows(s, width-1, false, c)
		r := rows[0]
		if strings.HasSuffix(r, sgrReset) {
			r = strings.TrimSuffix(r, sgrReset) + "…" + sgrReset
		} else {
			r += "…"
		}
		rows[0] = r
	}
	return rows
}

func cutRows(s string, width int, wrap bool, c cells) (rows []string, truncated bool) {
	var (
		b     strings.Builder
		st    sgrState
		col   int // cells in the current row
		lcol  int // cells in the logical line (for tabs)
		state byte
	)
	endRow := func() {
		if st.active() {
			b.WriteString(sgrReset)
		}
		rows = append(rows, b.String())
		b.Reset()
		b.WriteString(st.prefix())
		col = 0
	}
	// put writes a cluster of w cells; false when the row is full and the
	// line doesn't wrap.
	put := func(seq string, w int) bool {
		if w > width {
			seq, w = "…", 1
		}
		if col+w > width {
			if !wrap {
				return false
			}
			endRow()
		}
		b.WriteString(seq)
		col += w
		lcol += w
		return true
	}
	for len(s) > 0 {
		seq, w, n, ns := c.method().DecodeSequenceInString(s, state, nil)
		state = ns
		if n <= 0 {
			break
		}
		s = s[n:]
		switch {
		case len(seq) > 0 && seq[0] == 0x1b:
			if isSGR(seq) {
				st.apply(seq)
				b.WriteString(seq)
			}
		case seq == "\t":
			for range tabWidth - lcol%tabWidth {
				if !put(" ", 1) {
					return finish(rows, &b, &st), true
				}
			}
		case w == 0:
			// controls (and stray zero-width bytes) are dropped
			if len(seq) == 1 && (seq[0] < 0x20 || seq[0] == 0x7f) {
				continue
			}
			b.WriteString(seq)
		default:
			if !put(seq, w) {
				return finish(rows, &b, &st), true
			}
		}
	}
	return finish(rows, &b, &st), false
}

// trimTrailingBlanks drops the spaces and tabs at the end of an ANSI line,
// keeping the escape sequences among and after them.
func trimTrailingBlanks(s string) string {
	if s == "" {
		return s
	}
	if c := s[len(s)-1]; c != ' ' && c != '\t' && c != 'm' {
		return s
	}
	last := 0 // just past the last visible byte that isn't blank
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i = escEnd(s, i)
			continue
		}
		if s[i] != ' ' && s[i] != '\t' {
			last = i + 1
		}
		i++
	}
	var b strings.Builder
	b.WriteString(s[:last])
	for i := last; i < len(s); {
		if s[i] == 0x1b {
			j := escEnd(s, i)
			b.WriteString(s[i:j])
			i = j
			continue
		}
		i++
	}
	return b.String()
}

func finish(rows []string, b *strings.Builder, st *sgrState) []string {
	if st.active() {
		b.WriteString(sgrReset)
	}
	return append(rows, b.String())
}

// span is a byte range [start, end) of a line's plain text.
type span struct{ start, end int }

// findAll returns the non-overlapping occurrences of query in plain. With
// fold, ASCII letters match case-insensitively (byte offsets stay valid).
func findAll(plain, query string, fold bool) []span {
	if query == "" {
		return nil
	}
	hay := plain
	if fold {
		hay = asciiLower(plain)
	}
	var out []span
	for off := 0; off <= len(hay)-len(query); {
		i := strings.Index(hay[off:], query)
		if i < 0 {
			break
		}
		start := off + i
		out = append(out, span{start, start + len(query)})
		off = start + len(query)
	}
	return out
}

// smartCase reports whether a query should match case-insensitively: when
// it has no upper-case letter.
func smartCase(query string) bool { return strings.ToLower(query) == query }

func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if c := s[i]; c >= 'A' && c <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if c := b[j]; c >= 'A' && c <= 'Z' {
					b[j] = c + 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}

// highlight marks spans of a line's plain text in the ANSI line: style on
// at each start (cur is the index of the span drawn with curStyle), the
// line's own SGR restored at each end. Colour changes inside a match are
// kept but the match style is applied again after them, so a match reads
// the same over coloured text.
func highlight(s string, spans []span, cur int, style, curStyle string) string {
	if len(spans) == 0 {
		return s
	}
	var (
		b     strings.Builder
		st    sgrState
		plain int // offset in the plain text
		k     int // next span
		in    = -1
	)
	b.Grow(len(s) + len(spans)*16)
	styleOf := func(i int) string {
		if i == cur {
			return curStyle
		}
		return style
	}
	boundary := func() {
		if in >= 0 && plain == spans[in].end {
			b.WriteString(sgrReset)
			b.WriteString(st.prefix())
			in = -1
		}
		if in < 0 && k < len(spans) && plain == spans[k].start {
			in = k
			k++
			b.WriteString(styleOf(in))
		}
	}
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			if in >= 0 && plain == spans[in].end {
				boundary()
			}
			j := escEnd(s, i)
			seq := s[i:j]
			b.WriteString(seq)
			if isSGR(seq) {
				st.apply(seq)
				if in >= 0 {
					b.WriteString(styleOf(in))
				}
			}
			i = j
			continue
		}
		boundary()
		b.WriteByte(s[i])
		i++
		plain++
	}
	boundary()
	if in >= 0 {
		b.WriteString(sgrReset)
		b.WriteString(st.prefix())
	}
	return b.String()
}

// escEnd returns the index just past the escape sequence starting at s[i]:
// a CSI ("ESC [ … final") or a two-byte escape.
func escEnd(s string, i int) int {
	if i+1 >= len(s) {
		return len(s)
	}
	if s[i+1] != '[' {
		return i + 2
	}
	j := i + 2
	for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
		j++
	}
	if j < len(s) {
		j++
	}
	return j
}

// stripSGR returns the plain text of an .ansi line: the bytes outside escape
// sequences. Its byte offsets are what highlight expects.
func stripSGR(s string) string {
	if strings.IndexByte(s, 0x1b) < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			i = escEnd(s, i)
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// sanitize turns a raw terminal line (docker logs) into an .ansi-style line:
// SGR kept, every other escape sequence and control character removed, and
// only the text after the last carriage return kept.
func sanitize(s string) string {
	s = strings.TrimSuffix(s, "\r")
	if i := strings.LastIndexByte(s, '\r'); i >= 0 {
		s = s[i+1:]
	}
	if !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 && r != '\t' || r == 0x7f }) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == 0x1b:
			j := escEnd(s, i)
			if i+1 < len(s) && s[i+1] == ']' {
				j = oscEnd(s, i)
			}
			if seq := s[i:j]; isSGR(seq) {
				b.WriteString(seq)
			}
			i = j
		case c < 0x20 && c != '\t' || c == 0x7f:
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// oscEnd returns the index past an OSC sequence (ended by BEL or ST).
func oscEnd(s string, i int) int {
	for j := i + 2; j < len(s); j++ {
		if s[j] == 0x07 {
			return j + 1
		}
		if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
			return j + 2
		}
	}
	return len(s)
}
