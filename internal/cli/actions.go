package cli

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/labsbit/rov/internal/config"
	"github.com/labsbit/rov/internal/core"
	"github.com/labsbit/rov/internal/shell"
)

func validName(name string) error {
	if err := config.ValidName(name); err != nil {
		return fmt.Errorf("%w (try: rov list)", err)
	}
	return nil
}

// forEach validates every name first (expanding "all"), then runs act on
// each, printing "<name>: <msg>". A failure is reported and the others
// still run; the command then exits 1. verb is the command ("restart"): an
// action that rov can't run because it runs inside the tree it stops is
// re-run from a detached rov (see detach). reverse runs "all" in reverse
// (ad-hoc newest first, then rov.yaml bottom up), for stop and down:
// dependents go before the infra they use.
func (a *App) forEach(ctx context.Context, argv []string, verb string, reverse bool, act func(context.Context, string) (string, error)) error {
	ar, err := parseArgs(argv)
	if err != nil {
		return err
	}
	if len(ar.pos) == 0 {
		return usagef("missing process name (try: rov list)")
	}
	names, err := a.engine.Resolve(ctx, ar.pos)
	if err != nil {
		return err
	}
	if reverse && slices.Contains(ar.pos, "all") {
		slices.Reverse(names)
	}
	failed := 0
	for _, n := range names {
		msg, err := act(ctx, n)
		if inside := (*core.InsideTreeError)(nil); errors.As(err, &inside) {
			msg, err = a.detach([]string{verb, n}, inside)
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failed++
			fmt.Fprintf(a.Stderr, "rov: %s: %s\n", n, err)
			continue
		}
		fmt.Fprintf(a.Stdout, "%s: %s\n", n, msg)
	}
	if failed > 0 {
		return &exitError{code: ExitFailed}
	}
	return nil
}

// envDetached marks a rov started by detach; its value is the pid of the
// rov that started it.
const envDetached = "ROV_DETACHED_FROM"

// detach re-runs `rov -C <root> <args>` in a new session, detached from
// this rov, which runs inside the process tree the action stops (an agent
// in the pane typing `rov restart web`): stopping the tree would kill this
// rov halfway, and the detached one finishes the job. Its output goes to
// <LockDir>/detached/<name>.log. A rov that is itself detached doesn't
// detach again (the tree still holds it: a subreaper adopted it), it fails
// with why.
func (a *App) detach(args []string, why *core.InsideTreeError) (string, error) {
	if os.Getenv(envDetached) != "" {
		return "", why
	}
	e := a.engine
	logPath := filepath.Join(e.LockDir(), "detached", why.Name+".log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return "", fmt.Errorf("%w (and detaching failed: %v)", why, err)
	}
	out, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", fmt.Errorf("%w (and detaching failed: %v)", why, err)
	}
	defer out.Close()
	fmt.Fprintf(out, "%s rov (pid %d) runs inside %s's tree (pid %d, %s): detached: rov %s\n",
		time.Now().Format(time.RFC3339), os.Getpid(), why.Name, why.PID, why.Comm, strings.Join(args, " "))
	cmd := exec.Command(e.RovBin, append([]string{"-C", e.Root()}, args...)...)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Env = append(os.Environ(), envDetached+"="+strconv.Itoa(os.Getpid()))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Dir = e.Root()
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("%w (and detaching failed: %v)", why, err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return fmt.Sprintf("this rov runs inside its process tree, so a detached rov (pid %d) runs `rov %s` once this one exits; output: %s",
		pid, strings.Join(args, " "), cmp.Or(relPath(e.Root(), logPath), logPath)), nil
}

// awaitDetachParent, in a rov started by detach, waits (up to 10 s) for the
// rov that started it to exit, so that it is out of the tree it acts on.
func awaitDetachParent() {
	from, err := strconv.Atoi(os.Getenv(envDetached))
	if err != nil || from <= 1 {
		return
	}
	deadline := time.Now().Add(10 * time.Second)
	for os.Getppid() == from && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
}

func (a *App) cmdUp(ctx context.Context, argv []string) error {
	return a.forEach(ctx, argv, "up", false, a.engine.Up)
}

func (a *App) cmdRestart(ctx context.Context, argv []string) error {
	return a.forEach(ctx, argv, "restart", false, a.engine.Restart)
}

func (a *App) cmdStop(ctx context.Context, argv []string) error {
	return a.forEach(ctx, argv, "stop", true, a.engine.Stop)
}

func (a *App) cmdDown(ctx context.Context, argv []string) error {
	return a.forEach(ctx, argv, "down", true, a.engine.Down)
}

func (a *App) cmdRm(ctx context.Context, argv []string) error {
	ar, err := parseArgs(argv, flagDef{names: []string{"--purge"}})
	if err != nil {
		return err
	}
	if len(ar.pos) == 0 {
		return usagef("missing process name")
	}
	for _, n := range ar.pos {
		if n == "all" {
			return usagef("rm takes names, not all (rov down all takes everything down)")
		}
		if err := validName(n); err != nil {
			return err
		}
		if _, ok := a.engine.Config().Get(n); ok {
			return fmt.Errorf("%s is configured in rov.yaml: use rov down %s (or rov config rm %s to remove it)", n, n, n)
		}
	}
	purge := ar.has("--purge")
	failed := 0
	for _, n := range ar.pos {
		msg, err := a.engine.Rm(ctx, n, purge)
		if inside := (*core.InsideTreeError)(nil); errors.As(err, &inside) {
			args := []string{"rm", n}
			if purge {
				args = append(args, "--purge")
			}
			msg, err = a.detach(args, inside)
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failed++
			fmt.Fprintf(a.Stderr, "rov: %s: %s\n", n, err)
			continue
		}
		fmt.Fprintf(a.Stdout, "%s: %s\n", n, msg)
	}
	if failed > 0 {
		return &exitError{code: ExitFailed}
	}
	return nil
}

