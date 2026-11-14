// Package cli implements rov's command line: every command, its flags, text
// and --json output, and exit codes. The work is done by package core,
// which the TUI shares.
//
// Exit codes: 0 success · 1 the action failed (including timeouts and
// validation) · 2 usage error · 3 the process wasn't running when it had to
// be (send, wait).
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/labsbit/rov/internal/core"
)

// Exit codes.
const (
	ExitOK         = 0
	ExitFailed     = 1
	ExitUsage      = 2
	ExitNotRunning = 3
)

// App is one run of the CLI. Fields are the outside world, so tests can
// replace them.
type App struct {
	Stdout, Stderr io.Writer
	Stdin          io.Reader
	// Getwd returns the caller's working directory.
	Getwd func() (string, error)
	// Open opens the engine for a directory (core.Open by default).
	Open func(dir string) (*core.Engine, error)
	// RunTUI is the `rov` with no arguments.
	RunTUI func(*core.Engine) error
	// Loc is the time zone for printed times.
	Loc *time.Location

	dir    string // -C
	engine *core.Engine
}

// Main runs the CLI with os.Args-style arguments (without the program name)
// and returns the exit code.
func Main(argv []string, tui func(*core.Engine) error) int {
	app := &App{
		Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin,
		Getwd: os.Getwd, Open: core.Open, RunTUI: tui, Loc: time.Local,
	}
	return app.Run(argv)
}

// Run executes one command line and returns the exit code.
func (a *App) Run(argv []string) int {
	if len(argv) > 0 && argv[0] == "__pipe" {
		return runPipe(argv[1:], a.Stdin, a.Stderr)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if os.Getenv(envDetached) != "" {
		awaitDetachParent()
	}
	err := a.run(ctx, argv)
	return a.report(err)
}

// exitError carries a specific exit code; msg (if any) is printed as
// "rov: msg".
type exitError struct {
	code int
	msg  string
}

func (e *exitError) Error() string { return e.msg }

func (a *App) report(err error) int {
	if err == nil {
		return ExitOK
	}
	var ue *usageError
	var ee *exitError
	switch {
	case errors.As(err, &ue):
		fmt.Fprintf(a.Stderr, "rov: %s\n(rov help for usage)\n", ue.msg)
		return ExitUsage
	case errors.As(err, &ee):
		if ee.msg != "" {
			fmt.Fprintf(a.Stderr, "rov: %s\n", ee.msg)
		}
		return ee.code
	case errors.Is(err, core.ErrNotRunning):
		fmt.Fprintf(a.Stderr, "rov: %s\n", err)
		return ExitNotRunning
	case errors.Is(err, context.Canceled):
		return 130
	}
	fmt.Fprintf(a.Stderr, "rov: %s\n", err)
	return ExitFailed
}

func (a *App) run(ctx context.Context, argv []string) error {
	// Global flags come before the command.
	for len(argv) > 0 && strings.HasPrefix(argv[0], "-") {
		switch f := argv[0]; {
		case f == "-C":
			if len(argv) < 2 {
				return usagef("-C needs a directory")
			}
			a.dir, argv = argv[1], argv[2:]
		case strings.HasPrefix(f, "-C="):
			a.dir, argv = strings.TrimPrefix(f, "-C="), argv[1:]
		case f == "--version" || f == "-V":
			fmt.Fprintln(a.Stdout, VersionString())
			return nil
		case f == "-h" || f == "--help":
			return a.help(nil)
		default:
			return usagef("unknown flag %s", f)
		}
	}
	if len(argv) == 0 {
		e, err := a.open(ctx)
		if err != nil {
			return err
		}
		if a.RunTUI == nil {
			return a.cmdStatus(ctx, nil)
		}
		return a.RunTUI(e)
	}
	cmd, rest := argv[0], argv[1:]
	switch cmd {
	case "help":
		return a.help(rest)
	case "version":
		fmt.Fprintln(a.Stdout, VersionString())
		return nil
	case "init":
		return a.cmdInit(rest)
	case "skill":
		return a.cmdSkill(rest)
	}
	h, ok := commands[cmd]
	if !ok {
		return usagef("unknown command %q", cmd)
	}
	if _, err := a.open(ctx); err != nil {
		return err
	}
	return h(a, ctx, rest)
}

// commands maps a command name to its handler.
var commands map[string]func(*App, context.Context, []string) error

func init() {
	commands = map[string]func(*App, context.Context, []string) error{
		"up":      (*App).cmdUp,
		"restart": (*App).cmdRestart,
		"stop":    (*App).cmdStop,
		"down":    (*App).cmdDown,
		"spawn":   (*App).cmdSpawn,
		"rm":      (*App).cmdRm,
		"status":  (*App).cmdStatus,
		"show":    (*App).cmdShow,
		"logs":    (*App).cmdLogs,
		"ports":   (*App).cmdPorts,
		"wait":    (*App).cmdWait,
		"config":  (*App).cmdConfig,
		"list":    (*App).cmdList,
		"attach":  (*App).cmdAttach,
		"send":    (*App).cmdSend,
	}
}

// open opens the engine once, then runs the housekeeping every command
// does: stale log cleanup, which can never fail a command.
func (a *App) open(ctx context.Context) (*core.Engine, error) {
	if a.engine != nil {
		return a.engine, nil
	}
	dir := a.dir
	if dir == "" {
		wd, err := a.Getwd()
		if err != nil {
			return nil, err
		}
		dir = wd
	}
	e, err := a.Open(dir)
	if err != nil {
		return nil, err
	}
	a.engine = e
	_, _ = e.CollectLogs(ctx, nil)
	return e, nil
}

// withField returns v's JSON object with one more field at the end, keeping
// v's field order (embedding v would lose its MarshalJSON's fields).
func withField(v any, key string, val any) json.RawMessage {
	obj, err := core.Marshal(v)
	if err != nil || len(obj) < 2 || obj[len(obj)-1] != '}' {
		return obj
	}
	k, _ := core.Marshal(key)
	x, _ := core.Marshal(val)
	out := append([]byte{}, obj[:len(obj)-1]...)
	if len(obj) > 2 {
		out = append(out, ',')
	}
	out = append(out, k...)
	out = append(out, ':')
	out = append(out, x...)
	return append(out, '}')
}

// writeJSON prints v as indented JSON.
func (a *App) writeJSON(v any) error {
	enc := json.NewEncoder(a.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
