package configedit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labsbit/rov/internal/config"
	"github.com/labsbit/rov/internal/lock"
)

// project makes a temp project root holding rov.yaml with content (none if
// nil) and returns the config path and lock dir.
func project(t *testing.T, content []byte) (path, lockDir string) {
	t.Helper()
	root := t.TempDir()
	path = filepath.Join(root, config.FileName)
	if content != nil {
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return path, filepath.Join(root, ".logs", ".lock")
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestWriteCreatesFile(t *testing.T) {
	path, lockDir := project(t, nil)
	out, err := Write(path, lockDir, nil, Add{Proc: flowProc("web", "npm run dev")})
	if err != nil {
		t.Fatal(err)
	}
	want := "processes:\n  web: { cmd: npm run dev }\n"
	if string(out) != want || string(readFile(t, path)) != want {
		t.Errorf("wrote %q, returned %q; want %q", readFile(t, path), out, want)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v", fi.Mode())
	}
	if _, err := os.Stat(filepath.Join(lockDir, "config")); err != nil {
		t.Errorf("lock file: %v", err)
	}
}

func TestWriteKeepsModeAndLeavesNoTemp(t *testing.T) {
	spec := fixture(t, "spec")
	path, lockDir := project(t, spec)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(path, lockDir, spec, Remove{Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	if strings.Contains(string(readFile(t, path)), "web:") {
		t.Error("web was not removed")
	}
}

func TestWriteThroughSymlink(t *testing.T) {
	spec := fixture(t, "spec")
	path, lockDir := project(t, nil)
	real := filepath.Join(filepath.Dir(path), "real.yaml")
	if err := os.WriteFile(real, spec, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real.yaml", path); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(path, lockDir, nil, Remove{Name: "web"}); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("rov.yaml is no longer a symlink: %v", err)
	}
	if strings.Contains(string(readFile(t, real)), "web:") {
		t.Error("the symlink target was not edited")
	}
}

func TestWriteNoOpDoesNotWrite(t *testing.T) {
	spec := fixture(t, "spec")
	path, lockDir := project(t, spec)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	out, err := Write(path, lockDir, spec, Move{Name: "infra", Pos: Position{First: true}})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, spec) {
		t.Error("no-op changed the content")
	}
	if fi, _ := os.Stat(path); !fi.ModTime().Equal(old) {
		t.Error("no-op rewrote the file")
	}
}

func TestWriteValidates(t *testing.T) {
	spec := fixture(t, "spec")
	path, lockDir := project(t, spec)
	if err := os.MkdirAll(filepath.Join(filepath.Dir(path), "tools", "mock-s3"), 0o755); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		op   Op
		want string
	}{
		{"empty cmd", Set{Name: "api", Patch: Patch{Cmd: new("")}}, "cmd is required"},
		{"missing cwd on add", Add{Proc: config.Process{Name: "x", Cmd: "y", Cwd: "nope"}}, `process "x": cwd "nope" does not exist`},
		{"missing cwd on set", Set{Name: "api", Patch: Patch{Cwd: new("nope/deeper")}}, `process "api": cwd "nope/deeper" does not exist`},
	}
	for _, tt := range tests {
		_, err := Write(path, lockDir, spec, tt.op)
		if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), path) {
			t.Errorf("%s: got %v, want an error naming %s and containing %q", tt.name, err, path, tt.want)
		}
		if !bytes.Equal(readFile(t, path), spec) {
			t.Fatalf("%s: the file changed after a failed write", tt.name)
		}
	}
	if _, err := Write(path, lockDir, spec, Add{Proc: mockS3}); err != nil {
		t.Errorf("existing cwd: %v", err)
	}
}

// edit returns spec with one line replaced, as another writer would.
func edit(t *testing.T, src []byte, old, new string) []byte {
	t.Helper()
	if !bytes.Contains(src, []byte(old)) {
		t.Fatalf("fixture has no %q", old)
	}
	return bytes.Replace(src, []byte(old), []byte(new), 1)
}

