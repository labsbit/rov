package logs

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/labsbit/rov/internal/pipe"
)

func TestNameOf(t *testing.T) {
	tests := []struct {
		file, name string
		ok         bool
	}{
		{"api.log", "api", true},
		{"api.ansi", "api", true},
		{"api.log.1", "api", true},
		{"api.ansi.1", "api", true},
		{"api.prev.log", "api", true},
		{"api.prev.ansi", "api", true},
		{"worker-go.log", "worker-go", true},
		{".log", "", false},
		{".lock", "", false},
		{"notes.txt", "", false},
		{"api.log.2", "", false},
	}
	for _, tt := range tests {
		name, ok := nameOf(tt.file)
		if name != tt.name || ok != tt.ok {
			t.Errorf("%q: got %q %v, want %q %v", tt.file, name, ok, tt.name, tt.ok)
		}
	}
}

func TestGC(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	old := now.Add(-8 * 24 * time.Hour)
	recent := now.Add(-time.Hour)
	touch := func(name string, mtime time.Time) {
		t.Helper()
		path := filepath.Join(dir, name)
		write(t, path, "x\n")
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	// stale: every file old, not kept → removed
	for _, f := range []string{"stale.log", "stale.ansi", "stale.log.1", "stale.ansi.1", "stale.prev.log", "stale.prev.ansi"} {
		touch(f, old)
	}
	// mixed: one recent file keeps the whole set
	touch("mixed.prev.log", old)
	touch("mixed.log", recent)
	// kept: old but configured
	touch("kept.log", old)
	// gone: only a prev file left, old
	touch("gone.prev.ansi", old)
	// unrelated files and the lock directory are never touched
	touch("notes.txt", old)
	if err := os.Mkdir(filepath.Join(dir, ".lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	touch(".lock/stale", old)
	if err := os.Mkdir(filepath.Join(dir, "dir.log"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(filepath.Join(dir, "dir.log"), old, old)

	removed, err := GC(dir, func(name string) bool { return name == "kept" }, 7*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"gone", "stale"}; !slices.Equal(removed, want) {
		t.Errorf("removed %q, want %q", removed, want)
	}
	entries, _ := os.ReadDir(dir)
	var left []string
	for _, e := range entries {
		left = append(left, e.Name())
	}
	if want := []string{".lock", "dir.log", "kept.log", "mixed.log", "mixed.prev.log", "notes.txt"}; !slices.Equal(left, want) {
		t.Errorf("left %q, want %q", left, want)
	}
	if _, err := os.Stat(filepath.Join(dir, ".lock", "stale")); err != nil {
		t.Errorf(".lock touched: %v", err)
	}
}

func TestGCMissingDir(t *testing.T) {
	removed, err := GC(filepath.Join(t.TempDir(), "none"), func(string) bool { return false }, time.Hour, time.Now())
	if err != nil || removed != nil {
		t.Errorf("got %q, %v", removed, err)
	}
}

func TestPurge(t *testing.T) {
	dir := t.TempDir()
	p := pipe.Paths(dir, "adhoc")
	for _, f := range []string{p.Log, p.ANSI, p.PrevLog} {
		write(t, f, "x\n")
	}
	write(t, pipe.Paths(dir, "other").Log, "keep\n")
	if err := Purge(dir, "adhoc"); err != nil {
		t.Fatal(err)
	}
	for _, f := range p.All() {
		if _, err := os.Lstat(f); err == nil {
			t.Errorf("%s still exists", f)
		}
	}
	if _, err := os.Stat(pipe.Paths(dir, "other").Log); err != nil {
		t.Error("purged another name")
	}
	if err := Purge(dir, "../x"); err == nil {
		t.Error("invalid name accepted")
	}
}
