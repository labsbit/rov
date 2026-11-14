package pipe

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// lines runs the discipline over chunks (then closes it) and returns the
// plain and .ansi lines.
func lines(chunks ...string) (plain, ansi []string) {
	d := newDiscipline(func(p, a []byte) {
		plain = append(plain, string(p))
		ansi = append(ansi, string(a))
	})
	for _, c := range chunks {
		d.write([]byte(c))
	}
	d.close()
	return plain, ansi
}

// stripSGR removes "ESC[…<final>" sequences: what .ansi adds to .log.
func stripSGR(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == esc && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

const dimMarker = "\x1b[2m── screen cleared ──\x1b[0m"

func TestDiscipline(t *testing.T) {
	tests := []struct {
		name   string
		chunks []string
		plain  []string
		ansi   []string // nil: same as plain
	}{
		{"lf", []string{"a\nb\n"}, []string{"a", "b"}, nil},
		{"crlf", []string{"a\r\nb\r\n"}, []string{"a", "b"}, nil},
		{"crlf split", []string{"a\r", "\nb\r", "\n"}, []string{"a", "b"}, nil},
		{"blank lines", []string{"\n\na\n"}, []string{"", "", "a"}, nil},
		{"cr rewind", []string{"50%\r60%\r100%\n"}, []string{"100%"}, nil},
		{"cr rewind split", []string{"50%\r", "60%\n"}, []string{"60%"}, nil},
		{"cr cr lf", []string{"a\r\r\nb\n"}, []string{"a", "b"}, nil},
		{"cr at start", []string{"\rabc\n"}, []string{"abc"}, nil},
		{"progress bar", []string{"\r\x1b[K[#   ] 10%", "\r\x1b[K[##  ] 50%", "\r\x1b[K[####] 100%\r\n"}, []string{"[####] 100%"}, nil},
		{"unterminated at eof", []string{"tail"}, []string{"tail"}, nil},

		{"sgr kept", []string{"\x1b[31mred\x1b[0m plain\n"}, []string{"red plain"}, []string{"\x1b[31mred\x1b[0m plain"}},
		{"sgr split", []string{"\x1b", "[3", "1mred\x1b[", "0m\n"}, []string{"red"}, []string{"\x1b[31mred\x1b[0m"}},
		{"sgr carried", []string{"\x1b[1;31mone\ntwo\x1b[0m\nthree\n"},
			[]string{"one", "two", "three"},
			[]string{"\x1b[1;31mone\x1b[0m", "\x1b[1;31mtwo\x1b[0m", "three"}},
		{"sgr to sgr", []string{"\x1b[31ma\x1b[1mb\n"}, []string{"ab"}, []string{"\x1b[31ma\x1b[0;1;31mb\x1b[0m"}},
		{"sgr off mid line", []string{"a\x1b[32mb\x1b[39mc\n"}, []string{"abc"}, []string{"a\x1b[32mb\x1b[0mc"}},
		{"sgr without text", []string{"\x1b[31m\x1b[0m\x1b[1m\n"}, []string{""}, []string{""}},
		{"sgr redundant", []string{"\x1b[31m\x1b[31mx\x1b[0m\x1b[0m\n"}, []string{"x"}, []string{"\x1b[31mx\x1b[0m"}},
		{"sgr 256 rgb", []string{"\x1b[38;5;208mx\x1b[48;2;1;2;3my\n"}, []string{"xy"},
			[]string{"\x1b[38;5;208mx\x1b[0;38;5;208;48;2;1;2;3my\x1b[0m"}},
		{"sgr colon", []string{"\x1b[38:2::10:20:30;4:3mz\n"}, []string{"z"}, []string{"\x1b[4:3;38;2;10;20;30mz\x1b[0m"}},
		{"sgr bright", []string{"\x1b[92;101mz\n"}, []string{"z"}, []string{"\x1b[92;101mz\x1b[0m"}},
		{"sgr empty resets", []string{"\x1b[1mx\x1b[my\n"}, []string{"xy"}, []string{"\x1b[1mx\x1b[0my"}},
		{"sgr kept across rewind", []string{"\x1b[33m10%\r20%\n"}, []string{"20%"}, []string{"\x1b[33m20%\x1b[0m"}},
		{"sgr private dropped", []string{"\x1b[>4;2mx\n"}, []string{"x"}, nil},
		{"sgr malformed ignored", []string{"\x1b[38;5mx\x1b[38;2;1;2mx\x1b[999999mx\n"}, []string{"xxx"}, nil},

		{"clear vite", []string{"a\n\x1b[2J\x1b[3J\x1b[Hb\n"}, []string{"a", ClearMarker, "b"}, []string{"a", dimMarker, "b"}},
		{"clear at start", []string{"\x1b[2J\x1b[Hhello\n"}, []string{"hello"}, nil},
		{"clear ends pending line", []string{"a\nb\x1bc"}, []string{"a", "b", ClearMarker}, []string{"a", "b", dimMarker}},
		{"clear home then J", []string{"a\n\x1b[H\x1b[Jb\n"}, []string{"a", ClearMarker, "b"}, []string{"a", dimMarker, "b"}},
		{"clear home 1;1 then 0J", []string{"a\n\x1b[1;1H\x1b[0Jb\n"}, []string{"a", ClearMarker, "b"}, []string{"a", dimMarker, "b"}},
		{"J without home", []string{"a\x1b[Jb\n"}, []string{"ab"}, nil},
		{"home text J", []string{"a\x1b[Hx\x1b[Jb\n"}, []string{"axb"}, nil},
		{"clears collapse", []string{"a\n\x1b[2J", "\x1b[H\x1b[2J\x1b[3J\x1bc"}, []string{"a", ClearMarker}, []string{"a", dimMarker}},
		{"clears with line between", []string{"a\n\x1b[2J\n\x1b[2J"}, []string{"a", ClearMarker, "", ClearMarker}, []string{"a", dimMarker, "", dimMarker}},
		{"clear split", []string{"a\n\x1b[", "2", "J", "b\n"}, []string{"a", ClearMarker, "b"}, []string{"a", dimMarker, "b"}},
		{"clear drops rewind", []string{"a\nb\r\x1b[2Jc\n"}, []string{"a", "b", ClearMarker, "c"}, []string{"a", "b", dimMarker, "c"}},
		{"RIS resets sgr", []string{"\x1b[31ma\n\x1bcb\n"}, []string{"a", ClearMarker, "b"}, []string{"\x1b[31ma\x1b[0m", dimMarker, "b"}},

		{"cursor movement", []string{"\x1b[2A\x1b[10;5Hx\x1b[3C\x1b[1Ky\n"}, []string{"xy"}, nil},
		{"alt screen", []string{"\x1b[?1049hx\x1b[?1049l\x1b[?25l\n"}, []string{"x"}, nil},
		{"osc bel", []string{"\x1b]0;title\x07x\n"}, []string{"x"}, nil},
		{"osc st", []string{"\x1b]2;t\x1b\\x\n"}, []string{"x"}, nil},
		{"osc split", []string{"\x1b]0;ti", "tle", "\x1b", "\\ok\n"}, []string{"ok"}, nil},
		{"osc 8 keeps text", []string{"see \x1b]8;id=1;https://x.y/\x1b\\link\x1b]8;;\x1b\\ here\n"}, []string{"see link here"}, nil},
		{"osc 8 in colour", []string{"\x1b[4m\x1b]8;;http://a\x07a\x1b]8;;\x07\x1b[24m\n"}, []string{"a"}, []string{"\x1b[4ma\x1b[0m"}},
		{"osc with newline inside", []string{"\x1b]0;a\nb\x07c\n"}, []string{"c"}, nil},
		{"dcs", []string{"\x1bPq#0;2;0;0;0#0~~\x1b\\x\n"}, []string{"x"}, nil},
		{"apc pm sos", []string{"\x1b_apc\x1b\\\x1b^pm\x1b\\\x1bXsos\x1b\\x\n"}, []string{"x"}, nil},
		{"charset", []string{"\x1b(Bx\x1b)0\x1b#8y\n"}, []string{"xy"}, nil},
		{"two byte escapes", []string{"\x1b7\x1b8\x1b=\x1b>\x1bMx\n"}, []string{"x"}, nil},
		{"c0 dropped", []string{"a\x07\x08\x00\x7fb\n"}, []string{"ab"}, nil},
		{"tab kept", []string{"a\tb\n"}, []string{"a\tb"}, nil},
		{"can aborts csi", []string{"\x1b[31\x18x\n"}, []string{"x"}, nil},
		{"esc restarts csi", []string{"\x1b[31\x1b[32mx\n"}, []string{"x"}, []string{"\x1b[32mx\x1b[0m"}},
		{"newline inside csi", []string{"a\x1b[3\n1mb\n"}, []string{"a", "b"}, []string{"a", "\x1b[31mb\x1b[0m"}},
		{"overlong csi dropped", []string{"\x1b[" + strings.Repeat("1;", 200) + "31mx\n"}, []string{"x"}, nil},

		{"utf8", []string{"héllo ✓ 🙂\n"}, []string{"héllo ✓ 🙂"}, nil},
		{"utf8 split", []string{"\xc3", "\xa9\xf0\x9f", "\x99", "\x82\n"}, []string{"é🙂"}, nil},
		{"invalid utf8", []string{"a\xffb\xc3\n"}, []string{"a\uFFFDb\uFFFD"}, nil},
		{"truncated before esc", []string{"\xe2\x82\x1b[31mx\n"}, []string{"\uFFFDx"}, []string{"\uFFFD\x1b[31mx\x1b[0m"}},
		{"invalid continuation", []string{"\xe0\x80\n"}, []string{"\uFFFD\uFFFD"}, nil},
		{"surrogate", []string{"\xed\xa0\x80\n"}, []string{"\uFFFD\uFFFD\uFFFD"}, nil},
		{"truncated at eof", []string{"x\xe2\x82"}, []string{"x\uFFFD"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plain, ansi := lines(tt.chunks...)
			want := tt.ansi
			if want == nil {
				want = tt.plain
			}
			if !equal(plain, tt.plain) {
				t.Errorf("plain = %q, want %q", plain, tt.plain)
			}
			if !equal(ansi, want) {
				t.Errorf("ansi = %q, want %q", ansi, want)
			}
		})
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDisciplineFlush(t *testing.T) {
	tests := []struct {
		name  string
		steps []string // "" means: the flush timer fires
		want  []string
	}{
		{"prompt then answer", []string{"Continue? [y/N] ", "", "y\r\n", "next\n"}, []string{"Continue? [y/N] ", "y", "next"}},
		{"terminator after flush", []string{"prompt", "", "\n", "after\n"}, []string{"prompt", "after"}},
		{"crlf after flush", []string{"prompt\r", "", "\n", "\n"}, []string{"prompt", ""}},
		{"continuation is a new line", []string{"Loading", "", "... done\n"}, []string{"Loading", "... done"}},
		{"nothing pending", []string{"a\n", "", "b\n"}, []string{"a", "b"}},
		{"progress flushed", []string{"10%\r", "", "20%\r", "", "100%\n"}, []string{"10%", "20%", "100%"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			d := newDiscipline(func(p, _ []byte) { got = append(got, string(p)) })
			for _, s := range tt.steps {
				if s == "" {
					d.flush()
					continue
				}
				d.write([]byte(s))
			}
			d.close()
			if !equal(got, tt.want) {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDisciplineLongLines(t *testing.T) {
	t.Run("split at cap", func(t *testing.T) {
		in := strings.Repeat("x", 3*MaxLineBytes+100) + "\n"
		plain, ansi := lines(in)
		wantLens := []int{MaxLineBytes, MaxLineBytes, MaxLineBytes, 100}
		if len(plain) != len(wantLens) || len(ansi) != len(wantLens) {
			t.Fatalf("got %d/%d lines, want %d", len(plain), len(ansi), len(wantLens))
		}
		for i, n := range wantLens {
			if len(plain[i]) != n {
				t.Errorf("line %d: %d bytes, want %d", i, len(plain[i]), n)
			}
		}
	})
	t.Run("split keeps runes whole", func(t *testing.T) {
		plain, _ := lines(strings.Repeat("é", MaxLineBytes) + "\n") // 2 bytes each
		for i, l := range plain {
			if !utf8.ValidString(l) || len(l) > MaxLineBytes {
				t.Fatalf("line %d invalid or too long (%d bytes)", i, len(l))
			}
		}
		if len(plain) != 2 {
			t.Fatalf("got %d lines, want 2", len(plain))
		}
	})
	t.Run("split carries colour", func(t *testing.T) {
		_, ansi := lines("\x1b[31m" + strings.Repeat("x", MaxLineBytes+1) + "\n")
		if len(ansi) != 2 || ansi[1] != "\x1b[31mx\x1b[0m" {
			t.Fatalf("second line = %q", ansi[len(ansi)-1])
		}
	})
	t.Run("ansi cap", func(t *testing.T) {
		in := strings.Repeat("\x1b[31mx\x1b[32my", MaxLineBytes/2) + "\n"
		plain, ansi := lines(in)
		if len(plain) < 2 {
			t.Fatalf("expected a split, got %d line(s)", len(plain))
		}
		for i := range ansi {
			if len(ansi[i]) > maxANSILineBytes {
				t.Errorf("ansi line %d: %d bytes > cap", i, len(ansi[i]))
			}
			if stripSGR(ansi[i]) != plain[i] {
				t.Errorf("line %d: ansi and plain disagree", i)
			}
		}
	})
	t.Run("huge input without newline stays bounded", func(t *testing.T) {
		n := 0
		d := newDiscipline(func(p, _ []byte) {
			if len(p) > MaxLineBytes {
				t.Fatalf("line of %d bytes", len(p))
			}
			n++
		})
		chunk := []byte(strings.Repeat("y", 32<<10))
		for range 1024 { // 32 MiB
			d.write(chunk)
		}
		d.close()
		if want := 32 << 20 / MaxLineBytes; n != want {
			t.Errorf("got %d lines, want %d", n, want)
		}
		if cap(d.plain) > 2*MaxLineBytes || cap(d.ansi) > 2*maxANSILineBytes {
			t.Errorf("buffers grew: %d / %d", cap(d.plain), cap(d.ansi))
		}
	})
}
