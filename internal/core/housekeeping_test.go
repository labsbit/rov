package core_test

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/labsbit/rov/internal/core"
	"github.com/labsbit/rov/internal/docker"
	"github.com/labsbit/rov/internal/tmux"
)

func TestCollectLogsGuards(t *testing.T) {
	old := time.Now().Add(-8 * 24 * time.Hour)
	touch := func(dir, name string) {
		path := filepath.Join(dir, ".logs", name)
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte("x\n"), 0o644)
		os.Chtimes(path, old, old)
	}
	open := func(dir string) *core.Engine {
		e, err := core.OpenWith(dir, core.Options{Socket: "rov-test-none-" + filepath.Base(dir), Docker: &docker.Client{Bin: "/nonexistent"}})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}

	// No rov.yaml: nothing is touched, however old.
	bare := t.TempDir()
	os.Mkdir(filepath.Join(bare, ".git"), 0o755)
	touch(bare, "stale.log")
	touch(bare, "stale.ansi")
	if removed, err := open(bare).CollectLogs(bg, []tmux.Session{}); err != nil || len(removed) != 0 {
		t.Errorf("without rov.yaml: removed %v, %v", removed, err)
	}

	// With rov.yaml: only rov-written (.ansi) unconfigured names go.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "rov.yaml"), []byte("processes:\n  kept: { cmd: x }\n"), 0o644)
	for _, f := range []string{"stale.log", "stale.ansi", "kept.log", "kept.ansi", "foreign.log"} {
		touch(dir, f)
	}
	removed, err := open(dir).CollectLogs(bg, []tmux.Session{})
	if err != nil || !slices.Equal(removed, []string{"stale"}) {
		t.Errorf("removed %v, %v; want [stale]", removed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".logs", "foreign.log")); err != nil {
		t.Errorf("foreign.log (not rov's) deleted: %v", err)
	}
}
