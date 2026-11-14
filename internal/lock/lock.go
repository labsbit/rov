// Package lock provides advisory, process-exclusive file locks (flock(2)).
//
// rov serialises concurrent callers — two agents, or an agent and the TUI —
// with one lock file per process (<log_dir>/.lock/<name>) and one for writes
// to rov.yaml (<log_dir>/.lock/config).
//
// The lock belongs to the open file description, so it is released when the
// holder calls release, closes the file, or dies: a crashed rov can never leave
// a stale lock behind. Lock files are never deleted; deleting one would let a
// later caller lock a fresh inode while an earlier one still holds the old.
package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ErrLocked is returned by TryAcquire when another holder has the lock.
var ErrLocked = errors.New("lock is held by another process")

// Poll intervals while waiting for a held lock: start fast so an uncontended
// hand-over is quick, back off so a long wait doesn't spin.
const (
	minPoll = 5 * time.Millisecond
	maxPoll = 100 * time.Millisecond
)

// Acquire takes an exclusive lock on path, creating the file and its parent
// directories if needed. It waits until the lock is free or ctx is done; give
// ctx a deadline to bound the wait. The returned release function unlocks and
// is safe to call more than once.
func Acquire(ctx context.Context, path string) (release func(), err error) {
	f, err := open(path)
	if err != nil {
		return nil, err
	}
	wait := minPoll
	for {
		ok, err := tryLock(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if ok {
			return releaser(f), nil
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			f.Close()
			return nil, fmt.Errorf("lock %s: still held by another rov: %w", path, ctx.Err())
		case <-t.C:
		}
		wait = min(wait*2, maxPoll)
	}
}

// TryAcquire takes an exclusive lock on path without waiting. It returns an
// error wrapping ErrLocked when another holder has it.
func TryAcquire(path string) (release func(), err error) {
	f, err := open(path)
	if err != nil {
		return nil, err
	}
	ok, err := tryLock(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if !ok {
		f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, ErrLocked)
	}
	return releaser(f), nil
}

func open(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("lock: create directory for %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("lock: open %s: %w", path, err)
	}
	return f, nil
}

// tryLock reports whether the non-blocking flock succeeded. A held lock is
// (false, nil); anything else unexpected is an error.
func tryLock(f *os.File) (bool, error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		default:
			return false, err
		}
	}
}

func releaser(f *os.File) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			// Closing the descriptor drops the lock too; the explicit unlock
			// just makes the hand-over independent of any dup'd descriptor.
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		})
	}
}
