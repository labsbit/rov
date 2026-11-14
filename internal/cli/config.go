package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/labsbit/rov/internal/config"
	"github.com/labsbit/rov/internal/configedit"
	"github.com/labsbit/rov/internal/shell"
)

var (
	fAfter  = flagDef{names: []string{"--after"}, value: true}
	fFirst  = flagDef{names: []string{"--first"}}
	fLast   = flagDef{names: []string{"--last"}}
	fUp     = flagDef{names: []string{"--up"}}
	fDown   = flagDef{names: []string{"--down"}}
	fDesc   = flagDef{names: []string{"--desc", "--description"}, value: true}
	fDocker = flagDef{names: []string{"--docker"}, value: true}
	fCmd    = flagDef{names: []string{"--cmd"}, value: true}
)

func (a *App) cmdConfig(ctx context.Context, argv []string) error {
	if len(argv) == 0 {
		return usagef("rov config add|set|rm|mv <name> … (see rov help)")
	}
	switch sub, rest := argv[0], argv[1:]; sub {
	case "add":
		return a.configAdd(rest)
	case "set":
		return a.configSet(ctx, rest)
	case "rm", "remove":
		return a.configRm(ctx, rest)
	case "mv", "move":
		return a.configMv(rest)
	default:
		return usagef("unknown config command %q (add, set, rm or mv)", sub)
	}
}

// position reads --after/--first/--last (and --up/--down when move).
func position(ar *args, move bool) (configedit.Position, error) {
	pos := configedit.Position{After: ar.str("--after"), First: ar.has("--first"), Last: ar.has("--last")}
	if move {
		pos.Up, pos.Down = ar.has("--up"), ar.has("--down")
	}
	n := 0
	for _, b := range []bool{pos.First, pos.Last, pos.Up, pos.Down, pos.After != ""} {
		if b {
			n++
		}
	}
	if n > 1 {
		return pos, usagef("give one position")
	}
	return pos, nil
}

// write applies op to rov.yaml under the config lock.
func (a *App) write(op configedit.Op) error {
	e := a.engine
	_, err := configedit.Write(e.ConfigPath(), e.LockDir(), nil, op)
	if err == nil {
		_, _ = e.Reload()
	}
	return err
}

func (a *App) configAdd(argv []string) error {
	ar, err := parseCmdArgs(argv, fAfter, fFirst, fLast, fCwd, fEnv, fDesc, fDocker, fCmd)
	if err != nil {
		return err
	}
	if len(ar.pos) != 1 {
		return usagef("rov config add <name> [opts] -- <cmd...>")
	}
	cmd := ar.str("--cmd")
	switch {
	case ar.dashdash && cmd != "":
		return usagef("give the command once: --cmd or after --")
	case ar.dashdash:
		cmd = shell.Command(ar.rest)
	}
	if strings.TrimSpace(cmd) == "" {
		return usagef("missing command: rov config add %s -- <cmd...>", ar.pos[0])
	}
	env, err := parseEnv(ar.all("--env"))
	if err != nil {
		return err
	}
	pos, err := position(ar, false)
	if err != nil {
		return err
	}
	p := config.Process{Name: ar.pos[0], Cmd: cmd, Cwd: ar.str("--cwd"), Env: env, Description: ar.str("--desc"), Docker: ar.str("--docker")}
	if err := a.write(configedit.Add{Proc: p, Pos: pos}); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "rov.yaml: added %s (%s)\n", p.Name, pos)
	return nil
}

