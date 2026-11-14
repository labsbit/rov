// Package testenv supports rov's integration tests: a throwaway tmux server
// per test, a temp project, the real rov binary (for `rov __pipe`) and a
// helper program that behaves like the servers rov runs.
//
// The helper is the test binary itself: a test package's TestMain calls
// Main, which runs the helper when the binary is started as
// `<test binary> __helper …`.
package testenv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labsbit/rov/internal/core"
	"github.com/labsbit/rov/internal/docker"
	"github.com/labsbit/rov/internal/shell"
)

// Main runs the helper program when the test binary was started as one,
// else the tests. Call it from TestMain.
func Main(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__helper" {
		os.Exit(helperMain(os.Args[2:]))
	}
	code := m.Run()
	if buildDir != "" {
		os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

var (
	buildOnce sync.Once
	buildDir  string
	rovBin    string
	buildErr  error
)

// RovBin builds cmd/rov once per test binary and returns its path. It is
// the `rov __pipe` the panes pipe into, exactly as in production.
func RovBin(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		out, err := exec.Command("go", "env", "GOMOD").Output()
		if err != nil {
			buildErr = fmt.Errorf("go env GOMOD: %w", err)
			return
		}
		mod := filepath.Dir(strings.TrimSpace(string(out)))
		buildDir, buildErr = os.MkdirTemp("", "rov-testbin-")
		if buildErr != nil {
			return
		}
		rovBin = filepath.Join(buildDir, "rov")
		cmd := exec.Command("go", "build", "-o", rovBin, "./cmd/rov")
		cmd.Dir = mod
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ./cmd/rov: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return rovBin
}

// Helper returns the shell command that runs the helper program with args.
func Helper(args ...string) string {
	self, _ := os.Executable()
	return shell.Join(append([]string{self, "__helper"}, args...))
}

// FreePort returns a TCP port that was free a moment ago.
func FreePort(t testing.TB) int {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// PortFree reports whether nobody listens on 127.0.0.1:port.
func PortFree(port int) bool {
	ln, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// Project is a temp project with its own rov.yaml and tmux server.
type Project struct {
	Dir    string
	Engine *core.Engine
	Socket string
}

// NewProject writes yaml as rov.yaml into a temp dir and opens an engine on
// a throwaway tmux server (rov-test-<rand>) with docker switched off. The
// server is killed when the test ends, after every process tree is stopped.
func NewProject(t *testing.T, yaml string) *Project {
	t.Helper()
	return newProject(t, "", yaml, false)
}

// NewDockerProject is NewProject with the real docker client.
func NewDockerProject(t *testing.T, yaml string) *Project {
	t.Helper()
	return newProject(t, "", yaml, true)
}

// NewProjectNamed is NewProject with the project in a directory called base
// (inside a temp dir), for paths with odd characters.
func NewProjectNamed(t *testing.T, base, yaml string) *Project {
	t.Helper()
	return newProject(t, base, yaml, false)
}

// NewProjectOwnSocket is NewProject on the project's own socket name (as the
// real rov computes it from the root, unique per temp dir), so the rov
// binary run inside a pane (RovBin) talks to the same server.
func NewProjectOwnSocket(t *testing.T, yaml string) *Project {
	t.Helper()
	return newProjectWith(t, yaml, projectOpts{ownSocket: true})
}

type projectOpts struct {
	base       string // directory name of the root ("" a temp dir itself)
	realDocker bool
	ownSocket  bool
}

func newProject(t *testing.T, base, yaml string, realDocker bool) *Project {
	t.Helper()
	return newProjectWith(t, yaml, projectOpts{base: base, realDocker: realDocker})
}

func newProjectWith(t *testing.T, yaml string, o projectOpts) *Project {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	dir := t.TempDir()
	if o.base != "" {
		dir = filepath.Join(dir, o.base)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "rov.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	rand.Read(b)
	socket := "rov-test-" + hex.EncodeToString(b)
	if o.ownSocket {
		socket = ""
	}
	opts := core.Options{Socket: socket, RovBin: RovBin(t), Docker: &docker.Client{Bin: filepath.Join(dir, "no-docker")}}
	if o.realDocker {
		opts.Docker = docker.Default()
	}
	e, err := core.OpenWith(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	e.LockTimeout = 20 * time.Second
	p := &Project{Dir: dir, Engine: e, Socket: e.Socket()}
	t.Cleanup(func() { p.Teardown() })
	return p
}

// Teardown stops every process tree (setsid children included) and kills
// the server.
func (p *Project) Teardown() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if st, err := p.Engine.Status(ctx); err == nil {
		for _, proc := range st.Processes {
			if !proc.HasSession {
				continue
			}
			if proc.Kind == core.KindAdhoc {
				_, _ = p.Engine.Rm(ctx, proc.Name, false)
			} else {
				_, _ = p.Engine.Down(ctx, proc.Name)
			}
		}
	}
	_ = p.Engine.Tmux.KillServer(ctx)
	_ = os.Remove(p.Engine.Tmux.SocketPath()) // tmux leaves the socket file
}

// WriteConfig replaces rov.yaml.
func (p *Project) WriteConfig(t *testing.T, yaml string) {
	t.Helper()
	path := filepath.Join(p.Dir, "rov.yaml")
	// Make sure the change is visible even within one mtime tick.
	old, _ := os.Stat(path)
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	if old != nil {
		_ = os.Chtimes(path, time.Now(), old.ModTime().Add(time.Second))
	}
}

// Status returns the current status or fails the test.
func (p *Project) Status(t *testing.T) *core.Status {
	t.Helper()
	st, err := p.Engine.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// Proc returns the named process from a fresh status or fails the test.
func (p *Project) Proc(t *testing.T, name string) *core.Process {
	t.Helper()
	pr := p.Status(t).Get(name)
	if pr == nil {
		t.Fatalf("no process %s", name)
	}
	return pr
}

// Eventually polls cond every 20 ms until it is true or timeout passes.
func Eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Log returns the plain log of name (current run), or "".
func (p *Project) Log(name string) string {
	b, _ := os.ReadFile(p.Engine.LogPaths(name).Log)
	return string(b)
}