var (
	fCwd = flagDef{names: []string{"--cwd"}, value: true}
	fEnv = flagDef{names: []string{"--env", "-e"}, value: true}
)

// parseEnv turns --env K=V values into env vars.
func parseEnv(vals []string) ([]config.EnvVar, error) {
	var env []config.EnvVar
	for _, kv := range vals {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, usagef("--env wants KEY=VALUE, got %q", kv)
		}
		if err := config.ValidEnvKey(k); err != nil {
			return nil, usagef("--env: %v", err)
		}
		replaced := false
		for i := range env {
			if env[i].Key == k {
				env[i].Value, replaced = v, true
			}
		}
		if !replaced {
			env = append(env, config.EnvVar{Key: k, Value: v})
		}
	}
	return env, nil
}

func (a *App) cmdSpawn(ctx context.Context, argv []string) error {
	ar, err := parseCmdArgs(argv, fCwd, fEnv,
		flagDef{names: []string{"--replace"}},
		flagDef{names: []string{"--wait"}},
		flagDef{names: []string{"--timeout"}, value: true},
	)
	if err != nil {
		return err
	}
	if len(ar.pos) == 0 {
		return usagef("missing name: rov spawn <name> [opts] -- <cmd...>")
	}
	if len(ar.pos) > 1 || !ar.dashdash {
		return usagef("put the command after --: rov spawn %s -- <cmd...>", ar.pos[0])
	}
	if len(ar.rest) == 0 {
		return usagef("missing command after --")
	}
	env, err := parseEnv(ar.all("--env"))
	if err != nil {
		return err
	}
	timeout, err := ar.duration("--timeout", core.DefaultWaitTimeout)
	if err != nil {
		return err
	}
	if ar.has("--timeout") && !ar.has("--wait") {
		return usagef("--timeout goes with --wait")
	}
	dir, err := a.spawnDir(ar.str("--cwd"), ar.has("--cwd"))
	if err != nil {
		return err
	}
	name := ar.pos[0]
	if err := validName(name); err != nil {
		return err
	}
	msg, err := a.engine.Spawn(ctx, core.SpawnOptions{
		Name: name, Cmd: shell.Command(ar.rest), Dir: dir, Env: env, Replace: ar.has("--replace"),
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "%s: %s\n", name, msg)
	if ar.has("--wait") {
		return a.wait(ctx, name, core.WaitCondition{}, timeout)
	}
	return nil
}

// spawnDir returns the working dir of a spawn, relative to the project
// root: --cwd relative to the root or absolute inside it; without --cwd the
// caller's cwd, which must lie inside the root.
func (a *App) spawnDir(flag string, set bool) (string, error) {
	root := a.engine.Root()
	if set {
		dir := flag
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(root, dir)
		}
		rel, ok := relInside(root, dir)
		if !ok {
			return "", fmt.Errorf("--cwd %s is outside the project root %s", flag, root)
		}
		return rel, nil
	}
	wd, err := a.Getwd()
	if err != nil {
		return "", err
	}
	rel, ok := relInside(root, wd)
	if !ok {
		return "", fmt.Errorf("cwd %s is outside the project root %s; pass --cwd", wd, root)
	}
	return rel, nil
}

// relInside returns dir relative to root ("." for root) and whether it lies
// inside it, symlinks resolved (root already is).
func relInside(root, dir string) (string, bool) {
	dir = filepath.Clean(dir)
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	rel, err := filepath.Rel(root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

func (a *App) cmdSend(ctx context.Context, argv []string) error {
	// Everything after the name is the text, dashes included.
	if len(argv) == 0 {
		return usagef("missing process name: rov send <name> <text...>")
	}
	name, text := argv[0], argv[1:]
	if len(text) > 0 && text[0] == "--" {
		text = text[1:]
	}
	if err := validName(name); err != nil {
		return err
	}
	return a.engine.Send(ctx, name, strings.Join(text, " "))
}

func (a *App) cmdAttach(ctx context.Context, argv []string) error {
	ar, err := parseArgs(argv)
	if err != nil {
		return err
	}
	if err := ar.exactly(1, "process name (try: rov list)"); err != nil {
		return err
	}
	name := ar.pos[0]
	names, err := a.engine.Resolve(ctx, []string{name})
	if err != nil {
		return err
	}
	if names[0] != name {
		return usagef("attach takes one process, not all")
	}
	if !isTerminal(os.Stdin) {
		return errors.New("attach needs a terminal (it's for humans; agents use rov logs and rov send)")
	}
	// As before: attaching starts the process if it isn't running.
	if _, err := a.engine.Up(ctx, name); err != nil {
		return err
	}
	return a.engine.Tmux.ExecAttach(name)
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
