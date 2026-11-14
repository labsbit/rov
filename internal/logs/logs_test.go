package logs

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/labsbit/rov/internal/pipe"
)

func write(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, path, s string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(s); err != nil {
		t.Fatal(err)
	}
}

func seq(from, to int, f string) []string {
	var out []string
	for i := from; i <= to; i++ {
		out = append(out, fmt.Sprintf(f, i))
	}
	return out
}

// fixture: .log.1 = line 1..5, .log = line 6..10 + an unfinished fragment;
// .ansi pair coloured; prev = p1..p3. Lines divisible by 3 say "error".
func fixture(t *testing.T) string {
	dir := t.TempDir()
	p := pipe.Paths(dir, "api")
	line := func(i int) string {
		if i%3 == 0 {
			return fmt.Sprintf("line %d error", i)
		}
		return fmt.Sprintf("line %d", i)
	}
	var l1, l, a1, a string
	for i := 1; i <= 10; i++ {
		plain := line(i) + "\n"
		ansi := "\x1b[31m" + strings.Replace(line(i), "err", "err\x1b[0;1m", 1) + "\x1b[0m\n"
		if i <= 5 {
			l1, a1 = l1+plain, a1+ansi
		} else {
			l, a = l+plain, a+ansi
		}
	}
	write(t, p.Log1, l1)
	write(t, p.Log, l+"line 11 still writ")
	write(t, p.ANSI1, a1)
	write(t, p.ANSI, a)
	write(t, p.PrevLog, "p1\np2 error\np3\n")
	write(t, p.PrevANSI, "p1\n\x1b[1mp2 error\x1b[0m\np3\n")
	return dir
}

func TestTail(t *testing.T) {
	dir := fixture(t)
	all := []string{"line 1", "line 2", "line 3 error", "line 4", "line 5", "line 6 error", "line 7", "line 8", "line 9 error", "line 10"}
	tests := []struct {
		name  string
		q     Query
		want  []string
		total int
	}{
		{"all", Query{}, all, 10},
		{"last 3", Query{N: 3}, all[7:], 10},
		{"across rotation", Query{N: 7}, all[3:], 10},
		{"more than there is", Query{N: 100}, all, 10},
		{"grep", Query{Grep: regexp.MustCompile(`error`)}, []string{"line 3 error", "line 6 error", "line 9 error"}, 3},
		{"grep last 2", Query{N: 2, Grep: regexp.MustCompile(`error`)}, []string{"line 6 error", "line 9 error"}, 3},
		{"grep no match", Query{N: 2, Grep: regexp.MustCompile(`panic`)}, nil, 0},
		{"ansi", Query{N: 1, ANSI: true}, []string{"\x1b[31mline 10\x1b[0m"}, 10},
		{"ansi grep matches plain text", Query{N: 1, ANSI: true, Grep: regexp.MustCompile(`9 error`)},
			[]string{"\x1b[31mline 9 err\x1b[0;1mor\x1b[0m"}, 1},
		{"prev", Query{Run: Prev}, []string{"p1", "p2 error", "p3"}, 3},
		{"prev last 1", Query{Run: Prev, N: 1}, []string{"p3"}, 3},
		{"prev ansi grep", Query{Run: Prev, ANSI: true, Grep: regexp.MustCompile(`^p2`)}, []string{"\x1b[1mp2 error\x1b[0m"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.q.Dir, tt.q.Name = dir, "api"
			got, total, err := Tail(tt.q)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) || total != tt.total {
				t.Errorf("got %q (total %d), want %q (total %d)", got, total, tt.want, tt.total)
			}
		})
	}
}

func TestTailMissing(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := Tail(Query{Dir: dir, Name: "nope"}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("err = %v, want ErrNotExist", err)
	}
	if _, _, err := Tail(Query{Dir: dir, Name: "../etc"}); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("invalid name: err = %v", err)
	}
	// Only the rotated part exists (between rename and re-create).
	write(t, pipe.Paths(dir, "x").Log1, "a\nb\n")
	if got, total, err := Tail(Query{Dir: dir, Name: "x", N: 1}); err != nil || !slices.Equal(got, []string{"b"}) || total != 2 {
		t.Errorf("got %q %d %v", got, total, err)
	}
	// Empty current file.
	write(t, pipe.Paths(dir, "y").Log, "")
	if got, total, err := Tail(Query{Dir: dir, Name: "y", N: 5}); err != nil || len(got) != 0 || total != 0 {
		t.Errorf("got %q %d %v", got, total, err)
	}
}