func (a *App) configSet(ctx context.Context, argv []string) error {
	ar, err := parseCmdArgs(argv, fCmd, fCwd, fEnv, fDesc, fDocker,
		flagDef{names: []string{"--unset-env"}, value: true},
		flagDef{names: []string{"--rename"}, value: true},
	)
	if err != nil {
		return err
	}
	if len(ar.pos) != 1 {
		return usagef("rov config set <name> [--cmd CMD] [--cwd DIR] [--env K=V]... [--unset-env K]... [--desc TEXT] [--docker PROJECT] [--rename NEW]")
	}
	if ar.dashdash {
		if ar.has("--cmd") {
			return usagef("give the command once: --cmd or after --")
		}
		cmd := shell.Command(ar.rest)
		ar.vals["--cmd"] = []string{cmd}
	}
	e := a.engine
	name := ar.pos[0]
	cur, ok := e.Config().Get(name)
	if !ok {
		return fmt.Errorf("no process %q in rov.yaml", name)
	}
	patch := configedit.Patch{
		Cmd: ar.strPtr("--cmd"), Cwd: ar.strPtr("--cwd"), Description: ar.strPtr("--desc"),
		Docker: ar.strPtr("--docker"), Rename: ar.strPtr("--rename"),
	}
	var changed []string
	for _, f := range []string{"--cmd", "--cwd", "--desc", "--docker"} {
		if ar.has(f) {
			changed = append(changed, strings.TrimPrefix(f, "--"))
		}
	}
	if ar.has("--env") || ar.has("--unset-env") {
		set, err := parseEnv(ar.all("--env"))
		if err != nil {
			return err
		}
		env := mergeEnv(cur.Env, set, ar.all("--unset-env"))
		patch.Env = &env
		changed = append(changed, "env")
	}
	if patch.Rename != nil {
		st, err := e.Status(ctx)
		if err != nil {
			return err
		}
		if p := st.Get(name); p != nil && p.Running() {
			return fmt.Errorf("%s is running: stop it first (rov stop %s), then rename", name, name)
		}
		changed = append(changed, "name")
	}
	if len(changed) == 0 {
		return usagef("nothing to set (see rov help)")
	}
	if err := a.write(configedit.Set{Name: name, Patch: patch}); err != nil {
		return err
	}
	if patch.Rename != nil {
		// The old name's stopped session would linger as ad-hoc.
		_ = e.DropSession(ctx, name)
		fmt.Fprintf(a.Stdout, "rov.yaml: renamed %s to %s\n", name, *patch.Rename)
		return nil
	}
	fmt.Fprintf(a.Stdout, "rov.yaml: updated %s (%s)\n", name, strings.Join(changed, ", "))
	if st, err := e.Status(ctx); err == nil {
		if p := st.Get(name); p != nil && p.Running() && p.Changed {
			fmt.Fprintf(a.Stdout, "%s is running the old definition: rov restart %s applies it\n", name, name)
		}
	}
	return nil
}

// mergeEnv returns cur with set applied (updated in place or appended) and
// the unset keys removed.
func mergeEnv(cur, set []config.EnvVar, unset []string) []config.EnvVar {
	out := append([]config.EnvVar(nil), cur...)
	for _, s := range set {
		found := false
		for i := range out {
			if out[i].Key == s.Key {
				out[i].Value, found = s.Value, true
			}
		}
		if !found {
			out = append(out, s)
		}
	}
	drop := map[string]bool{}
	for _, k := range unset {
		drop[k] = true
	}
	kept := out[:0]
	for _, kv := range out {
		if !drop[kv.Key] {
			kept = append(kept, kv)
		}
	}
	return kept
}

func (a *App) configRm(ctx context.Context, argv []string) error {
	ar, err := parseArgs(argv, flagDef{names: []string{"--stop"}})
	if err != nil {
		return err
	}
	if err := ar.exactly(1, "process name"); err != nil {
		return err
	}
	e := a.engine
	name := ar.pos[0]
	if _, ok := e.Config().Get(name); !ok {
		return fmt.Errorf("no process %q in rov.yaml", name)
	}
	st, err := e.Status(ctx)
	if err != nil {
		return err
	}
	if p := st.Get(name); p != nil && p.Running() {
		if !ar.has("--stop") {
			return fmt.Errorf("%s is running: rov config rm %s --stop stops it first", name, name)
		}
		msg, err := e.Stop(ctx, name)
		if err != nil {
			return err
		}
		fmt.Fprintf(a.Stdout, "%s: %s\n", name, msg)
	}
	if err := a.write(configedit.Remove{Name: name}); err != nil {
		return err
	}
	// A leftover session would show up as an ad-hoc process.
	_ = e.DropSession(ctx, name)
	fmt.Fprintf(a.Stdout, "rov.yaml: removed %s\n", name)
	return nil
}

func (a *App) configMv(argv []string) error {
	ar, err := parseArgs(argv, fAfter, fFirst, fLast, fUp, fDown)
	if err != nil {
		return err
	}
	if err := ar.exactly(1, "process name"); err != nil {
		return err
	}
	pos, err := position(ar, true)
	if err != nil {
		return err
	}
	if pos == (configedit.Position{}) {
		return usagef("where to? --after P, --first, --last, --up or --down")
	}
	if err := a.write(configedit.Move{Name: ar.pos[0], Pos: pos}); err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "rov.yaml: moved %s %s\n", ar.pos[0], pos)
	return nil
}

func (a *App) cmdInit(argv []string) error {
	ar, err := parseArgs(argv)
	if err != nil {
		return err
	}
	if err := ar.exactly(0, ""); err != nil {
		return err
	}
	dir := a.dir
	if dir == "" {
		wd, err := a.Getwd()
		if err != nil {
			return err
		}
		dir = wd
		// Inside a git repository the config belongs at its root.
		if root, _, err := config.FindRoot(wd); err == nil {
			dir = root
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", abs)
	}
	created, err := config.Init(abs)
	for _, f := range created {
		fmt.Fprintf(a.Stdout, "wrote %s\n", f)
	}
	return err
}
