package tui

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/labsbit/rov/internal/config"
	"github.com/labsbit/rov/internal/configedit"
	"github.com/labsbit/rov/internal/core"
	"github.com/labsbit/rov/internal/docker"
	"github.com/labsbit/rov/internal/ports"
	"github.com/labsbit/rov/internal/testenv"
	"github.com/labsbit/rov/internal/tmux"
)

// sampleYAML is the config of testenv.SampleInputs as a file, with
// comments, so edits through configedit can be checked byte for byte.
const sampleYAML = `# rov.yaml — sample project
processes:
  # docker infra
  infra:
    cmd: just infra
    description: Redis + ClickHouse (docker)
    docker: search-api-gateway-local
  gateway:
    cmd: just gateway
    description: search gateway
  tracking:    { cmd: just tracking }
  worker-rust: { cmd: just worker-rust }
  api:
    cmd: just api
    env:
      PORT: "3001"
  panel:       { cmd: just panel }
`

// fakeBackend serves canned tmux sessions, ports and containers (those of
// testenv.SampleInputs) over a real rov.yaml in a temp dir; actions are
// recorded, config writes go through the real configedit.
type fakeBackend struct {
	t    testing.TB
	root string
	home string

	mu         sync.Mutex
	cfg        *config.Config
	cfgErr     error
	cfgData    []byte
	sessions   []tmux.Session
	snap       *ports.Snapshot
	containers []docker.Container
	dockerErr  error
	actions    []string
	dockerLogs map[string]string
	// dockerMore is what a container's log stream gives when picked up
	// again (since set).
	dockerMore map[string]string
	logCalls   []logCall
	// gate, when set, holds Up until it is closed.
	gate chan struct{}
}

func newFake(t testing.TB) *fakeBackend {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, "myapp")
	if err := os.MkdirAll(filepath.Join(root, ".logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	in := testenv.SampleInputs(t)
	f := &fakeBackend{t: t, root: root, home: home, sessions: in.Sessions, snap: in.Ports, containers: in.Containers, dockerLogs: map[string]string{}, dockerMore: map[string]string{}}
	f.writeConfig(sampleYAML)
	return f
}

func (f *fakeBackend) writeConfig(yaml string) {
	f.t.Helper()
	if err := os.WriteFile(f.ConfigPath(), []byte(yaml), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.Reload()
}

// writeLog writes the .ansi (and .log) file of a process.
func (f *fakeBackend) writeLog(name string, lines []string) {
	f.t.Helper()
	data := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(f.LogDir(), name+".ansi"), []byte(data), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeBackend) appendLog(name string, lines []string) {
	f.t.Helper()
	fh, err := os.OpenFile(filepath.Join(f.LogDir(), name+".ansi"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		f.t.Fatal(err)
	}
	defer fh.Close()
	if _, err := fh.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeBackend) setSessions(ss []tmux.Session) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = ss
}

func (f *fakeBackend) actionLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.actions...)
}

func (f *fakeBackend) record(a string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.actions = append(f.actions, a)
}

func (f *fakeBackend) Root() string       { return f.root }
func (f *fakeBackend) Name() string       { return "myapp" }
func (f *fakeBackend) Socket() string     { return "rov-myapp-3f9a1c2e" }
func (f *fakeBackend) ConfigPath() string { return filepath.Join(f.root, "rov.yaml") }
func (f *fakeBackend) HasConfig() bool    { return true }
func (f *fakeBackend) LockDir() string    { return filepath.Join(f.root, ".logs", ".lock") }
func (f *fakeBackend) LogDir() string     { return filepath.Join(f.root, ".logs") }

func (f *fakeBackend) Config() *config.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

func (f *fakeBackend) ConfigErr() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfgErr
}

func (f *fakeBackend) Inputs(sessions []tmux.Session, snap *ports.Snapshot, containers []docker.Container, dockerErr error) core.Inputs {
	return core.Inputs{
		Root: f.root, Project: f.Name(), Socket: f.Socket(),
		Config: f.Config(), ConfigErr: f.ConfigErr(), LogDir: f.LogDir(),
		Sessions: sessions, Ports: snap, Containers: containers, DockerErr: dockerErr,
		Now: testenv.SampleTime,
	}
}

