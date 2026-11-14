package pipe

import (
	"bytes"
	"strconv"
)

// colorKind tells how a colour was specified.
type colorKind uint8

const (
	colorDefault colorKind = iota
	colorBasic             // v 0–15: 30–37 / 90–97 (40–47 / 100–107 for bg)
	color256               // v 0–255: 38;5;v
	colorRGB               // v 0xRRGGBB: 38;2;r;g;b
)

type color struct {
	kind colorKind
	v    uint32
}

type attr uint16

const (
	attrBold attr = 1 << iota
	attrDim
	attrItalic
	attrBlink
	attrRapidBlink
	attrReverse
	attrConceal
	attrStrike
	attrOverline
)

// attrCodes is the render order of the boolean attributes.
var attrCodes = [...]struct {
	a    attr
	code string
}{
	{attrBold, "1"}, {attrDim, "2"}, {attrItalic, "3"}, {attrBlink, "5"},
	{attrRapidBlink, "6"}, {attrReverse, "7"}, {attrConceal, "8"},
	{attrStrike, "9"}, {attrOverline, "53"},
}

// sgrState is the graphic rendition in effect: what a terminal would apply
// to the next character. The zero value is the default rendition.
type sgrState struct {
	attrs     attr
	underline uint8 // 0 none, 1 single, 2 double, 3 curly, 4 dotted, 5 dashed
	fg, bg    color
	ulColor   color
}

// maxSGRBytes bounds the length of one rendered transition, "\x1b[0;…m".
const maxSGRBytes = 128

func (s sgrState) isZero() bool { return s == sgrState{} }

// apply updates s with the parameters of one SGR sequence (the bytes between
// "ESC[" and "m"). Unknown or malformed parameters are ignored.
func (s *sgrState) apply(params []byte) {
	toks := bytes.Split(params, []byte{';'})
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if bytes.IndexByte(t, ':') >= 0 {
			s.applySub(bytes.Split(t, []byte{':'}))
			continue
		}
		code, ok := sgrNum(t)
		if !ok {
			continue
		}
		switch code {
		case 38, 48, 58:
			c, used, ok := extColor(toks[i+1:])
			i += used
			if ok {
				s.setColor(code, c)
			}
		default:
			s.applyCode(code)
		}
	}
}

// applySub handles one colon-separated parameter: "4:3", "38:5:n",
// "38:2:r:g:b" and "38:2:cs:r:g:b" (ITU T.416, with a colour-space id).
func (s *sgrState) applySub(sub [][]byte) {
	code, ok := sgrNum(sub[0])
	if !ok {
		return
	}
	switch code {
	case 4:
		n, ok := 1, true
		if len(sub) > 1 {
			n, ok = sgrNum(sub[1])
		}
		if ok && n <= 5 {
			s.underline = uint8(n)
		}
	case 38, 48, 58:
		if len(sub) < 2 {
			return
		}
		rest := sub[1:]
		if mode, _ := sgrNum(rest[0]); mode == 2 && len(rest) >= 5 {
			rest = append([][]byte{rest[0]}, rest[len(rest)-3:]...) // drop colour space id
		}
		if c, _, ok := extColor(rest); ok {
			s.setColor(code, c)
		}
	default:
		s.applyCode(code)
	}
}

// extColor parses the tokens after 38/48/58: "5;n" or "2;r;g;b". used is the
// number of tokens consumed.
func extColor(rest [][]byte) (c color, used int, ok bool) {
	if len(rest) == 0 {
		return color{}, 0, false
	}
	mode, ok := sgrNum(rest[0])
	if !ok {
		return color{}, 1, false
	}
	switch mode {
	case 5:
		if len(rest) < 2 {
			return color{}, len(rest), false
		}
		n, ok := sgrNum(rest[1])
		if !ok || n > 255 {
			return color{}, 2, false
		}
		return color{color256, uint32(n)}, 2, true
	case 2:
		if len(rest) < 4 {
			return color{}, len(rest), false
		}
		var v uint32
		for _, t := range rest[1:4] {
			n, ok := sgrNum(t)
			if !ok || n > 255 {
				return color{}, 4, false
			}
			v = v<<8 | uint32(n)
		}
		return color{colorRGB, v}, 4, true
	}
	return color{}, 1, false
}

func (s *sgrState) setColor(code int, c color) {
	switch code {
	case 38:
		s.fg = c
	case 48:
		s.bg = c
	case 58:
		s.ulColor = c
	}
}

