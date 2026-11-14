package core

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/labsbit/rov/internal/config"
	"github.com/labsbit/rov/internal/docker"
	"github.com/labsbit/rov/internal/tmux"
)

// Defaults for Engine settings.
const (
	DefaultLockTimeout = 30 * time.Second
	DefaultStopTimeout = 5 * time.Second
	// Width and height of a new session: detached sessions default to 80x24
	// and would hard-wrap output; tmux shrinks the window when a client
	// attaches.
	SessionWidth  = 220
	SessionHeight = 50
)

// Engine runs one project: its config, its tmux server and its docker
// containers. The CLI and the TUI share it, so an action from either goes
// through the same code and the same locks. An Engine is safe for
// concurrent use; actions on different processes run in parallel, actions
// on the same process are serialised (also across rov processes) by a flock.
type Engine struct {
	// Tmux talks to the project's own server (tmux -L <Socket>).
	Tmux *tmux.Client
	// Docker discovers and acts on the project's compose containers.
	Docker *docker.Client
	// RovBin is the absolute path of the rov binary pipe-pane runs as
	// `rov __pipe` (the running executable by default).
	RovBin string
	// LockTimeout bounds the wait for another rov acting on the same process.
	LockTimeout time.Duration
	// StopTimeout is how long a stop waits after SIGTERM before SIGKILL.
	StopTimeout time.Duration
	// Now is the clock; time.Now by default.
	Now func() time.Time

	// kill sends signals for stopTree; syscall.Kill if nil (tests replace it).
	kill func(pid int, sig syscall.Signal) error

	root      string
	name      string
	hasConfig atomic.Bool
	cfg       atomic.Pointer[configState]
	reloadMu  sync.Mutex
}

// configState is one load of rov.yaml.
type configState struct {
	cfg *config.Config // never nil
	err error          // problems (a *config.ValidationError) or a read error
	// file identity at load time, to skip reloading an unchanged file
	exists  bool
	modTime time.Time
	size    int64
}

// Options configures OpenWith. Zero fields take defaults.
type Options struct {
	// Socket overrides the tmux socket name (default: the project's
	// SocketName, "rov-<base>-<hash8>"). Tests use a throwaway one.
	Socket string
	// RovBin overrides the rov binary used for __pipe.
	RovBin string
	// Docker overrides the docker client.
	Docker *docker.Client
}

// Open finds the project dir belongs to (see config.FindRoot) and returns
// its Engine. A rov.yaml with problems doesn't fail Open: the engine keeps
// what could be read and reports the problems through ConfigErr and the
// status warnings, so `rov status` still works while the file is broken.
func Open(dir string) (*Engine, error) { return OpenWith(dir, Options{}) }

// OpenWith is Open with options.
func OpenWith(dir string, o Options) (*Engine, error) {
	root, _, err := config.FindRoot(dir)
	if err != nil {
		return nil, err
	}
	e := &Engine{
		Docker:      o.Docker,
		RovBin:      o.RovBin,
		LockTimeout: DefaultLockTimeout,
		StopTimeout: DefaultStopTimeout,
		Now:         time.Now,
		root:        root,
		name:        filepath.Base(root),
	}
	if e.Docker == nil {
		e.Docker = docker.Default()
	}
	if e.RovBin == "" {
		e.RovBin = selfPath()
	}
	// Config problems don't fail Open; they stay available as ConfigErr.
	_, _ = e.Reload()
	socket := o.Socket
	if socket == "" {
		socket = e.Project().SocketName()
	}
	e.Tmux = tmux.New(socket)
	return e, nil
}

// selfPath returns the absolute, symlink-free path of the running binary.
func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return "rov"
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	return p
}

// Root is the absolute project root.
func (e *Engine) Root() string { return e.root }

// Name is the project's name (the root's base name).
func (e *Engine) Name() string { return e.name }

// Socket is the tmux socket name of the project's server.
func (e *Engine) Socket() string { return e.Tmux.Socket }