// TestTailBackwards compares the backwards reader with a naive split on
// random line lengths around the chunk size.
func TestTailBackwards(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for round := range 20 {
		dir := t.TempDir()
		p := pipe.Paths(dir, "b")
		var want []string
		var files [2]strings.Builder
		for i := range 300 {
			n := r.IntN(8)
			switch r.IntN(4) {
			case 0:
				n = r.IntN(3 * chunkSize)
			case 1:
				n = chunkSize - 2 + r.IntN(4)
			}
			l := fmt.Sprintf("%d:%s", i, strings.Repeat("x", n))
			if r.IntN(10) == 0 {
				l = ""
			}
			want = append(want, l)
			files[min(i/150, 1)].WriteString(l + "\n")
		}
		write(t, p.Log1, files[0].String())
		write(t, p.Log, files[1].String()+"frag")
		for _, n := range []int{1, 2, 149, 150, 151, 299, 300, 1000} {
			got, total, err := Tail(Query{Dir: dir, Name: "b", N: n})
			if err != nil {
				t.Fatal(err)
			}
			w := want[max(0, len(want)-n):]
			if !slices.Equal(got, w) || total != 300 {
				t.Fatalf("round %d, n %d: got %d lines (total %d), want %d", round, n, len(got), total, len(w))
			}
		}
	}
}

func TestLoadTail(t *testing.T) {
	dir := fixture(t)
	got, err := LoadTail(Query{Dir: dir, Name: "api", ANSI: true}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"\x1b[31mline 9 err\x1b[0;1mor\x1b[0m", "\x1b[31mline 10\x1b[0m"}; !slices.Equal(got, want) {
		t.Errorf("got %q", got)
	}
}

func TestRunString(t *testing.T) {
	if Current.String() != "current" || Prev.String() != "prev" {
		t.Error("Run.String")
	}
}

func BenchmarkTail128MiB(b *testing.B) {
	dir := b.TempDir()
	p := pipe.Paths(dir, "big")
	line := strings.Repeat("0123456789abcdef", 8) + "\n" // 129 bytes
	f, err := os.Create(p.Log)
	if err != nil {
		b.Fatal(err)
	}
	block := strings.Repeat(line, 8192)
	for written := 0; written < 128<<20; written += len(block) {
		f.WriteString(block)
	}
	f.Close()
	for _, n := range []int{100, ScrollbackLines} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			for b.Loop() {
				if _, err := LoadTail(Query{Dir: dir, Name: "big"}, n); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	b.Run("n=100+total", func(b *testing.B) {
		for b.Loop() {
			if _, _, err := Tail(Query{Dir: dir, Name: "big", N: 100}); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// TestLastLinesOfTruncated: a file truncated while its last lines are read
// gives lines it holds, never NUL bytes for the part that is gone.
func TestLastLinesOfTruncated(t *testing.T) {
	const line = "old line\n"
	n := 2*chunkSize/len(line) + 50 // lines over three chunks; read them all
	old := strings.Repeat(line, n)
	for _, cut := range []int64{
		int64(len(old)) - chunkSize - 4*int64(len(line)) - 3, // into the second chunk read
		int64(len(line)), // the first line only
		0,
	} {
		t.Run(fmt.Sprint(cut), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "a.log")
			write(t, path, old)
			s, err := openSegment(path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.f.Close()
			reads := 0
			readChunk = func(f *os.File, b []byte, off int64) (int, error) {
				if reads++; reads == 2 {
					if err := os.Truncate(path, cut); err != nil {
						t.Fatal(err)
					}
				}
				return f.ReadAt(b, off)
			}
			defer func() { readChunk = (*os.File).ReadAt }()
			lines, err := lastLinesOf(s, n)
			if err != nil {
				t.Fatal(err)
			}
			if reads < 2 {
				t.Fatalf("only %d read", reads)
			}
			if want := int(cut) / len(line); len(lines) != want {
				t.Errorf("got %d lines, want %d", len(lines), want)
			}
			for i, l := range lines {
				if string(l) != "old line" {
					t.Fatalf("line %d is %q", i, l)
				}
			}
		})
	}
}
