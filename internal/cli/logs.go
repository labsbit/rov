package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"

	"github.com/labsbit/rov/internal/core"
	"github.com/labsbit/rov/internal/logs"
)

const defaultLogLines = 80

func (a *App) cmdLogs(ctx context.Context, argv []string) error {
	ar, err := parseArgs(argv, fJSON,
		flagDef{names: []string{"-n", "--lines"}, value: true},
		flagDef{names: []string{"-f", "--follow"}},
		flagDef{names: []string{"--grep"}, value: true},
		flagDef{names: []string{"--prev"}},
		flagDef{names: []string{"--color", "--colour"}},
	)
	if err != nil {
		return err
	}
	if err := ar.exactly(1, "process name (try: rov list)"); err != nil {
		return err
	}
	n, err := ar.int("-n", defaultLogLines)
	if err != nil {
		return err
	}
	var re *regexp.Regexp
	if ar.has("--grep") {
		if re, err = regexp.Compile(ar.str("--grep")); err != nil {
			return usagef("--grep: %v", err)
		}
	}
	if ar.has("-f") && ar.has("--json") {
		return usagef("-f and --json don't combine")
	}
	name := ar.pos[0]
	if ref, ok := core.ParseTarget(name); ok {
		if ar.has("--prev") {
			return usagef("--prev reads rov's own logs; docker keeps one log per container")
		}
		return a.containerLogs(ctx, ref, n, re, ar.has("-f"), ar.has("--json"))
	}

	e := a.engine
	if err := validName(name); err != nil {
		return err
	}
	if !e.KnownName(ctx, name) {
		return fmt.Errorf("unknown process %q (try: rov list)", name)
	}
	run := logs.Current
	if ar.has("--prev") {
		run = logs.Prev
	}
	q := logs.Query{Dir: e.LogDir(), Name: name, Run: run, N: n, Grep: re, ANSI: ar.has("--color")}

	if ar.has("-f") {
		return a.follow(ctx, q)
	}
	lines, total, err := logs.Tail(q)
	if errors.Is(err, fs.ErrNotExist) {
		if run == logs.Prev {
			return fmt.Errorf("no previous run of %s (rov keeps the run before the last start)", name)
		}
		return fmt.Errorf("no log yet for %s (start it: rov up %s)", name, name)
	}
	if err != nil {
		return err
	}
	if ar.has("--json") {
		state := string(core.StateAbsent)
		if st, err := e.Status(ctx); err == nil {
			if p := st.Get(name); p != nil {
				state = string(p.State)
			}
		}
		if lines == nil {
			lines = []string{}
		}
		return a.writeJSON(struct {
			Name       string   `json:"name"`
			State      string   `json:"state"`
			Run        string   `json:"run"`
			Lines      []string `json:"lines"`
			TotalLines int      `json:"total_lines"`
		}{name, state, run.String(), lines, total})
	}
	for _, l := range lines {
		fmt.Fprintln(a.Stdout, l)
	}
	return nil
}

// follow prints lines as they arrive until interrupted (humans only).
func (a *App) follow(ctx context.Context, q logs.Query) error {
	out := make(chan string, 64)
	errc := make(chan error, 1)
	go func() { errc <- logs.Follow(ctx, q, out) }()
	for {
		select {
		case l := <-out:
			fmt.Fprintln(a.Stdout, l)
		case err := <-errc:
			if errors.Is(err, context.Canceled) {
				return nil
			}
			return err
		}
	}
}

// containerLogs runs docker logs for a container target.
func (a *App) containerLogs(ctx context.Context, ref core.ContainerRef, n int, re *regexp.Regexp, follow, asJSON bool) error {
	st, err := a.engine.Status(ctx)
	if err != nil {
		return err
	}
	c, _, err := core.FindContainer(st, ref)
	if err != nil {
		return err
	}
	tail := n
	if re != nil || n == 0 {
		tail = -1 // filter (or show) everything, then cut
	}
	rc, err := a.engine.Docker.Logs(ctx, c.Name, tail, follow)
	if err != nil {
		return err
	}
	defer rc.Close()
	if follow {
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			if re == nil || re.MatchString(sc.Text()) {
				fmt.Fprintln(a.Stdout, sc.Text())
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		return rc.Close()
	}
	lines, err := readLines(rc, re, n)
	if err != nil {
		return err
	}
	if err := rc.Close(); err != nil {
		return err
	}
	if asJSON {
		return a.writeJSON(struct {
			Name       string   `json:"name"`
			State      string   `json:"state"`
			Run        string   `json:"run"`
			Lines      []string `json:"lines"`
			TotalLines int      `json:"total_lines"`
		}{ref.String(), c.State, "current", lines, len(lines)})
	}
	for _, l := range lines {
		fmt.Fprintln(a.Stdout, l)
	}
	return nil
}

// dockerLines returns the last n lines of a container's log.
func (a *App) dockerLines(ctx context.Context, container string, n int) ([]string, error) {
	if n <= 0 {
		return []string{}, nil
	}
	rc, err := a.engine.Docker.Logs(ctx, container, n, false)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return readLines(rc, nil, n)
}

// readLines reads r, keeps the lines matching re (all if nil) and returns
// the last n of them (all if n is 0).
func readLines(r io.Reader, re *regexp.Regexp, n int) ([]string, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	lines := []string{}
	for sc.Scan() {
		l := strings.TrimRight(sc.Text(), "\r")
		if re != nil && !re.MatchString(l) {
			continue
		}
		lines = append(lines, l)
		if n > 0 && len(lines) > 2*n {
			lines = append(lines[:0], lines[len(lines)-n:]...)
		}
	}
	if n > 0 && len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, sc.Err()
}
