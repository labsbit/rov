package tui

import (
	"context"
	"io"
	"os/exec"
	"time"

	"github.com/labsbit/rov/internal/config"
	"github.com/labsbit/rov/internal/configedit"
	"github.com/labsbit/rov/internal/core"
	"github.com/labsbit/rov/internal/docker"
	"github.com/labsbit/rov/internal/ports"
	"github.com/labsbit/rov/internal/tmux"
)

// Backend is everything the TUI reads and does. *core.Engine provides it
// (see engineBackend); tests use a fake with canned sessions, ports and
// containers over a real rov.yaml.
//
// Every method may block on I/O and is called from a tea.Cmd, never from
// Update, except the cheap accessors marked "no I/O".
type Backend interface {
	// No I/O.
	Root() string
	Name() string
	Socket() string
	ConfigPath() string
	HasConfig() bool
	LockDir() string
	LogDir() string
	Config() *config.Config
	ConfigErr() error
	// Inputs fills Compose inputs with the current config and clock (no I/O).
	Inputs(sessions []tmux.Session, snap *ports.Snapshot, containers []docker.Container, dockerErr error) core.Inputs

	// Sources.
	Reload() (bool, error)
	Sessions(ctx context.Context) ([]tmux.Session, error)
	ScanPorts() (*ports.Snapshot, error)
	Containers(ctx context.Context) ([]docker.Container, error)
	RecordPorts(ctx context.Context, st *core.Status) error

	// Actions; each returns a short message for "<name>: <msg>".
	Up(ctx context.Context, name string) (string, error)
	Restart(ctx context.Context, name string) (string, error)
	Stop(ctx context.Context, name string) (string, error)
	Rm(ctx context.Context, name string, purge bool) (string, error)
	Spawn(ctx context.Context, o core.SpawnOptions) (string, error)
	Redefine(ctx context.Context, o core.SpawnOptions) (string, error)
	DropSession(ctx context.Context, name string) error
	ContainerStart(ctx context.Context, target string) (string, error)
	ContainerStop(ctx context.Context, target string) (string, error)
	// WriteConfig applies op to rov.yaml (configedit.WriteContext) and
	// reloads the config.
	WriteConfig(ctx context.Context, baseline []byte, op configedit.Op) ([]byte, error)

	// AttachCommand is run with tea.ExecProcess.
	AttachCommand(name string) *exec.Cmd
	// ContainerLogs streams `docker logs` (since: from then on; zero: the
	// last tail lines).
	ContainerLogs(ctx context.Context, container string, tail int, since time.Time, follow bool) (io.ReadCloser, error)
}

// engineBackend adapts *core.Engine.
type engineBackend struct{ *core.Engine }

// NewBackend returns the Backend of a real project.
func NewBackend(e *core.Engine) Backend { return engineBackend{e} }

func (b engineBackend) WriteConfig(ctx context.Context, baseline []byte, op configedit.Op) ([]byte, error) {
	out, err := configedit.WriteContext(ctx, b.ConfigPath(), b.LockDir(), baseline, op)
	_, _ = b.Reload()
	return out, err
}

func (b engineBackend) ContainerLogs(ctx context.Context, container string, tail int, since time.Time, follow bool) (io.ReadCloser, error) {
	return b.Docker.LogsSince(ctx, container, tail, since, follow)
}
