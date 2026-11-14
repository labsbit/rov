// Package logs reads the log files `rov __pipe` writes (see package pipe):
// tail and grep across rotation, follow like `tail -F`, and clean up.
//
// Only complete lines are ever returned. A trailing fragment without a
// newline is a line the writer is still writing; it shows up once finished.
package logs

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"regexp"

	"github.com/labsbit/rov/internal/pipe"
)

// Run selects which run of a process to read.
type Run int

const (
	Current Run = iota // the running (or last) run: <name>.log.1 then <name>.log
	Prev               // the run before the last start: <name>.prev.log
)

// String returns "current" or "prev", the values of the JSON "run" field.
func (r Run) String() string {
	if r == Prev {
		return "prev"
	}
	return "current"
}

// Query selects log lines.
type Query struct {
	Dir, Name string
	Run       Run
	N         int            // last N (matching) lines; 0 = all
	Grep      *regexp.Regexp // keep only matching lines; nil = all
	ANSI      bool           // read the .ansi files (Grep still matches the plain text)
}

// maxReadLine caps a line read back. Lines pipe writes are far shorter; the
// cap only bounds memory on files written by something else.
const maxReadLine = 1 << 20

// files returns the paths of the older and the current segment; older is
// empty when the run has no rotated part.
func (q Query) files() (older, cur string) {
	p := pipe.Paths(q.Dir, q.Name)
	switch {
	case q.Run == Prev && q.ANSI:
		return "", p.PrevANSI
	case q.Run == Prev:
		return "", p.PrevLog
	case q.ANSI:
		return p.ANSI1, p.ANSI
	default:
		return p.Log1, p.Log
	}
}

// matcher returns the line filter of q, or nil when every line matches.
func (q Query) matcher() func([]byte) bool {
	if q.Grep == nil {
		return nil
	}
	if q.ANSI {
		return func(l []byte) bool { return q.Grep.Match(stripSGR(l)) }
	}
	return q.Grep.Match
}

// segment is one open log file and the size it had when opened; reads stop
// there, so a file growing meanwhile yields a consistent snapshot.
type segment struct {
	f    *os.File
	info fs.FileInfo
	size int64
}

func closeAll(segs []segment) {
	for _, s := range segs {
		s.f.Close()
	}
}

// openSegments opens the existing files of older and cur, oldest first. If a
// rotation renames files while it opens them it retries, so the result
// never holds the same file twice nor skips one. The error wraps
// fs.ErrNotExist when neither file exists.
func openSegments(older, cur string) ([]segment, error) {
	var segs []segment
	for range 3 {
		segs = segs[:0]
		for _, path := range []string{older, cur} {
			if path == "" {
				continue
			}
			s, err := openSegment(path)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				closeAll(segs)
				return nil, err
			}
			segs = append(segs, s)
		}
		if stable(segs, older, cur) {
			break
		}
		closeAll(segs)
		segs = nil
	}
	if len(segs) == 0 {
		// Either nothing exists, or rotation raced three times: read cur.
		s, err := openSegment(cur)
		if err != nil {
			return nil, err
		}
		segs = append(segs, s)
	}
	return segs, nil
}

func openSegment(path string) (segment, error) {
	f, err := os.Open(path)
	if err != nil {
		return segment{}, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return segment{}, err
	}
	return segment{f: f, info: fi, size: fi.Size()}, nil
}

// stable reports whether the paths still name exactly the opened files.
func stable(segs []segment, paths ...string) bool {
	i := 0
	for _, path := range paths {
		if path == "" {
			continue
		}
		fi, err := os.Stat(path)
		opened := i < len(segs) && segs[i].f.Name() == path
		switch {
		case err != nil && !opened:
		case err == nil && opened && os.SameFile(fi, segs[i].info):
		default:
			return false
		}
		if opened {
			i++
		}
	}
	return true
}

// stripSGR removes "ESC[…<final>" sequences: the only escapes .ansi holds.
func stripSGR(l []byte) []byte {
	if bytes.IndexByte(l, 0x1b) < 0 {
		return l
	}
	out := make([]byte, 0, len(l))
	for i := 0; i < len(l); i++ {
		if l[i] == 0x1b && i+1 < len(l) && l[i+1] == '[' {
			j := i + 2
			for j < len(l) && (l[j] < 0x40 || l[j] > 0x7e) {
				j++
			}
			i = j
			continue
		}
		out = append(out, l[i])
	}
	return out
}

// splitter cuts a byte stream into lines, keeping at most maxReadLine bytes
// of each (the rest of an overlong line is dropped).
type splitter struct{ buf []byte }

func (s *splitter) feed(p []byte, emit func([]byte) error) error {
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			s.add(p)
			return nil
		}
		s.add(p[:i])
		p = p[i+1:]
		err := emit(s.buf)
		s.buf = s.buf[:0]
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *splitter) add(p []byte) {
	if room := maxReadLine - len(s.buf); len(p) > room {
		p = p[:room]
	}
	s.buf = append(s.buf, p...)
}

func (s *splitter) reset() { s.buf = s.buf[:0] }