func TestWriteReappliesAfterUnrelatedChange(t *testing.T) {
	baseline := fixture(t, "spec")
	tests := []struct {
		name  string
		disk  []byte
		op    Op
		check []string // all must be in the result
	}{
		{"set beside an edit", edit(t, baseline, "just tracking", "just tracking-v2"),
			Set{Name: "gateway", Patch: Patch{Cmd: new("just gateway-dev")}}, []string{"just tracking-v2", "cmd: just gateway-dev"}},
		{"add beside an add", edit(t, baseline, "  web:         { cmd: just web }\n", "  web:         { cmd: just web }\n  docs:        { cmd: just docs }\n"),
			Add{Proc: flowProc("mock", "node mock.js"), Pos: Position{After: "api"}}, []string{"docs:", "mock:"}},
		{"rm beside a comment change", edit(t, baseline, "# rov.yaml", "# edited: rov.yaml"),
			Remove{Name: "panel"}, []string{"# edited: rov.yaml"}},
		{"move beside a reorder", edit(t, baseline, "  panel:       { cmd: just panel }\n  api:         { cmd: just api }\n", "  api:         { cmd: just api }\n  panel:       { cmd: just panel }\n"),
			Move{Name: "fetch-py", Pos: Position{After: "api"}}, []string{"  api:         { cmd: just api }\n  fetch-py:"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, lockDir := project(t, tt.disk)
			out, err := Write(path, lockDir, baseline, tt.op)
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range tt.check {
				if !strings.Contains(string(out), c) {
					t.Errorf("result lacks %q:\n%s", c, out)
				}
			}
			if !bytes.Equal(out, readFile(t, path)) {
				t.Error("returned content differs from the file")
			}
		})
	}
}

func TestWriteConflict(t *testing.T) {
	baseline := fixture(t, "spec")
	tests := []struct {
		name   string
		disk   []byte
		op     Op
		change string
	}{
		{"set on edited entry", edit(t, baseline, "just gateway\n", "just gateway-other\n"),
			Set{Name: "gateway", Patch: Patch{Description: new("x")}}, `process "gateway" was edited`},
		{"set on removed entry", edit(t, baseline, "  api:         { cmd: just api }\n", ""),
			Set{Name: "api", Patch: Patch{Cmd: new("x")}}, `process "api" was removed`},
		{"add of a name added on disk", edit(t, baseline, "processes:\n", "processes:\n  mock: { cmd: other }\n"),
			Add{Proc: flowProc("mock", "node mock.js")}, `process "mock" was added`},
		{"rename onto a name added on disk", edit(t, baseline, "processes:\n", "processes:\n  mock: { cmd: other }\n"),
			Set{Name: "api", Patch: Patch{Rename: new("mock")}}, `process "mock" was added`},
		{"rm of edited entry", edit(t, baseline, "just web", "just web --open"),
			Remove{Name: "web"}, `process "web" was edited`},
		{"move after a removed entry", edit(t, baseline, "  api:         { cmd: just api }\n", ""),
			Move{Name: "web", Pos: Position{After: "api"}}, `process "api" was removed`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, lockDir := project(t, tt.disk)
			_, err := Write(path, lockDir, baseline, tt.op)
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("got %v, want ErrConflict", err)
			}
			var cerr *ConflictError
			if !errors.As(err, &cerr) || !bytes.Equal(cerr.Current, tt.disk) {
				t.Errorf("ConflictError does not carry the file on disk: %#v", err)
			}
			if !strings.Contains(err.Error(), tt.change) || !strings.Contains(err.Error(), path) {
				t.Errorf("error %q lacks %q or the path", err, tt.change)
			}
			if !bytes.Equal(readFile(t, path), tt.disk) {
				t.Error("the file changed despite the conflict")
			}
		})
	}
}

func TestWriteMoveAfterEditedRefIsFine(t *testing.T) {
	baseline := fixture(t, "spec")
	disk := edit(t, baseline, "just api", "just api --watch")
	path, lockDir := project(t, disk)
	if _, err := Write(path, lockDir, baseline, Move{Name: "web", Pos: Position{After: "gateway"}}); err != nil {
		t.Fatalf("an edit of the position reference is not a conflict: %v", err)
	}
}

func TestWriteWaitsForLock(t *testing.T) {
	spec := fixture(t, "spec")
	path, lockDir := project(t, spec)
	release, err := lock.Acquire(context.Background(), filepath.Join(lockDir, "config"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := WriteContext(ctx, path, lockDir, spec, Remove{Name: "web"}); !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), path) {
		t.Errorf("got %v, want a timeout naming %s", err, path)
	}
	release()
	if _, err := Write(path, lockDir, spec, Remove{Name: "web"}); err != nil {
		t.Errorf("after release: %v", err)
	}
}

// TestConcurrentWriters: writers that each add their own entry, all from the
// same baseline, serialise on the lock and re-apply on top of each other.
func TestConcurrentWriters(t *testing.T) {
	spec := fixture(t, "spec")
	path, lockDir := project(t, spec)
	names := []string{"w1", "w2", "w3", "w4", "w5", "w6"}
	var wg sync.WaitGroup
	for _, n := range names {
		wg.Go(func() {
			if _, err := Write(path, lockDir, spec, Add{Proc: flowProc(n, "run "+n), Pos: Position{After: "api"}}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if _, ok := cfg.Get(n); !ok {
			t.Errorf("%s is missing:\n%s", n, readFile(t, path))
		}
	}
	if len(cfg.Processes) != 9+len(names) {
		t.Errorf("got %d processes", len(cfg.Processes))
	}
}
