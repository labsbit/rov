package pipe

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/labsbit/rov/internal/lock"
)

// FilesSet holds the paths of every log file of one process.
type FilesSet struct {
	Log      string // <name>.log, current run, plain
	ANSI     string // <name>.ansi, current run, SGR kept
	Log1     string // <name>.log.1, rotated part of the current run
	ANSI1    string // <name>.ansi.1
	PrevLog  string // <name>.prev.log, the run before the last start
	PrevANSI string // <name>.prev.ansi
}

// Paths returns the log file paths of process name in dir. It does not
// validate name; see [CheckName].
func Paths(dir, name string) FilesSet {
	p := func(suffix string) string { return filepath.Join(dir, name+suffix) }
	return FilesSet{
		Log:      p(".log"),
		ANSI:     p(".ansi"),
		Log1:     p(".log.1"),
		ANSI1:    p(".ansi.1"),
		PrevLog:  p(".prev.log"),
		PrevANSI: p(".prev.ansi"),
	}
}

// All returns every path of the set.
func (f FilesSet) All() []string {
	return []string{f.Log, f.ANSI, f.Log1, f.ANSI1, f.PrevLog, f.PrevANSI}
}

// CheckName reports whether name is safe to use as a log file base name:
// non-empty, no path separators or NUL, and not starting with a dot (which
// also keeps it clear of the .lock directory).
func CheckName(name string) error {
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, "/\\\x00") {
		return fmt.Errorf("invalid log name %q", name)
	}
	return nil
}

// rotateLock is the lock file a writer holds while it rotates the files of
// name in dir, and PrepareRun while it moves them: <dir>/.lock/<name>.rotate.
// Both hold it for a few file operations only.
func rotateLock(dir, name string) string {
	return filepath.Join(dir, ".lock", name+".rotate")
}

// rotateLockWait bounds the wait for the rotate lock.
const rotateLockWait = 5 * time.Second

// PrepareRun readies the log files of name for a new run. Core calls it
// before every start, before pipe-pane attaches the new writer:
//
//   - if either current file exists, both <name>.prev.{log,ansi} are replaced
//     by the current files (a missing current file leaves no prev file, so
//     the prev pair never mixes two runs); otherwise prev is left alone;
//   - <name>.{log,ansi}.1 are removed;
//   - empty current files are created, so readers find a log at once.
//
// dir is created if needed. It holds the rotate lock of name meanwhile, so a
// writer still running from the last start can't rotate halfway through.
func PrepareRun(dir, name string) error {
	if err := CheckName(name); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), rotateLockWait)
	release, err := lock.Acquire(ctx, rotateLock(dir, name))
	cancel()
	if err != nil {
		return err
	}
	defer release()
	p := Paths(dir, name)
	if exists(p.Log) || exists(p.ANSI) {
		for _, mv := range [][2]string{{p.Log, p.PrevLog}, {p.ANSI, p.PrevANSI}} {
			if err := removeIfExists(mv[1]); err != nil {
				return err
			}
			if err := os.Rename(mv[0], mv[1]); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	for _, f := range []string{p.Log1, p.ANSI1} {
		if err := removeIfExists(f); err != nil {
			return err
		}
	}
	for _, f := range []string{p.Log, p.ANSI} {
		fh, err := os.OpenFile(f, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return err
		}
		if err := fh.Close(); err != nil {
			return err
		}
	}
	return nil
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