func (f *fakeBackend) Reload() (bool, error) {
	data, err := os.ReadFile(f.ConfigPath())
	f.mu.Lock()
	defer f.mu.Unlock()
	if err == nil && f.cfg != nil && bytes.Equal(data, f.cfgData) {
		return false, f.cfgErr
	}
	f.cfgData = data
	cfg, cerr := config.Parse(f.ConfigPath(), data)
	if cfg == nil {
		if f.cfg == nil {
			f.cfg = config.Default(f.ConfigPath())
		}
	} else {
		f.cfg = cfg
	}
	f.cfgErr = cerr
	return true, cerr
}

func (f *fakeBackend) Sessions(context.Context) ([]tmux.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]tmux.Session(nil), f.sessions...), nil
}

func (f *fakeBackend) ScanPorts() (*ports.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap, nil
}

func (f *fakeBackend) Containers(context.Context) ([]docker.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.containers, f.dockerErr
}

func (f *fakeBackend) RecordPorts(context.Context, *core.Status) error { return nil }

func (f *fakeBackend) Up(_ context.Context, name string) (string, error) {
	if f.gate != nil {
		<-f.gate
	}
	f.record("up " + name)
	return "started", nil
}

func (f *fakeBackend) Restart(_ context.Context, name string) (string, error) {
	f.record("restart " + name)
	return "restarted", nil
}

func (f *fakeBackend) Stop(_ context.Context, name string) (string, error) {
	f.record("stop " + name)
	return "stopped", nil
}

func (f *fakeBackend) Rm(_ context.Context, name string, _ bool) (string, error) {
	f.record("rm " + name)
	return "removed", nil
}

func (f *fakeBackend) Spawn(_ context.Context, o core.SpawnOptions) (string, error) {
	f.record("spawn " + o.Name + " " + o.Cmd)
	return "started", nil
}

func (f *fakeBackend) Redefine(_ context.Context, o core.SpawnOptions) (string, error) {
	f.record("redefine " + o.Name + " " + o.Cmd)
	return "definition saved", nil
}

func (f *fakeBackend) DropSession(_ context.Context, name string) error {
	f.record("drop " + name)
	return nil
}

func (f *fakeBackend) ContainerStart(_ context.Context, target string) (string, error) {
	f.record("docker start " + target)
	return "started", nil
}

func (f *fakeBackend) ContainerStop(_ context.Context, target string) (string, error) {
	f.record("docker stop " + target)
	return "stopped", nil
}

func (f *fakeBackend) WriteConfig(ctx context.Context, baseline []byte, op configedit.Op) ([]byte, error) {
	out, err := configedit.WriteContext(ctx, f.ConfigPath(), f.LockDir(), baseline, op)
	_, _ = f.Reload()
	return out, err
}

func (f *fakeBackend) AttachCommand(name string) *exec.Cmd { return exec.Command("true") }

func (f *fakeBackend) ContainerLogs(_ context.Context, container string, _ int, since time.Time, _ bool) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logCalls = append(f.logCalls, logCall{container, since})
	if !since.IsZero() {
		// picked up again: what came since, once
		more := f.dockerMore[container]
		delete(f.dockerMore, container)
		return io.NopCloser(strings.NewReader(more)), nil
	}
	s, ok := f.dockerLogs[container]
	if !ok {
		return nil, errors.New("no such container: " + container)
	}
	return io.NopCloser(strings.NewReader(s)), nil
}

// logCall is one ContainerLogs call.
type logCall struct {
	container string
	since     time.Time
}

func (f *fakeBackend) containerLogCalls() []logCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]logCall(nil), f.logCalls...)
}

func (f *fakeBackend) setContainers(cs []docker.Container, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers, f.dockerErr = cs, err
}

// waitFile polls path until cond holds.
func waitFile(t testing.TB, path string, cond func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(path)
		if cond(string(b)) {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never matched; it is:\n%s", path, b)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
