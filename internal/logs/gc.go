package logs

import (
	"errors"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/labsbit/rov/internal/pipe"
)

// suffixes are the log file suffixes, longest first so "x.prev.log" parses
// as name "x" rather than "x.prev".
var suffixes = []string{".prev.ansi", ".prev.log", ".ansi.1", ".log.1", ".ansi", ".log"}

// nameOf returns the process name of a log file name.
func nameOf(file string) (string, bool) {
	for _, s := range suffixes {
		if name, ok := strings.CutSuffix(file, s); ok && pipe.CheckName(name) == nil {
			return name, true
		}
	}
	return "", false
}

// GC deletes the log files of every name in dir that keep rejects and whose
// newest file was modified more than olderThan before now. It returns the
// names whose files it removed, sorted. Only regular files with a log suffix
// are considered: directories (the .lock directory), symlinks and other files
// are never touched. A missing dir is not an error.
//
// Each file's mtime is checked again right before removal, so a name that
// starts writing meanwhile is left alone.
func GC(dir string, keep func(name string) bool, olderThan time.Duration, now time.Time) (removed []string, err error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	newest := map[string]time.Time{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		name, ok := nameOf(e.Name())
		if !ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // removed meanwhile
		}
		if t, seen := newest[name]; !seen || info.ModTime().After(t) {
			newest[name] = info.ModTime()
		}
	}
	names := make([]string, 0, len(newest))
	for name := range newest {
		names = append(names, name)
	}
	sort.Strings(names)
	cutoff := now.Add(-olderThan)
	var errs []error
	for _, name := range names {
		if !newest[name].Before(cutoff) || keep(name) {
			continue
		}
		files := pipe.Paths(dir, name).All()
		if !allOlder(files, cutoff) {
			continue
		}
		if err := removeAll(files); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, name)
	}
	return removed, errors.Join(errs...)
}

func allOlder(files []string, cutoff time.Time) bool {
	for _, f := range files {
		if fi, err := os.Lstat(f); err == nil && !fi.ModTime().Before(cutoff) {
			return false
		}
	}
	return true
}

// Purge deletes every log file of name in dir (`rov rm --purge`). Missing
// files are fine.
func Purge(dir, name string) error {
	if err := pipe.CheckName(name); err != nil {
		return err
	}
	return removeAll(pipe.Paths(dir, name).All())
}

// removeAll removes the regular files among files.
func removeAll(files []string) error {
	var errs []error
	for _, f := range files {
		if fi, err := os.Lstat(f); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
