package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/labsbit/rov/internal/config"
	"github.com/labsbit/rov/internal/tmux"
)

// adhocDefinition validates o as the definition of an ad-hoc process: a
// valid name that rov.yaml doesn't use, a command, valid env keys and an
// existing working directory.
func (e *Engine) adhocDefinition(o SpawnOptions) (definition, error) {
	if err := config.ValidName(o.Name); err != nil {
		return definition{}, err
	}
	if _, ok := e.Config().Get(o.Name); ok {
		return definition{}, fmt.Errorf("%s is configured in rov.yaml: start it with rov up %s, or spawn under another name", o.Name, o.Name)
	}
	if strings.TrimSpace(o.Cmd) == "" {
		return definition{}, errors.New("missing command (rov spawn <name> -- <cmd...>)")
	}
	for _, kv := range o.Env {
		if err := config.ValidEnvKey(kv.Key); err != nil {
			return definition{}, err
		}
	}
	dir := absDir(e.root, cleanRel(o.Dir))
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return definition{}, fmt.Errorf("working directory %s does not exist", dir)
	}
	return definition{Name: o.Name, Kind: KindAdhoc, Cmd: o.Cmd, Cwd: relTo(e.root, dir), Env: o.Env}, nil
}

// Redefine replaces the stored definition (command, working dir, env) of
// the existing ad-hoc process o.Name without restarting it: a running
// process keeps running what it was started with, and the next start or
// restart runs the new definition. (Spawn with Replace changes it and
// restarts at once.) o.Replace is ignored. The TUI's edit form uses it.
func (e *Engine) Redefine(ctx context.Context, o SpawnOptions) (string, error) {
	_, _ = e.Reload()
	def, err := e.adhocDefinition(o)
	if err != nil {
		return "", err
	}
	return e.locked(ctx, o.Name, "redefining", func() (string, error) {
		s, err := e.session(ctx, o.Name)
		if err != nil {
			return "", err
		}
		if s == nil {
			return "", unknownError(o.Name)
		}
		cmds := []tmux.Cmd{
			tmux.SetOptionCmd(s.PaneID, OptKind, string(KindAdhoc)),
			tmux.SetOptionCmd(s.PaneID, OptCmd, def.Cmd),
			tmux.SetOptionCmd(s.PaneID, OptCwd, def.Cwd),
			tmux.SetOptionCmd(s.PaneID, OptEnv, encodeEnv(def.Env)),
		}
		if !s.Dead && s.Option(OptRunDef) == "" && s.Option(OptCmd) != "" {
			// Keep what runs, so the status can say the definition changed.
			cmds = append(cmds, tmux.SetOptionCmd(s.PaneID, OptRunDef, encodeRunDef(storedDefinition(*s))))
		}
		_, err = e.Tmux.Run(ctx, cmds...)
		if err != nil {
			return "", err
		}
		if !s.Dead {
			return "definition saved; restart to apply", nil
		}
		return "definition saved", nil
	})
}
