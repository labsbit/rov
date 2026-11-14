package pipe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const (
	// DefaultMax is the rotation cap per file when Options.Max is 0.
	DefaultMax int64 = 64 << 20
	// DefaultFlushAfter is the partial-line timeout when Options.FlushAfter
	// is 0.
	DefaultFlushAfter = 500 * time.Millisecond

	readSize = 32 << 10
)

// Options configures [Run].
type Options struct {
	Dir        string        // absolute log directory, created if missing
	Name       string        // process name: the file base name
	Max        int64         // rotation cap per file; 0 means DefaultMax
	FlushAfter time.Duration // partial-line timeout; 0 means DefaultFlushAfter
}

// Run reads raw terminal output from r and appends it, line by line, to the
// .log and .ansi files of opt.Name until r reaches EOF or ctx is done. Lines
// are written after every read and a partial line after opt.FlushAfter of
// silence; on exit any partial line is flushed too.
//
// EOF and ctx cancellation return nil; read and write errors are returned.
// Files are opened with O_APPEND, so Run continues a run [PrepareRun] began.
func Run(ctx context.Context, opt Options, r io.Reader) (err error) {
	if err := CheckName(opt.Name); err != nil {
		return err
	}
	if opt.Max <= 0 {
		opt.Max = DefaultMax
	}
	if opt.FlushAfter <= 0 {
		opt.FlushAfter = DefaultFlushAfter
	}
	if err := os.MkdirAll(opt.Dir, 0o755); err != nil {
		return err
	}
	w, err := openPair(opt.Dir, opt.Name, opt.Max)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, w.close()) }()
	d := newDiscipline(w.line)

	chunks := make(chan []byte)
	ack := make(chan struct{})
	readErr := make(chan error, 1)
	quit := make(chan struct{})
	defer close(quit)
	go readLoop(r, chunks, ack, readErr, quit)

	timer := time.NewTimer(opt.FlushAfter)
	timer.Stop()
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			d.close()
			return nil
		case b := <-chunks:
			d.write(b)
			ack <- struct{}{}
			if err := w.flush(); err != nil {
				return fmt.Errorf("write log: %w", err)
			}
			if d.pending() {
				timer.Reset(opt.FlushAfter)
			} else {
				timer.Stop()
			}
		case <-timer.C:
			d.flush()
			if err := w.flush(); err != nil {
				return fmt.Errorf("write log: %w", err)
			}
		case err := <-readErr:
			d.close()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read pane output: %w", err)
		}
	}
}

// readLoop reads r into one reused buffer and hands each chunk to Run,
// waiting for ack before reading again. It ends at the first error or when
// quit closes (a Read already in progress keeps the goroutine until it
// returns; in __pipe the process exits anyway).
func readLoop(r io.Reader, chunks chan<- []byte, ack <-chan struct{}, readErr chan<- error, quit <-chan struct{}) {
	buf := make([]byte, readSize)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			select {
			case chunks <- buf[:n]:
			case <-quit:
				return
			}
			select {
			case <-ack:
			case <-quit:
				return
			}
		}
		if err != nil {
			readErr <- err
			return
		}
	}
}
