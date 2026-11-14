package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/labsbit/rov/internal/config"
	"github.com/labsbit/rov/internal/pipe"
	"github.com/labsbit/rov/internal/shell"
	"github.com/labsbit/rov/internal/tmux"
)

// launch (re)starts def in its session, creating the session if needed. The
// caller holds the process lock. Order matters:
//
//  1. stop what still runs of the old tree;
//  2. move the last run's logs to .prev (pipe.PrepareRun) — before tmux is
//     touched, so a failure here leaves the process as it was;
//  3. in one tmux call: create the session with the real command (or
//     respawn the pane with it), pipe-pane the new `rov __pipe` writer, and
//     store the definition and start time and clear the stopped flag.
//
// tmux runs a command chain without reading pane output or reaping
// children in between, so the pipe gets the command's first byte and a pane
// whose command exits at once can still be piped. The pipe-pane closes the
// old pipe, whose writer gets EOF and finishes into the moved files. If the
// call fails, nothing started: no session was created, or the pane kept its
// old (dead) process.
func (e *Engine) launch(ctx context.Context, def definition, s *tmux.Session) error {
	dir := def.dir(e.root)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fmt.Errorf("working directory %s does not exist", dir)
	}
	if s != nil {
		if _, err := e.stopMarked(ctx, s); err != nil {
			return err
		}
		if !s.Dead {
			e.waitDead(ctx, s.Name)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := pipe.PrepareRun(e.LogDir(), def.Name); err != nil {
		return fmt.Errorf("prepare logs of %s: %w", def.Name, err)
	}

	target := tmux.SessionTarget(def.Name)
	now := e.Now()
	var cmds []tmux.Cmd
	if s == nil {
		cmds = tmux.NewSessionCmds(tmux.NewSession{
			Name: def.Name, Dir: dir, Width: SessionWidth, Height: SessionHeight,
			Env: paneEnv(def.Env), Command: def.Cmd,
		}, e.serverOptions())
	} else {
		target = s.PaneID
		cmds = []tmux.Cmd{tmux.RespawnCmd(tmux.Respawn{Pane: target, Dir: dir, Env: paneEnv(def.Env), Command: def.Cmd})}
	}
	cmds = append(cmds,
		tmux.PipePaneCmd(target, e.pipeCommand(def.Name)),
		tmux.SetOptionCmd(target, OptKind, string(def.Kind)),
		tmux.SetOptionCmd(target, OptCmd, def.Cmd),
		tmux.SetOptionCmd(target, OptCwd, def.Cwd),
		tmux.SetOptionCmd(target, OptEnv, encodeEnv(def.Env)),
		tmux.SetOptionCmd(target, OptStarted, strconv.FormatInt(now.Unix(), 10)),
		tmux.SetOptionCmd(target, OptRunID, strconv.FormatInt(now.UnixNano(), 10)),
		tmux.SetOptionCmd(target, OptRunDef, ""),
		tmux.SetOptionCmd(target, OptStopped, ""),
	)
	if _, err := e.Tmux.Run(ctx, cmds...); err != nil {
		if s != nil && errIsGone(err) {
			return fmt.Errorf("session %s disappeared while restarting it", def.Name)
		}
		if s == nil {
			return fmt.Errorf("create session %s: %w", def.Name, err)
		}
		return fmt.Errorf("start %s: %w", def.Name, err)
	}
	return nil
}

// stopMarked is stopTree for a stop rov means: a live pane is flagged
// stopped first, so a status read while it dies says stopped rather than
// dead (killed by rov's own signal). The flag is taken back if the tree
// survives; a start clears it with the respawn.
func (e *Engine) stopMarked(ctx context.Context, s *tmux.Session) (int, error) {
	marked := false
	if !s.Dead && s.Option(OptStopped) != "1" {
		if _, err := e.Tmux.Run(ctx, tmux.SetOptionCmd(s.PaneID, OptStopped, "1")); err != nil && !errIsGone(err) {
			return 0, err
		}
		marked = true
	}
	n, err := e.stopTree(ctx, s)
	if err != nil && marked {
		_, _ = e.Tmux.Run(context.WithoutCancel(ctx), tmux.SetOptionCmd(s.PaneID, OptStopped, ""))
	}
	return n, err
}

// pipeCommand is the pipe-pane command: the rov binary's log writer.
func (e *Engine) pipeCommand(name string) string {
	return "exec " + shell.Join([]string{e.RovBin, "__pipe", e.root, name})
}

// paneEnv is the env passed to the pane: the definition's plus
// FORCE_COLOR=1 and CLICOLOR_FORCE=1 unless it sets them, for tools that
// don't colour without them (the pane is a real TTY either way).
func paneEnv(env []config.EnvVar) []string {
	out := make([]string, 0, len(env)+2)
	set := map[string]bool{}
	for _, kv := range env {
		out = append(out, kv.String())
		set[kv.Key] = true
	}
	for _, k := range []string{"FORCE_COLOR", "CLICOLOR_FORCE"} {
		if !set[k] {
			out = append(out, k+"=1")
		}
	}
	return out
}

// serverOptions are set on the project's server whenever rov creates a
// session. Commands run with tmux's default-shell, the user's $SHELL, as
// before; a shell that isn't POSIX (fish, nu, …) is replaced by /bin/sh,
// because stored spawn commands are POSIX-quoted.
func (e *Engine) serverOptions() tmux.ServerOptions {
	o := tmux.ServerOptions{RemainOnExit: true}
	switch filepath.Base(os.Getenv("SHELL")) {
	case "sh", "bash", "zsh", "dash", "ksh", "mksh", "ash", "yash", "busybox":
	default:
		o.DefaultShell = "/bin/sh"
	}
	return o
}

// session returns the session of name, if any.
func (e *Engine) session(ctx context.Context, name string) (*tmux.Session, error) {
	s, ok, err := e.Tmux.Session(ctx, name, sessionOptions...)
	if err != nil || !ok {
		return nil, err
	}
	return &s, nil
}

// definitionOf returns how to start name: rov.yaml first, then the session's
// stored definition (ad-hoc, or a name that left rov.yaml).
func (e *Engine) definitionOf(name string, s *tmux.Session) (definition, error) {
	if p, ok := e.Config().Get(name); ok {
		if probs := e.problemsOf(name); len(probs) > 0 {
			return definition{}, fmt.Errorf("rov.yaml: process %q can't start: %s", name, joinProblems(probs))
		}
		return configDefinition(p), nil
	}
	if s != nil {
		def := storedDefinition(*s)
		def.Kind = KindAdhoc
		if def.Cmd == "" {
			return definition{}, fmt.Errorf("%s has no stored command (it wasn't started by this rov); rov down %s and start it again", name, name)
		}
		return def, nil
	}
	return definition{}, unknownError(name)
}

func joinProblems(probs []string) string {
	out := probs[0]
	for _, p := range probs[1:] {
		out += "; " + p
	}
	return out
}

// errIsGone reports whether err means the session or server vanished.
func errIsGone(err error) bool {
	return errors.Is(err, tmux.ErrNoSession) || errors.Is(err, tmux.ErrNoServer)
}
