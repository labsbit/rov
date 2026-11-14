package pipe

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzDiscipline checks the invariants of the line discipline on arbitrary
// input split at arbitrary points:
//
//   - output does not depend on how the input is split into reads;
//   - every line pair agrees: the .ansi line minus its SGR is the .log line;
//   - .log lines hold valid UTF-8 and no control bytes other than tab;
//   - .ansi lines hold no escape other than complete SGR sequences;
//   - lines respect the length caps.
//
// Both files always get the same number of lines because every emit writes
// one line to each.
func FuzzDiscipline(f *testing.F) {
	for _, s := range []string{
		"a\r\nb\rc\n",
		"\x1b[1;31mred\nnext\x1b[0m\n",
		"\x1b[2J\x1b[3J\x1b[Hx\n\x1bc",
		"\x1b]8;;http://x\x1b\\t\x1b]8;;\x07",
		"\x1bPq\x1b\\\x1b(B\x1b[?1049h",
		"\xff\xc3\xe2\x82\x1b[38:2::1:2:3m",
		"\x1b[" + strings.Repeat("9", 300) + "m",
	} {
		f.Add([]byte(s), uint16(1), uint16(7))
	}
	f.Fuzz(func(t *testing.T, data []byte, step1, step2 uint16) {
		whole := collect(data, nil)
		split := collect(data, []int{int(step1%64) + 1, int(step2%64) + 1})
		if len(whole.plain) != len(split.plain) {
			t.Fatalf("split changes line count: %d vs %d", len(whole.plain), len(split.plain))
		}
		for i := range whole.plain {
			if whole.plain[i] != split.plain[i] || whole.ansi[i] != split.ansi[i] {
				t.Fatalf("line %d differs when split", i)
			}
		}
		for i, p := range whole.plain {
			a := whole.ansi[i]
			if !utf8.ValidString(p) || !utf8.ValidString(a) {
				t.Fatalf("line %d: invalid UTF-8", i)
			}
			if len(p) > MaxLineBytes || len(a) > maxANSILineBytes {
				t.Fatalf("line %d: too long (%d / %d)", i, len(p), len(a))
			}
			for _, c := range []byte(p) {
				if c < 0x20 && c != '\t' || c == 0x7f {
					t.Fatalf("line %d: control byte %#x in .log", i, c)
				}
			}
			if !onlySGR(a) {
				t.Fatalf("line %d: non-SGR escape in .ansi: %q", i, a)
			}
			if stripSGR(a) != p {
				t.Fatalf("line %d: .ansi %q does not match .log %q", i, a, p)
			}
		}
	})
}

type output struct{ plain, ansi []string }

// collect runs data through a discipline in chunks of the given sizes
// (cycled), or in one write if steps is nil. Timer flushes are left out on
// purpose: they depend on timing, not on the bytes (see TestDisciplineFlush).
func collect(data []byte, steps []int) output {
	var o output
	d := newDiscipline(func(p, a []byte) {
		o.plain = append(o.plain, string(p))
		o.ansi = append(o.ansi, string(a))
	})
	if steps == nil {
		d.write(data)
	} else {
		for i := 0; len(data) > 0; i++ {
			n := min(steps[i%len(steps)], len(data))
			d.write(data[:n])
			data = data[n:]
		}
	}
	d.close()
	return o
}

// onlySGR reports whether every ESC in s starts a complete "ESC[<params>m"
// with parameter bytes only.
func onlySGR(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != esc {
			continue
		}
		if i+1 >= len(s) || s[i+1] != '[' {
			return false
		}
		j := i + 2
		for j < len(s) && s[j] >= 0x30 && s[j] <= 0x3f {
			j++
		}
		if j >= len(s) || s[j] != 'm' {
			return false
		}
		i = j
	}
	return true
}