// ConfigPath is the absolute path of rov.yaml (whether or not it exists).
func (e *Engine) ConfigPath() string { return filepath.Join(e.root, config.FileName) }

// HasConfig reports whether rov.yaml exists.
func (e *Engine) HasConfig() bool { return e.hasConfig.Load() }

// Config returns the current config. It is never nil and must not be
// modified; Reload replaces it.
func (e *Engine) Config() *config.Config { return e.cfg.Load().cfg }

// ConfigErr returns the problems of the current rov.yaml, nil if none. It is
// a *config.ValidationError when the file could be read.
func (e *Engine) ConfigErr() error { return e.cfg.Load().err }

// Project returns the project with the current config.
func (e *Engine) Project() *config.Project {
	return &config.Project{Root: e.root, Name: e.name, Config: e.Config(), HasConfig: e.HasConfig()}
}

// LogDir is the absolute log directory.
func (e *Engine) LogDir() string { return e.Project().LogDir() }

// LockDir is the absolute directory of the locks.
func (e *Engine) LockDir() string { return e.Project().LockDir() }

// Reload re-reads rov.yaml if it changed on disk (by size, mtime or
// existence) and reports whether it did. The TUI calls it on a file-watch
// event; every action calls it first, so an edit made a moment ago applies.
// Problems in the file are returned (and kept for ConfigErr) but the config
// is still replaced with what could be read; invalid YAML keeps the
// previous config.
func (e *Engine) Reload() (changed bool, err error) {
	e.reloadMu.Lock()
	defer e.reloadMu.Unlock()
	path := e.ConfigPath()
	fi, statErr := os.Stat(path)
	exists := statErr == nil && fi.Mode().IsRegular()
	old := e.cfg.Load()
	if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
		if old == nil {
			e.cfg.Store(&configState{cfg: config.Default(path), err: statErr})
		}
		return false, statErr
	}
	if old != nil && old.exists == exists && (!exists || old.modTime.Equal(fi.ModTime()) && old.size == fi.Size()) {
		return false, old.err
	}
	st := &configState{exists: exists}
	if exists {
		st.modTime, st.size = fi.ModTime(), fi.Size()
		cfg, err := config.Load(path)
		switch {
		case cfg != nil:
			st.cfg, st.err = cfg, err
		case old != nil && old.exists:
			// Unparsable: keep running on the last good config.
			st.cfg, st.err = old.cfg, err
		default:
			st.cfg, st.err = config.Default(path), err
		}
	} else {
		st.cfg = config.Default(path)
	}
	e.hasConfig.Store(exists)
	e.cfg.Store(st)
	return true, st.err
}

// problemsOf returns the rov.yaml problems of process name.
func (e *Engine) problemsOf(name string) []string {
	return problemsFor(e.ConfigErr(), name)
}

func problemsFor(err error, name string) []string {
	var verr *config.ValidationError
	if !errors.As(err, &verr) {
		return nil
	}
	var out []string
	for _, p := range verr.Problems {
		if p.Process == name {
			out = append(out, p.Msg)
		}
	}
	return out
}

// configWarnings turns config problems into warning lines.
func configWarnings(err error) []string {
	if err == nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(err.Error(), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// relToRoot returns path relative to the root when inside it, else path.
func (e *Engine) relToRoot(path string) string { return relTo(e.root, path) }

func relTo(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return rel
}

// absDir resolves a stored cwd: absolute as is, relative to root otherwise.
func absDir(root, cwd string) string {
	if filepath.IsAbs(cwd) {
		return filepath.Clean(cwd)
	}
	return filepath.Join(root, cwd)
}

// cleanRel normalises a relative cwd ("", "./" → ".").
func cleanRel(cwd string) string {
	if cwd == "" {
		return "."
	}
	return filepath.Clean(cwd)
}

// unknownError says that name is neither configured nor running.
func unknownError(name string) error {
	return fmt.Errorf("unknown process %q (try: rov list)", name)
}
