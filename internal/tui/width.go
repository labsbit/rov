package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// cells measures, cuts and pads text the way the renderer will draw it.
//
// Bubble Tea's renderer counts cells with wcwidth (every codepoint) unless
// the terminal reports Unicode core mode (2027), when it switches to
// grapheme widths. The two disagree on emoji sequences — 👨‍👩‍👧 is 6
// cells or 2, ❤️ 1 or 2 — so every width the TUI computes must use the
// method the renderer uses, or rows come out too long or too short and the
// right border moves. The model keeps the active method (it starts as
// WcWidth, like the renderer, and follows the same mode report) and hands
// it to everything that lays out text.
type cells ansi.Method

func (c cells) method() ansi.Method { return ansi.Method(c) }

// width is the number of cells s takes (escape sequences are free).
func (c cells) width(s string) int { return c.method().StringWidth(s) }

// trunc cuts s to w cells, ending in tail when it was longer.
func (c cells) trunc(s string, w int, tail string) string {
	return c.method().Truncate(s, max(0, w), tail)
}

// fit truncates s (plain or styled) to w cells with "…".
func (c cells) fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if c.width(s) <= w {
		return s
	}
	return c.trunc(s, w, "…")
}

// pad fills s with spaces to w cells.
func (c cells) pad(s string, w int) string {
	if n := c.width(s); n < w {
		return s + strings.Repeat(" ", w-n)
	}
	return s
}

// exact makes s exactly w cells: truncated or padded.
func (c cells) exact(s string, w int) string {
	if n := c.width(s); n > w {
		s = c.trunc(s, w, "")
	}
	return c.pad(s, w)
}

// wrap wraps plain text at w cells, breaking words only when they don't
// fit on a line.
func (c cells) wrap(s string, w int) []string {
	if w <= 0 {
		return []string{s}
	}
	return strings.Split(c.method().Wrap(s, w, ""), "\n")
}

// splice draws over (w cells) onto base at cell x: the cells of base before
// x, then over, then what base has after x+w in its own colours. A wide
// character cut by either edge becomes spaces, so the row keeps its width.
func (c cells) splice(base, over string, x, w int) string {
	left, lw := c.upTo(base, x)
	var b strings.Builder
	b.WriteString(left)
	b.WriteString(sgrReset)
	b.WriteString(strings.Repeat(" ", max(0, x-lw)))
	b.WriteString(c.exact(over, w))
	b.WriteString(sgrReset)
	b.WriteString(c.from(base, x+w))
	return b.String()
}

// upTo returns the start of s that fits in k cells (escape sequences
// before the cut included, none after it) and its width.
func (c cells) upTo(s string, k int) (string, int) {
	col := 0
	var state byte
	for i := 0; i < len(s); {
		seq, w, n, ns := c.method().DecodeSequenceInString(s[i:], state, nil)
		state = ns
		if n <= 0 {
			return s[:i], col
		}
		if len(seq) == 0 || seq[0] != 0x1b {
			if col+w > k {
				return s[:i], col
			}
			col += w
		}
		i += n
	}
	return s, col
}

// from returns s from cell k on: the SGR in effect there, then the rest. A
// wide character starting before k is replaced by spaces for its cells
// from k on.
func (c cells) from(s string, k int) string {
	var (
		st    sgrState
		col   int
		state byte
	)
	for i := 0; i < len(s); {
		seq, w, n, ns := c.method().DecodeSequenceInString(s[i:], state, nil)
		state = ns
		if n <= 0 {
			break
		}
		switch {
		case len(seq) > 0 && seq[0] == 0x1b:
			if isSGR(seq) {
				st.apply(seq)
			}
		case col >= k && w > 0:
			return st.prefix() + s[i:]
		case col+w > k:
			// straddles k
			i += n
			col += w
			return st.prefix() + strings.Repeat(" ", col-k) + s[i:]
		}
		col += w
		i += n
	}
	return ""
}
