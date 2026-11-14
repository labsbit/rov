package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/labsbit/rov/internal/config"
	"github.com/labsbit/rov/internal/logs"
	"github.com/labsbit/rov/internal/pipe"
	"github.com/labsbit/rov/internal/tmux"
)

// Every action on a process returns a short message for "<name>: <msg>"
// ("started", "already running", …) and takes the process lock. They are
// safe to run concurrently (the TUI runs them in the background) and reload
// rov.yaml first.

// Resolve validates the process names of an action and expands "all" into
// every process — config order, then ad-hoc by start time. Every name is
// checked before anything runs, so a typo in the third name can't leave the
// first two restarted. Duplicates are dropped.
func (e *Engine) Resolve(ctx context.Context, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, errors.New("missing process name (try: rov list)")
	}
	_, _ = e.Reload()
	sessions, err := e.Sessions(ctx)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	var all []string
	for _, n := range e.Config().Names() {
		known[n] = true
		all = append(all, n)
	}
	st := Compose(e.Inputs(sessions, nil, nil, nil))
	for _, p := range st.Processes {
		if !known[p.Name] {
			known[p.Name] = true
			all = append(all, p.Name)
		}
	}
	if slices.Contains(names, "all") {
		return all, nil
	}
	var out []string
	for _, n := range names {
		if err := config.ValidName(n); err != nil {
			return nil, err
		}
		if !known[n] {
			return nil, unknownError(n)
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// Up starts name unless it is running.
func (e *Engine) Up(ctx context.Context, name string) (string, error) {
	_, _ = e.Reload()
	return e.locked(ctx, name, "starting", func() (string, error) {
		s, err := e.session(ctx, name)
		if err != nil {
			return "", err
		}
		def, err := e.definitionOf(name, s)
		if err != nil {
			return "", err
		}
		if s != nil && !s.Dead {
			return "already running", nil
		}
		if err := e.launch(ctx, def, s); err != nil {
			return "", err
		}
		return "started", nil
	})
}

// Restart stops the process tree of name (if anything runs) and starts it
// again with its current definition.
func (e *Engine) Restart(ctx context.Context, name string) (string, error) {
	_, _ = e.Reload()
	return e.locked(ctx, name, "restarting", func() (string, error) {
		s, err := e.session(ctx, name)
		if err != nil {
			return "", err
		}
		def, err := e.definitionOf(name, s)
		if err != nil {
			return "", err
		}
		if err := e.launch(ctx, def, s); err != nil {
			return "", err
		}
		return "restarted", nil
	})
}

// Stop stops the process tree of name and keeps the session, flagged as
// stopped (so it isn't mistaken for a crash: the killed process reports a
// signal either way). For a process with `docker:` it also runs
// `docker compose -p <project> stop`: the containers stay and the next up
// restarts them.
func (e *Engine) Stop(ctx context.Context, name string) (string, error) {
	_, _ = e.Reload()
	return e.locked(ctx, name, "stopping", func() (string, error) {
		s, err := e.session(ctx, name)
		if err != nil {
			return "", err
		}
		cp, isCfg := e.Config().Get(name)
		if s == nil && !isCfg {
			return "", unknownError(name)
		}
		msg := "not running"
		if s != nil {
			n, err := e.stopMarked(ctx, s)
			if err != nil {
				return "", err
			}
			switch {
			case !s.Dead:
				e.waitDead(ctx, name)
				if _, err := e.Tmux.Run(ctx, tmux.SetOptionCmd(s.PaneID, OptStopped, "1")); err != nil && !errIsGone(err) {
					return "", err
				}
				msg = "stopped"
			case n > 0:
				msg = fmt.Sprintf("not running; stopped %s left over from its last run", plural(n, "process", "processes"))
			}
		}
		if isCfg && cp.Docker != "" {
			if err := e.Docker.ComposeStop(ctx, cp.Docker); err != nil {
				return "", fmt.Errorf("%s; docker compose -p %s stop failed: %w", msg, cp.Docker, err)
			}
			msg += "; containers stopped (docker compose stop)"
			// A one-shot (compose up --wait) has exited long ago: the row
			// still reads stopped, as the containers are.
			if s != nil && s.Dead {
				if _, err := e.Tmux.Run(ctx, tmux.SetOptionCmd(s.PaneID, OptStopped, "1")); err != nil && !errIsGone(err) {
					return "", err
				}
			}
		}
		return msg, nil
	})
}

// Down stops name and kills its session. For a process with `docker:` it
// also runs `docker compose -p <project> down` (volumes are kept: rov never
// passes -v). For an ad-hoc process it is Rm.
func (e *Engine) Down(ctx context.Context, name string) (string, error) {
	_, _ = e.Reload()
	cp, isCfg := e.Config().Get(name)
	if !isCfg {
		return e.Rm(ctx, name, false)
	}
	return e.locked(ctx, name, "taking down", func() (string, error) {
		s, err := e.session(ctx, name)
		if err != nil {
			return "", err
		}
		msg := "no session"
		if s != nil {
			if _, err := e.stopMarked(ctx, s); err != nil {
				return "", err
			}
			if err := e.Tmux.KillSession(ctx, name); err != nil {
				return "", err
			}
			msg = "session killed"
		}
		if cp.Docker != "" {
			if err := e.Docker.ComposeDown(ctx, cp.Docker); err != nil {
				return "", fmt.Errorf("%s; docker compose -p %s down failed: %w", msg, cp.Docker, err)
			}
			msg += "; containers removed (docker compose down, volumes kept)"
		}
		return msg, nil
	})
}

// SpawnOptions describes an ad-hoc process.
type SpawnOptions struct {
	Name string
	// Cmd is the shell command (see shell.Command for turning arguments
	// into one).
	Cmd string
	// Dir is the working directory: absolute, or relative to the root; ""
	// is the root.
	Dir string
	Env []config.EnvVar
	// Replace stops a running process of the same name and starts Cmd
	// instead. An exited ad-hoc process is always replaced.
	Replace bool
}

// ErrAlreadyRunning is wrapped by Spawn's error when the name is taken by a
// running process and Replace isn't set.
var ErrAlreadyRunning = errors.New("already running")

// Spawn starts an ad-hoc process. Its definition is kept in the session (not
// in rov.yaml), and it stays listed after it exits until Rm.
func (e *Engine) Spawn(ctx context.Context, o SpawnOptions) (string, error) {
	_, _ = e.Reload()
	def, err := e.adhocDefinition(o)
	if err != nil {
		return "", err
	}
	return e.locked(ctx, o.Name, "spawning", func() (string, error) {
		s, err := e.session(ctx, o.Name)
		if err != nil {
			return "", err
		}
		if s != nil && !s.Dead && !o.Replace {
			return "", fmt.Errorf("%s is %w (pid %d): use --replace to restart it with this command, or pick another name", o.Name, ErrAlreadyRunning, s.PanePID)
		}
		if err := e.launch(ctx, def, s); err != nil {
			return "", err
		}
		if s != nil {
			return "replaced", nil
		}
		return "started", nil
	})
}

// Rm stops an ad-hoc process and removes its session; purge also deletes
// its logs. Configured processes can't be removed (Down them, or remove them
// from rov.yaml).
func (e *Engine) Rm(ctx context.Context, name string, purge bool) (string, error) {
	_, _ = e.Reload()
	if err := config.ValidName(name); err != nil {
		return "", err
	}
	if _, ok := e.Config().Get(name); ok {
		return "", fmt.Errorf("%s is configured in rov.yaml: use rov down %s (or rov config rm %s to remove it)", name, name, name)
	}
	return e.locked(ctx, name, "removing", func() (string, error) {
		s, err := e.session(ctx, name)
		if err != nil {
			return "", err
		}
		if s == nil {
			if purge && e.hasLogs(name) {
				if err := logs.Purge(e.LogDir(), name); err != nil {
					return "", err
				}
				return "logs deleted", nil
			}
			return "", unknownError(name)
		}
		if _, err := e.stopMarked(ctx, s); err != nil {
			return "", err
		}
		if err := e.Tmux.KillSession(ctx, name); err != nil {
			return "", err
		}
		if purge {
			if err := logs.Purge(e.LogDir(), name); err != nil {
				return "removed; deleting its logs failed: " + err.Error(), nil
			}
			return "removed, logs deleted", nil
		}
		return "removed", nil
	})
}

// DropSession stops what runs in the session of name and kills the session,
// leaving docker alone: for rov config rm and renames, whose old session
// would otherwise linger as an ad-hoc process. No session is not an error.
func (e *Engine) DropSession(ctx context.Context, name string) error {
	_, err := e.locked(ctx, name, "removing the session of", func() (string, error) {
		s, err := e.session(ctx, name)
		if err != nil || s == nil {
			return "", err
		}
		if _, err := e.stopMarked(ctx, s); err != nil {
			return "", err
		}
		return "", e.Tmux.KillSession(ctx, name)
	})
	return err
}

// Send types text into the pane of a running process, followed by Enter.
// It fails with ErrNotRunning when the process isn't running.
func (e *Engine) Send(ctx context.Context, name, text string) error {
	_, _ = e.Reload()
	s, err := e.session(ctx, name)
	if err != nil {
		return err
	}
	if s == nil {
		if _, ok := e.Config().Get(name); !ok {
			return unknownError(name)
		}
	}
	if s == nil || s.Dead {
		return fmt.Errorf("%s is %w", name, ErrNotRunning)
	}
	return e.Tmux.SendText(ctx, s.PaneID, text, true)
}

// hasLogs reports whether any log file of name exists.
func (e *Engine) hasLogs(name string) bool {
	if pipe.CheckName(name) != nil {
		return false
	}
	for _, f := range pipe.Paths(e.LogDir(), name).All() {
		if _, err := os.Lstat(f); err == nil {
			return true
		}
	}
	return false
}

// KnownName reports whether name is configured, has a session, or has logs:
// whether `rov logs name` has something to read.
func (e *Engine) KnownName(ctx context.Context, name string) bool {
	if _, ok := e.Config().Get(name); ok {
		return true
	}
	if s, err := e.session(ctx, name); err == nil && s != nil {
		return true
	}
	return e.hasLogs(name)
}

// LogPaths returns the log files of name.
func (e *Engine) LogPaths(name string) pipe.FilesSet { return pipe.Paths(e.LogDir(), name) }

// LockPath returns the lock file of process name (for messages and tests):
// <LockDir>/proc/<name>, so no process name can collide with the rov.yaml
// lock (<LockDir>/config) or the other files in LockDir.
func (e *Engine) LockPath(name string) string { return filepath.Join(e.LockDir(), "proc", name) }

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
