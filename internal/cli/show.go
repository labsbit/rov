package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"

	"github.com/labsbit/rov/internal/core"
	"github.com/labsbit/rov/internal/logs"
)

const defaultShowLines = 30

func (a *App) cmdShow(ctx context.Context, argv []string) error {
	ar, err := parseArgs(argv, fJSON, flagDef{names: []string{"-n", "--lines"}, value: true})
	if err != nil {
		return err
	}
	if err := ar.exactly(1, "process name (try: rov list)"); err != nil {
		return err
	}
	n, err := ar.int("-n", defaultShowLines)
	if err != nil {
		return err
	}
	name := ar.pos[0]
	st, err := a.engine.Status(ctx)
	if err != nil {
		return err
	}
	if ref, ok := core.ParseTarget(name); ok {
		return a.showContainer(ctx, st, ref, n, ar.has("--json"))
	}
	p := st.Get(name)
	if p == nil {
		return fmt.Errorf("unknown process %q (try: rov list)", name)
	}
	lines, _, err := logs.Tail(logs.Query{Dir: a.engine.LogDir(), Name: name, N: n})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if lines == nil {
		lines = []string{}
	}
	if ar.has("--json") {
		return a.writeJSON(withField(*p, "lines", lines))
	}
	writeShow(a.Stdout, p, st.Time, a.Loc)
	if n > 0 {
		fmt.Fprintf(a.Stdout, "\n── last %d lines of %s ──\n", n, p.Log)
		for _, l := range lines {
			fmt.Fprintln(a.Stdout, l)
		}
	}
	return nil
}

// writeShow prints the fields of one process, one "key  value" per line.
func writeShow(w io.Writer, p *core.Process, now time.Time, loc *time.Location) {
	t := newTable(12)
	t.add("name", p.Name)
	if p.Description != "" {
		t.add("description", p.Description)
	}
	t.add("kind", string(p.Kind))
	state := p.StateText()
	if p.DockerState != "" {
		state += " · docker " + p.DockerState
	}
	if p.Signal > 0 {
		state += fmt.Sprintf(" (signal %d)", p.Signal)
	}
	t.add("state", state)
	t.add("pid", pidCell(p))
	if up := p.Uptime(now); up > 0 {
		t.add("uptime", fmtUptime(up))
	}
	if !p.StartedAt.IsZero() {
		t.add("started", p.StartedAt.In(loc).Format("2006-01-02 15:04:05 MST"))
	}
	if len(p.Ports) == 0 {
		t.add("ports", "-")
	}
	for i, pt := range p.Ports {
		key := ""
		if i == 0 {
			key = "ports"
		}
		t.add(key, portText(pt))
	}
	t.add("last ports", orDash(joinInts(p.LastPorts, ",")))
	t.add("cmd", oneLine(p.Cmd))
	if p.Changed {
		t.add("running cmd", oneLine(p.RunningCmd)+"  (rov.yaml changed: restart to apply)")
	}
	t.add("cwd", p.Cwd)
	t.add("env", orDash(strings.Join(p.EnvKeys, ", ")))
	t.add("log", p.Log)
	if p.Docker != "" {
		t.add("docker", p.Docker)
	}
	if len(p.Containers) > 0 {
		var b strings.Builder
		ct := newTable()
		for i := range p.Containers {
			c := &p.Containers[i]
			ct.add(p.Name+"/"+c.Service, c.Name, containerState(c), portsOfContainer(c))
		}
		ct.write(&b)
		for i, line := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
			key := ""
			if i == 0 {
				key = "containers"
			}
			t.add(key, line)
		}
	}
	for i, pr := range p.Problems {
		key := ""
		if i == 0 {
			key = "problems"
		}
		t.add(key, pr)
	}
	t.write(w)
}

// portText is "8080 0.0.0.0 pid 41301 (api-gateway)" or "6379 127.0.0.1
// docker sag-redis".
func portText(p core.Port) string {
	if p.Source == core.SourceDocker {
		return fmt.Sprintf("%d %s docker %s", p.Port, p.Addr, p.Container)
	}
	return fmt.Sprintf("%d %s pid %d (%s)", p.Port, p.Addr, p.PID, p.Comm)
}

func (a *App) showContainer(ctx context.Context, st *core.Status, ref core.ContainerRef, n int, asJSON bool) error {
	c, _, err := core.FindContainer(st, ref)
	if err != nil {
		return err
	}
	lines, err := a.dockerLines(ctx, c.Name, n)
	if err != nil {
		return err
	}
	if asJSON {
		return a.writeJSON(withField(*c, "lines", lines))
	}
	t := newTable(12)
	t.add("name", ref.String())
	t.add("container", c.Name)
	t.add("project", c.Project)
	t.add("state", containerState(c))
	t.add("status", c.Status)
	if len(c.Ports) == 0 {
		t.add("ports", "-")
	}
	for i, p := range c.Ports {
		key := ""
		if i == 0 {
			key = "ports"
		}
		t.add(key, fmt.Sprintf("%d %s", p.Port, p.Addr))
	}
	t.write(a.Stdout)
	if n > 0 {
		fmt.Fprintf(a.Stdout, "\n── last %d lines of docker logs %s ──\n", n, c.Name)
		for _, l := range lines {
			fmt.Fprintln(a.Stdout, l)
		}
	}
	return nil
}
