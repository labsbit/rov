package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labsbit/rov/internal/core"
)

func (a *App) cmdStatus(ctx context.Context, argv []string) error {
	ar, err := parseArgs(argv, fJSON)
	if err != nil {
		return err
	}
	if err := ar.exactly(0, ""); err != nil {
		return err
	}
	st, err := a.engine.Status(ctx)
	if err != nil {
		return err
	}
	if ar.has("--json") {
		return a.writeJSON(st)
	}
	writeStatus(a.Stdout, st, st.Time)
	return nil
}

// cmdWidthCap keeps one long command from pushing every row's tags (status)
// or descriptions (list) right.
const (
	cmdWidthCap     = 40
	listCmdWidthCap = 48
)

// writeStatus prints the status table: config processes, the ad-hoc
// section, unowned containers, then the warnings.
func writeStatus(w io.Writer, st *core.Status, now time.Time) {
	t := newTable(12, 12, 7, 12, 7)
	t.add("SERVICE", "STATE", "PID", "PORTS", "UPTIME", "COMMAND")

	// Tags only appear on config rows: align them there.
	cmdWidth := 0
	for i := range st.Processes {
		p := &st.Processes[i]
		if n := utf8.RuneCountInString(oneLine(p.Cmd)); p.Kind == core.KindConfig && n <= cmdWidthCap {
			cmdWidth = max(cmdWidth, n)
		}
	}
	adhoc := false
	for i := range st.Processes {
		p := &st.Processes[i]
		if p.Kind == core.KindAdhoc && !adhoc {
			adhoc = true
			t.line("ad-hoc")
		}
		t.add(p.Name, p.StateText(), pidCell(p), portsCell(p), fmtUptime(p.Uptime(now)), commandCell(p, cmdWidth))
	}
	if len(st.Docker.Unowned) > 0 {
		t.line("docker (containers no process owns)")
		for _, g := range st.Docker.Unowned {
			for i := range g.Containers {
				c := &g.Containers[i]
				t.add(g.Project+"/"+c.Service, containerState(c), "-", portsOfContainer(c), "-", "container "+c.Name)
			}
		}
	}
	t.write(w)
	if len(st.Processes) == 0 && len(st.Docker.Unowned) == 0 {
		fmt.Fprintln(w, "(no processes: rov init writes a starter rov.yaml; rov spawn <name> -- <cmd> starts an ad-hoc one)")
	}
	for _, warn := range st.Warnings {
		fmt.Fprintln(w, "! "+warn)
	}
}

// commandCell is the command, padded so that tags line up, plus
// [docker …], [changed] and [invalid] tags.
func commandCell(p *core.Process, width int) string {
	cmd := oneLine(p.Cmd)
	var tags []string
	if tag := dockerTag(p); tag != "" {
		tags = append(tags, tag)
	}
	if p.Changed {
		tags = append(tags, "[changed]")
	}
	if len(p.Problems) > 0 {
		tags = append(tags, "[invalid rov.yaml entry]")
	}
	if len(tags) == 0 {
		return cmd
	}
	pad := max(width-utf8.RuneCountInString(cmd), 0) + 1
	return cmd + strings.Repeat(" ", pad) + strings.Join(tags, " ")
}

func (a *App) cmdList(ctx context.Context, argv []string) error {
	ar, err := parseArgs(argv, fJSON)
	if err != nil {
		return err
	}
	if err := ar.exactly(0, ""); err != nil {
		return err
	}
	e := a.engine
	// Ad-hoc processes need tmux; without it, list what rov.yaml has.
	sessions, _ := e.Sessions(ctx)
	st := core.Compose(e.Inputs(sessions, nil, nil, nil))
	if ar.has("--json") {
		type entry struct {
			Name        string    `json:"name"`
			Kind        core.Kind `json:"kind"`
			Cmd         string    `json:"cmd"`
			Cwd         string    `json:"cwd"`
			Description string    `json:"description"`
			Docker      string    `json:"docker,omitempty"`
		}
		out := []entry{}
		for _, p := range st.Processes {
			out = append(out, entry{p.Name, p.Kind, p.Cmd, p.Cwd, p.Description, p.Docker})
		}
		return a.writeJSON(out)
	}
	writeList(a.Stdout, st)
	return nil
}

// writeList prints "name  cmd  description", config processes first.
func writeList(w io.Writer, st *core.Status) {
	t := newTable(12)
	cmdWidth := 0
	for _, p := range st.Processes {
		if n := utf8.RuneCountInString(oneLine(p.Cmd)); n <= listCmdWidthCap {
			cmdWidth = max(cmdWidth, n)
		}
	}
	adhoc := false
	for _, p := range st.Processes {
		if p.Kind == core.KindAdhoc && !adhoc {
			adhoc = true
			t.line("ad-hoc")
		}
		cmd := oneLine(p.Cmd)
		if p.Description != "" {
			cmd += strings.Repeat(" ", max(cmdWidth-utf8.RuneCountInString(cmd), 0)+2) + p.Description
		}
		t.add(p.Name, cmd)
	}
	t.write(w)
}
