package core

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/labsbit/rov/internal/docker"
	"github.com/labsbit/rov/internal/ports"
	"github.com/labsbit/rov/internal/tmux"
)

// Sessions lists the project's tmux sessions with rov's options, in one tmux
// call. A server that isn't running has none. Cheap: the TUI polls it every
// second.
func (e *Engine) Sessions(ctx context.Context) ([]tmux.Session, error) {
	return e.Tmux.Sessions(ctx, sessionOptions...)
}

// ScanPorts reads every listening TCP socket and the process table once
// (/proc on Linux; a few ms). On systems without support it returns a nil
// snapshot and no error: processes just show no ports.
func (e *Engine) ScanPorts() (*ports.Snapshot, error) {
	snap, err := ports.Scan()
	if errors.Is(err, ports.ErrUnsupported) {
		return nil, nil
	}
	return snap, err
}

// Containers lists the compose containers under the project root (one
// `docker ps`, 2 s timeout). Without docker the error wraps
// docker.ErrUnavailable; the TUI polls it every 5 s.
func (e *Engine) Containers(ctx context.Context) ([]docker.Container, error) {
	return e.Docker.List(ctx, e.root)
}

// Inputs returns Compose inputs filled with the project's current config,
// paths and clock; the caller supplies the polled sources.
func (e *Engine) Inputs(sessions []tmux.Session, snap *ports.Snapshot, containers []docker.Container, dockerErr error) Inputs {
	return Inputs{
		Root: e.root, Project: e.name, Socket: e.Socket(),
		Config: e.Config(), ConfigErr: e.ConfigErr(), LogDir: e.LogDir(),
		Sessions: sessions, Ports: snap, Containers: containers, DockerErr: dockerErr,
		Now: e.Now(),
	}
}

// Status gathers every source (tmux, /proc and docker, concurrently),
// composes the status and records newly seen ports. Only a tmux failure is
// an error; a failed port scan or docker call shows up in the warnings.
func (e *Engine) Status(ctx context.Context) (*Status, error) {
	_, _ = e.Reload()
	var (
		wg         sync.WaitGroup
		snap       *ports.Snapshot
		snapErr    error
		containers []docker.Container
		dockerErr  error
	)
	wg.Add(2)
	go func() { defer wg.Done(); snap, snapErr = e.ScanPorts() }()
	go func() { defer wg.Done(); containers, dockerErr = e.Containers(ctx) }()
	sessions, err := e.Sessions(ctx)
	wg.Wait()
	if err != nil {
		return nil, err
	}
	st := Compose(e.Inputs(sessions, snap, containers, dockerErr))
	if snapErr != nil {
		st.Warnings = append(st.Warnings, "ports: "+snapErr.Error())
	}
	_ = e.RecordPorts(ctx, st)
	return st, nil
}

// RecordPorts stores each process's current ports as @rov_last_ports when
// they differ from what is stored, so a later conflict can be explained.
// It writes nothing when nothing changed (one tmux call otherwise); the TUI
// calls it after composing a status with a fresh port scan.
func (e *Engine) RecordPorts(ctx context.Context, st *Status) error {
	var cmds []tmux.Cmd
	for i := range st.Processes {
		p := &st.Processes[i]
		if !p.HasSession || len(p.LastPorts) == 0 || slices.Equal(p.LastPorts, p.storedLastPorts) {
			continue
		}
		cmds = append(cmds, tmux.SetOptionCmd(p.PaneID, OptLastPorts, encodePorts(p.LastPorts)))
		p.storedLastPorts = p.LastPorts
	}
	_, err := e.Tmux.Run(ctx, cmds...)
	return err
}
