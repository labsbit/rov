package logs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/labsbit/rov/internal/pipe"
)

// pollInterval is how often Follow checks the file. Tests shorten it.
var pollInterval = 200 * time.Millisecond

// Follow sends lines of the selected run to out as they are written, like
// `tail -F`, until ctx is done; it then returns ctx.Err(). It first sends the
// last q.N lines (none if q.N is 0) as [Tail] would, then every complete line
// appended after them, filtered by q.Grep.
//
// It polls every 200 ms and survives both ways the file is replaced: rotation
// (.log → .log.1, new .log) and a restart (PrepareRun moves the run to .prev
// and creates an empty .log). It finishes reading the old file, then follows
// the new one from its start, so no line is lost or repeated; a restart is
// not signalled, the new run's lines simply follow. A file truncated in place
// is read again from the start. A missing file is waited for.
//
// Follow never closes out.
func Follow(ctx context.Context, q Query, out chan<- string) error {
	return FollowNumbered(ctx, q, out, nil)
}

// FollowNumbered is Follow that, before it sends anything, calls first (if
// not nil) with the number of (matching) lines of the run before the first
// line it sends: that line's 0-based number in the run, counted across the
// .1 file and the current one as `rov logs -n 0` prints them.
func FollowNumbered(ctx context.Context, q Query, out chan<- string, first func(n int)) error {
	if err := pipe.CheckName(q.Name); err != nil {
		return err
	}
	older, cur := q.files()
	match := q.matcher()
	send := func(l []byte) error {
		if match != nil && !match(l) {
			return nil
		}
		select {
		case out <- string(l):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	fw := &follower{path: cur, rotated: older, send: send, buf: make([]byte, chunkSize)}
	defer fw.close()
	segs, err := openSegments(older, cur)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if first != nil {
			first(0)
		}
	case err != nil:
		return err
	default:
		if err := fw.start(segs, q.N, match, first); err != nil {
			return err
		}
	}

	t := time.NewTicker(pollInterval)
	defer t.Stop()
	for {
		again, err := fw.poll()
		if err != nil {
			return err
		}
		if again {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// follower reads one path as it grows and is replaced.
type follower struct {
	path    string
	rotated string // where rotation moves path to; "" if it never does
	send    func([]byte) error
	buf     []byte

	f    *os.File
	info fs.FileInfo
	off  int64 // read position in f
	sp   splitter
}

// start sends the initial tail and keeps the current file open at the end
// of its last complete line, so a line being written is read whole later.
// first, if set, gets the number of lines before the tail.
func (fw *follower) start(segs []segment, n int, match func([]byte) bool, first func(int)) error {
	last := segs[len(segs)-1]
	if last.f.Name() == fw.path {
		off, err := lineEnd(last)
		if err != nil {
			closeAll(segs)
			return err
		}
		last.size = off
		segs[len(segs)-1] = last
		fw.f, fw.info, fw.off = last.f, last.info, off
		defer closeAll(segs[:len(segs)-1])
	} else {
		defer closeAll(segs)
	}
	if n == 0 {
		if first != nil {
			total := 0
			for _, s := range segs {
				c, err := countMatching(s, match)
				if err != nil {
					return err
				}
				total += c
			}
			first(total)
		}
		return nil
	}
	lines, total, err := tailSegments(segs, n, match, first != nil)
	if err != nil {
		return err
	}
	if first != nil {
		first(total - len(lines))
	}
	for _, l := range lines {
		if err := fw.send(l); err != nil {
			return err
		}
	}
	return nil
}

// lineEnd returns the offset just past the last newline of s.
func lineEnd(s segment) (int64, error) {
	buf := make([]byte, chunkSize)
	for end := s.size; end > 0; {
		start := max(0, end-chunkSize)
		b := buf[:end-start]
		if _, err := s.f.ReadAt(b, start); err != nil && err != io.EOF {
			return 0, err
		}
		for i := len(b) - 1; i >= 0; i-- {
			if b[i] == '\n' {
				return start + int64(i) + 1, nil
			}
		}
		end = start
	}
	return 0, nil
}

// poll reads what was appended and checks whether the path now names
// another file. again reports that it switched files and should poll again
// without waiting.
func (fw *follower) poll() (again bool, err error) {
	if fw.f == nil {
		f, err := os.Open(fw.path)
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return false, err
		}
		fw.f, fw.info, fw.off = f, info, 0
		fw.sp.reset()
	}
	if err := fw.drain(); err != nil {
		return false, err
	}
	fi, err := os.Stat(fw.path)
	switch {
	case err != nil:
		// Renamed away and not yet recreated: keep reading the old file.
		return false, nil
	case !os.SameFile(fi, fw.info):
		// Replaced: lines written before the switch are in the old file.
		if err := fw.drain(); err != nil {
			return false, err
		}
		old := fw.info
		fw.close()
		return true, fw.catchUp(old)
	case fi.Size() < fw.off:
		fw.off = 0
		fw.sp.reset()
		return true, nil
	}
	return false, nil
}

// catchUp sends the lines of the rotated file if it is not the file just
// finished: the path rotated twice since the last poll, and the file in
// between is now the .1 file. (A third rotation within one poll interval
// deletes that file; at the real cap that takes far more than 200 ms.)
func (fw *follower) catchUp(finished fs.FileInfo) error {
	if fw.rotated == "" {
		return nil
	}
	s, err := openSegment(fw.rotated)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer s.f.Close()
	if os.SameFile(s.info, finished) {
		return nil
	}
	return scan(s, fw.send)
}

// drain reads f from off to its end and sends the complete lines.
func (fw *follower) drain() error {
	for {
		n, err := fw.f.ReadAt(fw.buf, fw.off)
		if n > 0 {
			fw.off += int64(n)
			if serr := fw.sp.feed(fw.buf[:n], fw.send); serr != nil {
				return serr
			}
		}
		if err == io.EOF || n == 0 {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (fw *follower) close() {
	if fw.f != nil {
		fw.f.Close()
		fw.f = nil
	}
	fw.sp.reset()
}
