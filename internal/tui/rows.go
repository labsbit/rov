package tui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/labsbit/rov/internal/core"
)

// rowKind is what a row of the process list shows.
type rowKind int

const (
	rowProc      rowKind = iota // a process: two lines (one when narrow)
	rowContainer                // a container under its process or group
	rowSection                  // "── ad-hoc ──", "── docker ──"
	rowGroup                    // an unowned compose project (not selectable)
)

// row is one entry of the process list. Rows are rebuilt from every Status;
// the selection follows a row's key, so it doesn't jump when rows appear or
// disappear around it.
type row struct {
	kind rowKind
	// key identifies a selectable row: "p:<name>" for a process,
	// "c:<owner>/<service>" for an owned container, "u:<project>/<service>"
	// for an unowned one. Empty for rows that can't be selected.
	key   string
	proc  *core.Process   // rowProc, and the owner of an owned rowContainer
	ctr   *core.Container // rowContainer
	group string          // rowGroup and unowned rowContainer: the compose project
	last  bool            // rowContainer: last of its parent (└ instead of ├)
	title string          // rowSection
	cs    []core.Container
}

func (r *row) selectable() bool { return r.key != "" }

// target is how actions address a container: "<owner>/<service>".
func (r *row) target() string {
	if r.kind != rowContainer {
		if r.proc != nil {
			return r.proc.Name
		}
		return ""
	}
	owner := r.group
	if r.proc != nil {
		owner = r.proc.Name
	}
	return owner + "/" + r.ctr.Service
}

// buildRows lists st as the left pane shows it: config processes in config
// order, then the ad-hoc section, then a docker section for unowned
// containers; owned containers nest under their process.
func buildRows(st *core.Status) []row {
	if st == nil {
		return nil
	}
	var rows []row
	adhocSection := false
	for i := range st.Processes {
		p := &st.Processes[i]
		if p.Kind == core.KindAdhoc && !adhocSection {
			rows = append(rows, row{kind: rowSection, title: "ad-hoc"})
			adhocSection = true
		}
		rows = append(rows, row{kind: rowProc, key: "p:" + p.Name, proc: p})
		for j := range p.Containers {
			c := &p.Containers[j]
			rows = append(rows, row{kind: rowContainer, key: "c:" + p.Name + "/" + c.Service, proc: p, ctr: c, last: j == len(p.Containers)-1})
		}
	}
	if len(st.Docker.Unowned) > 0 {
		rows = append(rows, row{kind: rowSection, title: "docker"})
		for gi := range st.Docker.Unowned {
			g := &st.Docker.Unowned[gi]
			rows = append(rows, row{kind: rowGroup, group: g.Project, cs: g.Containers})
			for j := range g.Containers {
				c := &g.Containers[j]
				rows = append(rows, row{kind: rowContainer, key: "u:" + g.Project + "/" + c.Service, group: g.Project, ctr: c, last: j == len(g.Containers)-1})
			}
		}
	}
	return rows
}

// reselect returns the index of key in rows, or — when that row is gone —
// the selectable row nearest to where it was (prev is its old index): the
// one that took its place, else the one above. -1 if nothing is selectable.
func reselect(rows []row, key string, prev int) int {
	if key != "" {
		for i := range rows {
			if rows[i].key == key {
				return i
			}
		}
	}
	if len(rows) == 0 {
		return -1
	}
	prev = max(0, min(prev, len(rows)-1))
	if rows[prev].selectable() {
		return prev
	}
	for i := prev - 1; i >= 0; i-- {
		if rows[i].selectable() {
			return i
		}
	}
	for i := prev + 1; i < len(rows); i++ {
		if rows[i].selectable() {
			return i
		}
	}
	return -1
}

// look is how a state is drawn: glyph, colour role and a short word.
type look struct {
	glyph string
	role  role
	word  string
}

type role int

const (
	roleNone role = iota
	roleAccent
	roleRunning
	roleWarn
	roleFailed
	roleDone
	roleIdle
)

// procLook is the glyph and state word of a process row. A process that
// owns containers shows their state (up N/N, degraded n/N) instead of a
// bare "exited": `infra` runs `compose up --wait` and exits 0.
func procLook(p *core.Process) look {
	if len(p.Problems) > 0 && !p.Running() {
		return look{"✗", roleFailed, "invalid"}
	}
	switch {
	case p.DockerState == "up":
		return look{"●", roleRunning, fmt.Sprintf("up %d/%d", len(p.Containers), len(p.Containers))}
	case strings.HasPrefix(p.DockerState, "degraded"):
		return look{"▲", roleWarn, p.DockerState}
	}
	switch p.State {
	case core.StateRunning:
		return look{"●", roleRunning, ""}
	case core.StateStopped:
		return look{"○", roleIdle, "stopped"}
	case core.StateExited:
		return look{"✓", roleDone, "exited"}
	case core.StateCrashed:
		code := ""
		if p.ExitCode != nil {
			code = " " + strconv.Itoa(*p.ExitCode)
		}
		return look{"✗", roleFailed, "crashed" + code}
	case core.StateDead:
		if p.Signal > 0 {
			return look{"✗", roleFailed, "killed " + strconv.Itoa(p.Signal)}
		}
		return look{"✗", roleFailed, "dead"}
	case core.StateAbsent:
		return look{"○", roleIdle, "absent"}
	}
	return look{"○", roleIdle, ""}
}

// ctrLook is the state word of a container sub-row.
func ctrLook(c *core.Container) look {
	switch {
	case c.State == "running" && c.Health == "healthy":
		return look{"●", roleRunning, "healthy"}
	case c.State == "running" && c.Health == "unhealthy":
		return look{"✗", roleFailed, "unhealthy"}
	case c.State == "running" && c.Health == "starting":
		return look{"▲", roleWarn, "starting"}
	case c.State == "running":
		return look{"●", roleRunning, "running"}
	case c.State == "restarting":
		return look{"▲", roleWarn, "restarting"}
	case c.State == "dead":
		return look{"✗", roleFailed, "dead"}
	case c.State == "":
		return look{"○", roleIdle, "unknown"}
	}
	return look{"○", roleIdle, c.State}
}

// groupLook summarises an unowned compose project.
func groupLook(cs []core.Container) look {
	up := 0
	for i := range cs {
		if cs[i].Up() {
			up++
		}
	}
	switch {
	case up == len(cs) && up > 0:
		return look{"●", roleRunning, fmt.Sprintf("up %d/%d", up, len(cs))}
	case up > 0:
		return look{"▲", roleWarn, fmt.Sprintf("degraded %d/%d", up, len(cs))}
	}
	return look{"○", roleIdle, "down"}
}

// counts are the state totals of the top bar.
type counts struct{ up, degraded, failed, idle, done int }

func countStates(st *core.Status) counts {
	var c counts
	if st == nil {
		return c
	}
	for i := range st.Processes {
		switch procLook(&st.Processes[i]).role {
		case roleRunning:
			c.up++
		case roleWarn:
			c.degraded++
		case roleFailed:
			c.failed++
		case roleDone:
			c.done++
		default:
			c.idle++
		}
	}
	return c
}

// fmtDur is a compact duration: 42s, 4m, 2h13m, 3d4h.
func fmtDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%dd%dh", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
}

// fmtPorts is ":8080 :8081".
func fmtPorts(ps []int) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = ":" + strconv.Itoa(p)
	}
	return strings.Join(parts, " ")
}

func ctrPorts(c *core.Container) []int {
	var out []int
	for _, p := range c.Ports {
		if len(out) == 0 || out[len(out)-1] != p.Port {
			out = append(out, p.Port)
		}
	}
	return out
}
