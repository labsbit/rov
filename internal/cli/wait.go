package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/labsbit/rov/internal/core"
	"github.com/labsbit/rov/internal/logs"
)

// waitTailLines is how many log lines a failed wait prints.
const waitTailLines = 20

func (a *App) cmdWait(ctx context.Context, argv []string) error {
	ar, err := parseArgs(argv,
		flagDef{names: []string{"--port"}, value: true},
		flagDef{names: []string{"--log"}, value: true},
		flagDef{names: []string{"--exit"}},
		flagDef{names: []string{"--healthy"}},
		flagDef{names: []string{"--timeout", "-t"}, value: true},
	)
	if err != nil {
		return err
	}
	if err := ar.exactly(1, "process name (try: rov list)"); err != nil {
		return err
	}
	var cond core.WaitCondition
	if ar.has("--port") {
		p, err := ar.int("--port", 0)
		if err != nil || p < 1 || p > 65535 {
			return usagef("--port wants a TCP port number, got %q", ar.str("--port"))
		}
		cond.Port = p
	}
	if ar.has("--log") {
		re, err := regexp.Compile(ar.str("--log"))
		if err != nil {
			return usagef("--log: %v", err)
		}
		cond.Log = re
	}
	cond.Exit = ar.has("--exit")
	cond.Healthy = ar.has("--healthy")
	n := 0
	for _, f := range []string{"--port", "--log", "--exit", "--healthy"} {
		if ar.has(f) {
			n++
		}
	}
	if n > 1 {
		return usagef("give one condition: --port, --log, --exit or --healthy")
	}
	timeout, err := ar.duration("--timeout", core.DefaultWaitTimeout)
	if err != nil {
		return err
	}
	return a.wait(ctx, ar.pos[0], cond, timeout)
}

// wait runs core.Wait and maps the outcome to the exit code: 0 met, 1
// timeout, 3 ended first. On 1 and 3 it prints the last log lines, so the
// caller sees why without another call.
func (a *App) wait(ctx context.Context, target string, cond core.WaitCondition, timeout time.Duration) error {
	res, err := a.engine.Wait(ctx, target, cond, timeout)
	if err != nil {
		return err
	}
	if res.Outcome == core.WaitMet {
		fmt.Fprintf(a.Stdout, "%s: %s\n", target, res.Message)
		return nil
	}
	code := ExitFailed
	if res.Outcome == core.WaitEnded {
		code = ExitNotRunning
	}
	fmt.Fprintf(a.Stderr, "rov: %s: %s\n", target, res.Message)
	a.printTail(ctx, target)
	return &exitError{code: code}
}

// printTail prints the last lines of target's log (docker logs for a
// container) under a header.
func (a *App) printTail(ctx context.Context, target string) {
	var lines []string
	var from string
	if ref, ok := core.ParseTarget(target); ok {
		st, err := a.engine.Status(ctx)
		if err != nil {
			return
		}
		c, _, err := core.FindContainer(st, ref)
		if err != nil {
			return
		}
		if lines, err = a.dockerLines(ctx, c.Name, waitTailLines); err != nil {
			return
		}
		from = "docker logs " + c.Name
	} else {
		var err error
		lines, _, err = logs.Tail(logs.Query{Dir: a.engine.LogDir(), Name: target, N: waitTailLines})
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(a.Stdout, "(no log for %s)\n", target)
			return
		}
		if err != nil {
			return
		}
		from = a.engine.LogPaths(target).Log
		if rel := relPath(a.engine.Root(), from); rel != "" {
			from = rel
		}
	}
	if len(lines) == 0 {
		fmt.Fprintf(a.Stdout, "(%s is empty)\n", from)
		return
	}
	fmt.Fprintf(a.Stdout, "── last %d lines of %s ──\n", len(lines), from)
	for _, l := range lines {
		fmt.Fprintln(a.Stdout, l)
	}
}

// relPath returns path relative to root when inside it, "" otherwise.
func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return ""
	}
	return rel
}
