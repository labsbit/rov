package pipe

import (
	"bufio"
	"context"
	"errors"
	"os"

	"github.com/labsbit/rov/internal/lock"
)

// bufSize is the write buffer of each file. Lines are buffered and flushed
// after every read batch and on the partial-line timer.
const bufSize = 64 << 10

// pair writes aligned lines to the .log and .ansi files and rotates both at
// the same line. Errors are sticky: after the first one every call is a no-op
// and err reports it.
type pair struct {
	paths    FilesSet
	max      int64
	rotating string // the lock rotation and PrepareRun take (rotateLock)

	log, ansi         *os.File
	logBuf, ansiBuf   *bufio.Writer
	logSize, ansiSize int64

	// detached is set once the paths no longer name this writer's files
	// (PrepareRun moved them for a new run): no more rotation, and lines
	// past the cap are dropped.
	detached bool
	err      error
}

func openPair(dir, name string, max int64) (*pair, error) {
	p := &pair{paths: Paths(dir, name), max: max, rotating: rotateLock(dir, name)}
	if err := p.open(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *pair) open() error {
	var err error
	if p.log, p.logSize, err = openAppend(p.paths.Log); err != nil {
		return err
	}
	if p.ansi, p.ansiSize, err = openAppend(p.paths.ANSI); err != nil {
		p.log.Close()
		return err
	}
	if p.logBuf == nil {
		p.logBuf = bufio.NewWriterSize(p.log, bufSize)
		p.ansiBuf = bufio.NewWriterSize(p.ansi, bufSize)
	} else {
		p.logBuf.Reset(p.log)
		p.ansiBuf.Reset(p.ansi)
	}
	return nil
}

func openAppend(path string) (*os.File, int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

// line writes one line to both files, rotating first if it would push
// either file past the cap. A line is never split by rotation, and an empty
// file always takes the line, however long.
func (p *pair) line(plain, ansi []byte) {
	if p.err != nil {
		return
	}
	nLog, nANSI := int64(len(plain)+1), int64(len(ansi)+1)
	if (p.logSize > 0 || p.ansiSize > 0) && (p.logSize+nLog > p.max || p.ansiSize+nANSI > p.max) {
		if !p.detached {
			p.err = p.rotate()
		}
		if p.err != nil || p.detached {
			return
		}
	}
	p.logBuf.Write(plain)
	p.logBuf.WriteByte('\n')
	p.ansiBuf.Write(ansi)
	p.ansiBuf.WriteByte('\n')
	p.logSize += nLog
	p.ansiSize += nANSI
}

// flush writes buffered lines to the files.
func (p *pair) flush() error {
	if p.err != nil {
		return p.err
	}
	if err := p.logBuf.Flush(); err != nil {
		p.err = err
		return err
	}
	if err := p.ansiBuf.Flush(); err != nil {
		p.err = err
	}
	return p.err
}

// rotate moves both files to .1 and starts new ones. If the paths no longer
// name this writer's files it detaches instead.
//
// The check, the renames and the reopen happen under the rotate lock, which
// PrepareRun takes too: otherwise PrepareRun could move the files and create
// the new run's right after the check, and this writer would rename the new
// run's files and then write into them. If the lock can't be had the writer
// detaches: it can't tell whether the files are still its own.
func (p *pair) rotate() error {
	if err := p.flush(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), rotateLockWait)
	release, err := lock.Acquire(ctx, p.rotating)
	cancel()
	if err != nil {
		p.detached = true
		return nil
	}
	defer release()
	if !owns(p.paths.Log, p.log) || !owns(p.paths.ANSI, p.ansi) {
		p.detached = true
		return nil
	}
	if testHookRotate != nil {
		testHookRotate()
	}
	if err := os.Rename(p.paths.Log, p.paths.Log1); err != nil {
		return err
	}
	if err := os.Rename(p.paths.ANSI, p.paths.ANSI1); err != nil {
		return err
	}
	if err := errors.Join(p.log.Close(), p.ansi.Close()); err != nil {
		return err
	}
	return p.open()
}

// testHookRotate, if set, runs in rotate between the ownership check and the
// renames.
var testHookRotate func()

// owns reports whether path still names the open file f.
func owns(path string, f *os.File) bool {
	pfi, err := os.Stat(path)
	if err != nil {
		return false
	}
	ffi, err := f.Stat()
	return err == nil && os.SameFile(pfi, ffi)
}

// close flushes and closes both files.
func (p *pair) close() error {
	err := p.flush()
	return errors.Join(err, p.log.Close(), p.ansi.Close())
}