func (s *sgrState) applyCode(n int) {
	switch {
	case n == 0:
		*s = sgrState{}
	case n == 1:
		s.attrs |= attrBold
	case n == 2:
		s.attrs |= attrDim
	case n == 3:
		s.attrs |= attrItalic
	case n == 4:
		s.underline = 1
	case n == 5:
		s.attrs |= attrBlink
	case n == 6:
		s.attrs |= attrRapidBlink
	case n == 7:
		s.attrs |= attrReverse
	case n == 8:
		s.attrs |= attrConceal
	case n == 9:
		s.attrs |= attrStrike
	case n == 21:
		s.underline = 2
	case n == 22:
		s.attrs &^= attrBold | attrDim
	case n == 23:
		s.attrs &^= attrItalic
	case n == 24:
		s.underline = 0
	case n == 25:
		s.attrs &^= attrBlink | attrRapidBlink
	case n == 27:
		s.attrs &^= attrReverse
	case n == 28:
		s.attrs &^= attrConceal
	case n == 29:
		s.attrs &^= attrStrike
	case n >= 30 && n <= 37:
		s.fg = color{colorBasic, uint32(n - 30)}
	case n == 39:
		s.fg = color{}
	case n >= 40 && n <= 47:
		s.bg = color{colorBasic, uint32(n - 40)}
	case n == 49:
		s.bg = color{}
	case n == 53:
		s.attrs |= attrOverline
	case n == 55:
		s.attrs &^= attrOverline
	case n == 59:
		s.ulColor = color{}
	case n >= 90 && n <= 97:
		s.fg = color{colorBasic, uint32(n - 90 + 8)}
	case n >= 100 && n <= 107:
		s.bg = color{colorBasic, uint32(n - 100 + 8)}
	}
}

// sgrNum parses one decimal SGR parameter; an empty one means 0.
func sgrNum(b []byte) (int, bool) {
	if len(b) == 0 {
		return 0, true
	}
	if len(b) > 5 {
		return 0, false
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// appendParams appends the parameters that set s from the default
// rendition, separated by ';', without a trailing separator.
func (s sgrState) appendParams(b []byte) []byte {
	start := len(b)
	sep := func() {
		if len(b) > start {
			b = append(b, ';')
		}
	}
	for _, ac := range attrCodes {
		if s.attrs&ac.a != 0 {
			sep()
			b = append(b, ac.code...)
		}
	}
	switch s.underline {
	case 0:
	case 1:
		sep()
		b = append(b, '4')
	default:
		sep()
		b = append(b, "4:"...)
		b = strconv.AppendUint(b, uint64(s.underline), 10)
	}
	for _, c := range []struct {
		base, ext int
		c         color
	}{{30, 38, s.fg}, {40, 48, s.bg}, {0, 58, s.ulColor}} {
		if c.c.kind == colorDefault {
			continue
		}
		sep()
		b = appendColor(b, c.base, c.ext, c.c)
	}
	return b
}

func appendColor(b []byte, base, ext int, c color) []byte {
	switch c.kind {
	case colorBasic:
		if base == 0 { // underline colour has no basic form
			return appendColor(b, base, ext, color{color256, c.v})
		}
		code := base + int(c.v)
		if c.v >= 8 {
			code = base + 60 + int(c.v) - 8
		}
		return strconv.AppendInt(b, int64(code), 10)
	case color256:
		b = strconv.AppendInt(b, int64(ext), 10)
		b = append(b, ";5;"...)
		return strconv.AppendUint(b, uint64(c.v), 10)
	default: // colorRGB
		b = strconv.AppendInt(b, int64(ext), 10)
		b = append(b, ";2;"...)
		b = strconv.AppendUint(b, uint64(c.v>>16&0xff), 10)
		b = append(b, ';')
		b = strconv.AppendUint(b, uint64(c.v>>8&0xff), 10)
		b = append(b, ';')
		return strconv.AppendUint(b, uint64(c.v&0xff), 10)
	}
}

// sgrReset ends a line whose rendition is not the default.
const sgrReset = "\x1b[0m"

// appendTransition appends the sequence that changes the rendition from
// "from" to "to" (to != from): a reset, or the full new state.
func appendTransition(b []byte, from, to sgrState) []byte {
	if to.isZero() {
		return append(b, sgrReset...)
	}
	b = append(b, "\x1b["...)
	if !from.isZero() {
		b = append(b, "0;"...)
	}
	b = to.appendParams(b)
	return append(b, 'm')
}
