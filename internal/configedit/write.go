package configedit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/labsbit/rov/internal/config"
	"github.com/labsbit/rov/internal/lock"
)

// ErrConflict is matched (errors.Is) by the *ConflictError Write returns
// when rov.yaml changed on disk in the entry the op touches.
var ErrConflict = errors.New("rov.yaml changed on disk")

// ConflictError reports that the entry an op works on changed between the
// caller's read of rov.yaml and the write.
type ConflictError struct {
	Path string
	// Name is the process whose entry changed.
	Name string
	// Change is what happened to it: "was added", "was removed" or "was edited".
	Change string
	// Current is the file as it is now on disk, to reload from.
	Current []byte
}

// Error says which entry changed and what to do about it.
func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s changed on disk: process %q %s since it was read; reload and try again", e.Path, e.Name, e.Change)
}

// Is makes errors.Is(err, ErrConflict) true.
func (e *ConflictError) Is(target error) bool { return target == ErrConflict }

// LockTimeout bounds how long Write waits for another rov writing rov.yaml.
const LockTimeout = 10 * time.Second

// Write applies op to the rov.yaml at path under the config lock
// (<lockDir>/config) and returns the content written. See WriteContext.
func Write(path, lockDir string, baseline []byte, op Op) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), LockTimeout)
	defer cancel()
	return WriteContext(ctx, path, lockDir, baseline, op)
}

// WriteContext applies op to the rov.yaml at path and writes it atomically.
// ctx bounds the wait for the config lock at <lockDir>/config.
//
// baseline is the content the caller last read and based the op on, or nil
// to apply the op to whatever is on disk. (To say "there was no file", pass
// an empty, non-nil slice.) If the file changed since, the op is re-applied
// to the fresh content, unless the entry the op touches changed too: then it
// returns a *ConflictError (errors.Is ErrConflict) holding the fresh content.
//
// A cwd the op sets must name an existing directory under the root (the
// directory of path). When rov.yaml doesn't exist, Add creates it. The write
// is a temp file in the same directory, fsynced and renamed over the old one,
// which keeps its permissions; a symlinked rov.yaml is written through.
func WriteContext(ctx context.Context, path, lockDir string, baseline []byte, op Op) ([]byte, error) {
	release, err := lock.Acquire(ctx, filepath.Join(lockDir, "config"))
	if err != nil {
		return nil, fmt.Errorf("can't edit %s: %w", path, err)
	}
	defer release()

	target := path
	if t, err := filepath.EvalSymlinks(path); err == nil {
		target = t
	}
	fresh, err := os.ReadFile(target)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if baseline != nil && !bytes.Equal(baseline, fresh) {
		if err := conflict(path, baseline, fresh, op); err != nil {
			return nil, err
		}
	}
	if err := checkCwd(filepath.Dir(path), op); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out, err := apply(path, fresh, op)
	if err != nil {
		return nil, err
	}
	if fresh != nil && bytes.Equal(out, fresh) {
		return out, nil
	}
	if err := writeAtomic(target, out); err != nil {
		return nil, err
	}
	return out, nil
}

// conflict returns a *ConflictError if an entry op edits differs between
// baseline and fresh, or an entry its position refers to is gone.
func conflict(path string, baseline, fresh []byte, op Op) error {
	was, _ := config.Parse(path, baseline)
	now, _ := config.Parse(path, fresh)
	edits, refs := op.touches()
	for _, name := range append(edits, refs...) {
		if was == nil || now == nil {
			return &ConflictError{Path: path, Name: name, Change: "was edited", Current: fresh}
		}
		a, inWas := was.Get(name)
		b, inNow := now.Get(name)
		change := ""
		switch {
		case inWas && !inNow:
			change = "was removed"
		case !slices.Contains(edits, name):
			// A position reference only has to still be there.
		case !inWas && inNow:
			change = "was added"
		case inWas && !sameProcesses([]config.Process{a}, []config.Process{b}):
			change = "was edited"
		}
		if change != "" {
			return &ConflictError{Path: path, Name: name, Change: change, Current: fresh}
		}
	}
	return nil
}

func checkCwd(root string, op Op) error {
	var name, cwd string
	switch op := op.(type) {
	case Add:
		name, cwd = op.Proc.Name, op.Proc.Cwd
	case Set:
		if op.Patch.Cwd != nil {
			name, cwd = op.Name, *op.Patch.Cwd
		}
	}
	if cwd == "" {
		return nil
	}
	if err := config.CheckCwd(root, cwd); err != nil {
		return fmt.Errorf("process %q: %w", name, err)
	}
	return nil
}

func writeAtomic(path string, data []byte) (err error) {
	mode := fs.FileMode(0o644)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if err = tmp.Chmod(mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	// Make the rename itself durable; failing here loses nothing already
	// written, so it isn't reported.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
