package cli

import (
	"context"
	"io"
	"strconv"

	"github.com/labsbit/rov/internal/core"
)

func (a *App) cmdPorts(ctx context.Context, argv []string) error {
	ar, err := parseArgs(argv, fJSON)
	if err != nil {
		return err
	}
	if err := ar.exactly(0, ""); err != nil {
		return err
	}
	rows, _, err := a.engine.HostPorts(ctx)
	if err != nil {
		return err
	}
	if ar.has("--json") {
		if rows == nil {
			rows = []core.HostPort{}
		}
		return a.writeJSON(rows)
	}
	writePorts(a.Stdout, rows)
	return nil
}

// writePorts prints the `rov ports` table.
func writePorts(w io.Writer, rows []core.HostPort) {
	t := newTable(6, 11, 6, 19, 8)
	t.add("PORT", "ADDR", "PID", "COMMAND", "OWNER", "")
	for _, r := range rows {
		pid, comm, note := "-", r.Comm, ""
		switch {
		case r.Source == core.SourceDocker:
			if r.Owner == "" {
				note = "(container " + r.Container + ")"
			}
		case !r.Visible:
			pid, comm, note = "?", "?", "(not visible: other user)"
		default:
			pid = strconv.Itoa(r.PID)
		}
		t.add(strconv.Itoa(r.Port), r.Addr, pid, orDash(comm), orDash(r.Owner), note)
	}
	t.write(w)
}
