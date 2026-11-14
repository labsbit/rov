package tmux

import (
	"strconv"
	"strings"
)

// EncodeValue makes s safe to store in a user option and read back through
// a format: printable ASCII stays as it is (so `tmux show-options` stays
// readable), and '%', '$', '\' plus every other byte become %XX. tmux would
// otherwise vis-encode control characters and escape '$' on output, and a
// value holding a tab or newline would break the one-line-per-session list.
func EncodeValue(s string) string {
	if !needsEncoding(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if plain(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigit[c>>4])
		b.WriteByte(hexDigit[c&0xf])
	}
	return b.String()
}

const hexDigit = "0123456789ABCDEF"

func plain(c byte) bool {
	return c >= 0x20 && c < 0x7f && c != '%' && c != '$' && c != '\\'
}

func needsEncoding(s string) bool {
	for i := 0; i < len(s); i++ {
		if !plain(s[i]) {
			return true
		}
	}
	return false
}

// DecodeValue reverses EncodeValue. A '%' not followed by two hex digits is
// kept as it is, so a value set by hand reads back unchanged.
func DecodeValue(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+3], 16, 8); err == nil {
				b.WriteByte(byte(v))
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
