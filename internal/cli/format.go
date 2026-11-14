package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/labsbit/rov/internal/core"
)

// table writes rows of left-aligned columns separated by one space. Each
// column is at least its minimum width and as wide as its widest cell; the
// last column isn't padded and trailing spaces are trimmed.
type table struct {
	min  []int
	rows [][]string
	// raw rows (section headers) are printed as they are, in order.
	raw map[int]string
}

func newTable(min ...int) *table { return &table{min: min, raw: map[int]string{}} }

func (t *table) add(cells ...string) { t.rows = append(t.rows, cells) }

// line adds a line printed verbatim, outside the columns.
func (t *table) line(s string) {
	t.raw[len(t.rows)] = s
	t.rows = append(t.rows, nil)
}

func (t *table) write(w io.Writer) {
	width := append([]int(nil), t.min...)
	for _, r := range t.rows {
		for i, c := range r {
			if i == len(r)-1 {
				continue
			}
			for len(width) <= i {
				width = append(width, 0)
			}
			width[i] = max(width[i], utf8.RuneCountInString(c))
		}
	}
	for i, r := range t.rows {
		if r == nil {
			fmt.Fprintln(w, t.raw[i])
			continue
		}
		var b strings.Builder
		for j, c := range r {
			b.WriteString(c)
			if j < len(r)-1 {
				b.WriteString(strings.Repeat(" ", width[j]-utf8.RuneCountInString(c)+1))
			}
		}
		fmt.Fprintln(w, strings.TrimRight(b.String(), " "))
	}
}

// fmtUptime formats a duration the way status shows it: 45s, 4m, 2h13m,
// 3d4h.
func fmtUptime(d time.Duration) string {
	switch {
	case d <= 0:
		return "-"
	case d < time.Minute:
		return strconv.Itoa(int(d/time.Second)) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
	return fmt.Sprintf("%dd%dh", int(d/(24*time.Hour)), int(d%(24*time.Hour)/time.Hour))
}

func joinInts(ns []int, sep string) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, sep)
}

// portsCell is the PORTS column: the ports now, else the last ports seen in
// parentheses, else "-".
func portsCell(p *core.Process) string {
	if ps := p.PortNumbers(); len(ps) > 0 {
		return joinInts(ps, ",")
	}
	if len(p.LastPorts) > 0 && !p.Running() {
		return "(" + joinInts(p.LastPorts, ",") + ")"
	}
	return "-"
}

func pidCell(p *core.Process) string {
	if p.PID > 0 {
		return strconv.Itoa(p.PID)
	}
	return "-"
}

// oneLine collapses a (possibly multi-line) command onto one line.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// dockerTag is "[docker 2/2 healthy]", "[docker 2/2 up]" or
// "[docker 1/2 degraded]"; "" without containers.
func dockerTag(p *core.Process) string {
	if len(p.Containers) == 0 {
		return ""
	}
	running, checked, healthy := 0, 0, 0
	for _, c := range p.Containers {
		if c.State == "running" {
			running++
		}
		if c.Health != "" {
			checked++
			if c.Health == "healthy" {
				healthy++
			}
		}
	}
	word := "degraded"
	if p.DockerState == "up" {
		word = "up"
		if checked > 0 && healthy == checked {
			word = "healthy"
		}
	}
	return fmt.Sprintf("[docker %d/%d %s]", running, len(p.Containers), word)
}

// containerState is a container's state with its health: "running healthy".
func containerState(c *core.Container) string {
	if c.Health != "" && c.State == "running" {
		return c.State + " " + c.Health
	}
	return c.State
}

func portsOfContainer(c *core.Container) string {
	var ns []int
	for _, p := range c.Ports {
		ns = append(ns, p.Port)
	}
	if len(ns) == 0 {
		return "-"
	}
	return joinInts(dedupe(ns), ",")
}

func dedupe(ns []int) []int {
	out := ns[:0:0]
	seen := map[int]bool{}
	for _, n := range ns {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
