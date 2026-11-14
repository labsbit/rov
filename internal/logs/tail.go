package logs

import (
	"bytes"
	"io"
	"os"

	"github.com/labsbit/rov/internal/pipe"
)

// ScrollbackLines is how many lines the TUI keeps for the selected process.
const ScrollbackLines = 50_000

const chunkSize = 64 << 10

// Tail returns the last q.N lines of the selected run (all lines if q.N is
// 0), oldest first, reading the rotated .1 file and then the current one as
// one log. With q.Grep it returns the last q.N matching lines.
//
// total is the number of lines that pass the filter across both files: all
// lines without q.Grep, the matching ones with it (the JSON total_lines).
//
// Without q.Grep and with q.N > 0 the files are read backwards from the end,
// so the cost follows the lines returned, plus a newline count for total.
// The error wraps fs.ErrNotExist when the run has no log file.
func Tail(q Query) (lines []string, total int, err error) {
	return tail(q, q.N, true)
}

// LoadTail returns the last maxLines lines (q.N, if smaller and non-zero),
// for the TUI scrollback; use [ScrollbackLines]. It skips counting total.
func LoadTail(q Query, maxLines int) ([]string, error) {
	n := maxLines
	if q.N > 0 && q.N < n {
		n = q.N
	}
	lines, _, err := tail(q, n, false)
	return lines, err
}

func tail(q Query, n int, wantTotal bool) ([]string, int, error) {
	if err := pipe.CheckName(q.Name); err != nil {
		return nil, 0, err
	}
	segs, err := openSegments(q.files())
	if err != nil {
		return nil, 0, err
	}
	defer closeAll(segs)
	lines, total, err := tailSegments(segs, n, q.matcher(), wantTotal)
	if err != nil {
		return nil, 0, err
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = string(l)
	}
	return out, total, nil
}

// tailSegments returns the last n lines (all if n is 0) of segs that match
// (nil matches everything) and, if wantTotal or match is set, how many lines
// match in all.
func tailSegments(segs []segment, n int, match func([]byte) bool, wantTotal bool) ([][]byte, int, error) {
	if match == nil && n > 0 {
		lines, err := lastLines(segs, n)
		if err != nil || !wantTotal {
			return lines, 0, err
		}
		total := 0
		for _, s := range segs {
			c, err := countLines(s)
			if err != nil {
				return nil, 0, err
			}
			total += c
		}
		return lines, total, nil
	}
	ring := newRing(n)
	total := 0
	for _, s := range segs {
		err := scan(s, func(l []byte) error {
			if match == nil || match(l) {
				ring.push(l)
				total++
			}
			return nil
		})
		if err != nil {
			return nil, 0, err
		}
	}
	return ring.lines(), total, nil
}

// scan calls fn for each complete line of s, in order.
func scan(s segment, fn func([]byte) error) error {
	var sp splitter
	buf := make([]byte, chunkSize)
	r := io.NewSectionReader(s.f, 0, s.size)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if ferr := sp.feed(buf[:n], fn); ferr != nil {
				return ferr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// countMatching counts the complete lines of s that match (all if match is
// nil).
func countMatching(s segment, match func([]byte) bool) (int, error) {
	if match == nil {
		return countLines(s)
	}
	n := 0
	err := scan(s, func(l []byte) error {
		if match(l) {
			n++
		}
		return nil
	})
	return n, err
}

// countLines counts the complete lines of s.
func countLines(s segment) (int, error) {
	buf := make([]byte, 4*chunkSize)
	r := io.NewSectionReader(s.f, 0, s.size)
	total := 0
	for {
		n, err := r.Read(buf)
		total += bytes.Count(buf[:n], []byte{'\n'})
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return 0, err
		}
	}
}

// lastLines returns the last n complete lines across segs, oldest first,
// reading each file backwards in chunks.
func lastLines(segs []segment, n int) ([][]byte, error) {
	var out [][]byte
	for i := len(segs) - 1; i >= 0 && len(out) < n; i-- {
		lines, err := lastLinesOf(segs[i], n-len(out))
		if err != nil {
			return nil, err
		}
		out = append(lines, out...)
	}
	return out, nil
}

// readChunk is (*os.File).ReadAt; tests replace it to truncate a file
// between two reads.
var readChunk = (*os.File).ReadAt

func lastLinesOf(s segment, n int) ([][]byte, error) {
	var chunks [][]byte // newest first
	newlines, pos := 0, s.size
	// n+1 newlines pin down the start of the n-th line from the end.
	for pos > 0 && newlines <= n {
		size := min(int64(chunkSize), pos)
		pos -= size
		b := make([]byte, size)
		k, err := readChunk(s.f, b, pos)
		if err != nil && err != io.EOF {
			return nil, err
		}
		if k < len(b) {
			// The file shrank since it was opened (truncated): it now ends at
			// pos+k, and what was read beyond that is gone.
			b, chunks, newlines = b[:k], chunks[:0], 0
		}
		newlines += bytes.Count(b, []byte{'\n'})
		chunks = append(chunks, b)
	}
	var buf []byte
	if len(chunks) == 1 {
		buf = chunks[0]
	} else {
		buf = make([]byte, 0, s.size-pos)
		for i := len(chunks) - 1; i >= 0; i-- {
			buf = append(buf, chunks[i]...)
		}
	}
	end := bytes.LastIndexByte(buf, '\n')
	if end < 0 {
		return nil, nil // no complete line
	}
	lines := bytes.Split(buf[:end], []byte{'\n'})
	if pos > 0 {
		lines = lines[1:] // may start mid-line
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i, l := range lines {
		if len(l) > maxReadLine {
			lines[i] = l[:maxReadLine]
		}
	}
	return lines, nil
}

// ring keeps the last n lines pushed (all of them if n is 0).
type ring struct {
	n     int
	buf   [][]byte
	start int
}

func newRing(n int) *ring { return &ring{n: n} }

func (r *ring) push(l []byte) {
	c := bytes.Clone(l)
	if r.n == 0 || len(r.buf) < r.n {
		r.buf = append(r.buf, c)
		return
	}
	r.buf[r.start] = c
	r.start = (r.start + 1) % r.n
}

func (r *ring) lines() [][]byte {
	return append(r.buf[r.start:len(r.buf):len(r.buf)], r.buf[:r.start]...)
}
