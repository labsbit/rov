package tui

import (
	"image/color"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/labsbit/rov/internal/core"
)

// Breakpoints of the narrow layouts.
const (
	narrowTop  = 100 // below: the top bar drops the path and the socket
	narrowList = 80  // below: list rows drop their second line
	singlePane = 50  // below: one pane at a time
	listMax    = 40  // outer width of the list pane at most…
	listShare  = 40  // …and at most this percentage of the width
)

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// layout is the geometry of one frame.
type layout struct {
	single  bool // one pane at a time
	leftW   int  // outer width of the list pane (0 when hidden)
	rightW  int  // outer width of the output pane (0 when hidden)
	paneH   int  // outer height of the panes
	twoLine bool // process rows have their command line
}

func (m *Model) layout() layout {
	l := layout{paneH: max(0, m.height-2), twoLine: m.width >= narrowList}
	if m.width < singlePane {
		l.single = true
		if m.focus == focusList {
			l.leftW = m.width
		} else {
			l.rightW = m.width
		}
		return l
	}
	l.leftW = min(listMax, m.width*listShare/100)
	l.rightW = m.width - l.leftW
	return l
}

// View renders the TUI.
func (m *Model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "rov ▸ " + m.b.Name()
	return v
}

func (m *Model) render() string {
	if m.width <= 0 || m.height <= 0 {
		return ""
	}
	l := m.layout()
	var b strings.Builder
	b.WriteString(m.topBar())
	if l.paneH >= 3 {
		var left, right []string
		if l.leftW > 0 {
			left = m.listPane(l)
		}
		if l.rightW > 0 {
			right = m.outputPane(l)
		}
		for i := range l.paneH {
			b.WriteByte('\n')
			if left != nil {
				b.WriteString(left[i])
			}
			if right != nil {
				b.WriteString(right[i])
			}
		}
	}
	if m.height > 1 {
		b.WriteByte('\n')
		b.WriteString(m.bottomBar())
	}
	base := b.String()
	switch {
	case m.form != nil:
		return m.overlay(base, m.form.view())
	case m.help:
		return m.overlay(base, m.helpView())
	}
	return base
}

// overlay draws box centred over base, measuring with the renderer's width
// method (Lip Gloss's compositor always counts graphemes).
func (m *Model) overlay(base, box string) string {
	rows := strings.Split(box, "\n")
	bw := 0
	for _, r := range rows {
		bw = max(bw, m.cw.width(r))
	}
	bw = min(bw, m.width)
	lines := strings.Split(base, "\n")
	x, y := max(0, (m.width-bw)/2), max(0, (len(lines)-len(rows))/2)
	for i, r := range rows {
		if y+i < len(lines) {
			lines[y+i] = m.cw.splice(lines[y+i], r, x, bw)
		}
	}
	return strings.Join(lines, "\n")
}

// seg is a styled piece of a line.
type seg struct {
	text string
	st   lipgloss.Style
}

// line builds one row of exactly w cells: segments in order, then padding,
// all on bg when set (the selected row).
type line struct {
	bg   color.Color
	segs []seg
}

func (l *line) add(text string, st lipgloss.Style) { l.segs = append(l.segs, seg{text, st}) }

func (l *line) width(c cells) int {
	n := 0
	for _, s := range l.segs {
		n += c.width(s.text)
	}
	return n
}

func (l *line) render(c cells, w int) string {
	var b strings.Builder
	n := 0
	for _, s := range l.segs {
		text := s.text
		if sw := c.width(text); n+sw > w {
			text = c.trunc(text, w-n, "")
		}
		n += c.width(text)
		st := s.st
		if l.bg != nil {
			st = st.Background(l.bg)
		}
		b.WriteString(st.Render(text))
		if n >= w {
			break
		}
	}
	if n < w {
		pad := lipgloss.NewStyle()
		if l.bg != nil {
			pad = pad.Background(l.bg)
		}
		b.WriteString(pad.Render(strings.Repeat(" ", w-n)))
	}
	return b.String()
}

// box frames inner lines (already w-2 wide) with a rounded border and a
// title in the top edge.
func (m *Model) box(title string, inner []string, w, h int, focused bool) []string {
	bc := m.sty.idle
	tst := m.sty.idle
	if focused {
		bc = m.sty.accent
		tst = lipgloss.NewStyle().Bold(true)
	}
	iw := max(0, w-2)
	title = m.cw.fit(title, max(0, iw-4))
	fill := max(0, iw-3-m.cw.width(title))
	top := bc.Render("╭─ ") + tst.Render(title) + bc.Render(" "+strings.Repeat("─", fill)+"╮")
	if iw < 4 {
		top = bc.Render("╭" + strings.Repeat("─", iw) + "╮")
	}
	out := []string{top}
	side := bc.Render("│")
	for i := 0; i < h-2; i++ {
		s := ""
		if i < len(inner) {
			s = inner[i]
		}
		if sw := m.cw.width(s); sw < iw {
			s += strings.Repeat(" ", iw-sw)
		} else if sw > iw {
			s = m.cw.trunc(s, iw, "")
		}
		out = append(out, side+s+side)
	}
	out = append(out, bc.Render("╰"+strings.Repeat("─", iw)+"╯"))
	return out
}

// topBar: rov ▸ project, problems, path, socket, docker trouble, state
// counts, clock. When it doesn't fit, the path is shortened in the middle,
// then the docker note, the socket and the path are dropped in that order.
// Below 100 columns path and socket aren't shown at all.
func (m *Model) topBar() string {
	s := m.sty
	w := m.width
	c := countStates(m.st)
	counts := lipgloss.NewStyle()
	right := []string{
		s.running.Render("●") + counts.Render(" "+strconv.Itoa(c.up)),
		s.warn.Render("▲") + counts.Render(" "+strconv.Itoa(c.degraded)),
		s.failed.Render("✗") + counts.Render(" "+strconv.Itoa(c.failed)),
		s.idle.Render("○") + counts.Render(" "+strconv.Itoa(c.idle)),
	}
	if c.done > 0 {
		right = append(right, s.done.Render("✓")+counts.Render(" "+strconv.Itoa(c.done)))
	}
	clock := m.now.Format("15:04:05")
	rightStr := strings.Join(right, "  ") + "   " + s.idle.Render(clock) + " "
	if w < narrowList {
		// compact: "●5 ▲0 ✗1 ○2"
		for i, r := range right {
			right[i] = strings.Replace(r, " ", "", 1)
		}
		rightStr = strings.Join(right, " ") + "  " + s.idle.Render(clock) + " "
		if w < 40 {
			rightStr = s.idle.Render(clock) + " "
		}
	}
	room := w - m.cw.width(rightStr) - 3 // a space at the start, two before the counts

	type item struct {
		text string
		st   lipgloss.Style
		drop int // 0: always shown; higher is dropped first
	}
	const sep = 3
	items := []item{{"rov ▸ " + m.b.Name(), s.bold, 0}}
	if problem := m.problem(); problem != "" {
		items = append(items, item{problem, s.failed, 0})
	}
	path := -1
	if w >= narrowTop {
		path = len(items)
		items = append(items, item{shortPath(m.b.Root(), m.opts.Home), s.idle, 2}, item{"tmux " + m.b.Socket(), s.idle, 3})
	}
	if d := m.dockerNote(); d != "" {
		items = append(items, item{d, s.idle, 4})
	}
	widthOf := func(its []item) int {
		n := 0
		for i, it := range its {
			if i > 0 {
				n += sep
			}
			n += m.cw.width(it.text)
		}
		return n
	}
	const minPath = 12
	for {
		over := widthOf(items) - room
		if over <= 0 {
			break
		}
		// shorten the path first, as long as it stays readable
		if path >= 0 && path < len(items) && items[path].drop == 2 {
			if pw := m.cw.width(items[path].text); pw-over >= minPath {
				items[path].text = m.cw.middle(items[path].text, pw-over)
				break
			}
		}
		worst := -1
		for i, it := range items {
			if it.drop > 0 && (worst < 0 || it.drop > items[worst].drop) {
				worst = i
			}
		}
		if worst < 0 {
			break
		}
		if worst < path {
			path--
		} else if worst == path {
			path = -1
		}
		items = append(items[:worst], items[worst+1:]...)
		if path >= 0 {
			items[path].text = shortPath(m.b.Root(), m.opts.Home)
		}
	}
	var b strings.Builder
	b.WriteString(" ")
	for i, it := range items {
		if i > 0 {
			b.WriteString(strings.Repeat(" ", sep))
		}
		b.WriteString(it.st.Render(it.text))
	}
	l := m.cw.fit(b.String(), room+1)
	return l + strings.Repeat(" ", max(0, w-m.cw.width(rightStr)-m.cw.width(l))) + rightStr
}

// dockerNote is a failed docker poll, dimly in the top bar: the containers
// shown are from the last poll that worked. Nothing when docker doesn't
// matter here (no container shown, no process owns any).
func (m *Model) dockerNote() string {
	if m.dockerErr == nil {
		return ""
	}
	relevant := len(m.containers) > 0
	if m.st != nil {
		for i := range m.st.Processes {
			if m.st.Processes[i].Docker != "" {
				relevant = true
			}
		}
	}
	if !relevant {
		return ""
	}
	return "docker: " + errText(m.dockerErr)
}

// problem is the most important thing wrong with the sources, for the top
// bar: a broken rov.yaml, tmux failing.
func (m *Model) problem() string {
	if err := m.b.ConfigErr(); err != nil {
		return "✗ rov.yaml: " + strings.TrimPrefix(firstLine(err.Error()), m.b.ConfigPath()+": ")
	}
	if m.tmuxErr != nil {
		return "✗ tmux: " + errText(m.tmuxErr)
	}
	return ""
}

// middle shortens s to w cells by replacing its middle with "…", keeping
// more of the end (the project's own directory).
func (c cells) middle(s string, w int) string {
	n := c.width(s)
	if n <= w {
		return s
	}
	if w < 3 {
		return c.fit(s, w)
	}
	head := (w - 1) / 3
	tail := w - 1 - head
	return c.trunc(s, head, "") + "…" + c.method().TruncateLeft(s, n-tail, "")
}

func shortPath(p, home string) string {
	if home != "" && (p == home || strings.HasPrefix(p, home+"/")) {
		return "~" + p[len(home):]
	}
	return p
}

// listLine is one line of the list pane: a row's first or second line.
type listLine struct {
	row int
	sub int
}

func (m *Model) listLines(l layout) []listLine {
	var out []listLine
	for i := range m.rows {
		out = append(out, listLine{i, 0})
		if m.rows[i].kind == rowProc && l.twoLine {
			out = append(out, listLine{i, 1})
		}
	}
	return out
}

// listPane renders the process list.
func (m *Model) listPane(l layout) []string {
	iw, ih := l.leftW-2, l.paneH-2
	lines := m.listLines(l)
	// keep the selected row in view
	first, last := -1, -1
	for i, ln := range lines {
		if ln.row == m.selIdx {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first >= 0 {
		if first < m.listTop {
			m.listTop = first
		}
		if last >= m.listTop+ih {
			m.listTop = last - ih + 1
		}
	}
	m.listTop = max(0, min(m.listTop, len(lines)-ih))
	nameW := m.nameWidth(iw)
	var inner []string
	for i := m.listTop; i < len(lines) && len(inner) < ih; i++ {
		inner = append(inner, m.listRow(lines[i], iw, nameW))
	}
	if len(m.rows) == 0 {
		msg := "no processes"
		if !m.b.HasConfig() {
			msg = "no rov.yaml — n adds a process"
		}
		inner = append(inner, " "+m.sty.idle.Render(m.cw.fit(msg, iw-1)))
	}
	return m.box("processes", inner, l.leftW, l.paneH, m.focus == focusList)
}

// nameWidth is the shared width of the name column.
func (m *Model) nameWidth(iw int) int {
	n := 4
	for i := range m.rows {
		switch r := &m.rows[i]; r.kind {
		case rowProc:
			n = max(n, m.cw.width(r.proc.Name))
		case rowContainer:
			n = max(n, m.cw.width(r.ctr.Service)+2)
		}
	}
	return min(n, max(6, iw-11))
}

func (m *Model) listRow(ln listLine, iw, nameW int) string {
	s := m.sty
	r := &m.rows[ln.row]
	sel := ln.row == m.selIdx
	var L line
	if sel {
		L.bg = s.p.selection
	}
	bar := func() {
		if sel {
			L.add("▌", s.accent)
		} else {
			L.add(" ", lipgloss.NewStyle())
		}
	}
	right := func(text string, st lipgloss.Style, used int) {
		room := iw - used - 1
		if text == "" || room < 2 {
			return
		}
		text = m.cw.fitPorts(text, room-1)
		gap := iw - 1 - used - m.cw.width(text)
		if gap < 1 || text == "" {
			return
		}
		L.add(strings.Repeat(" ", gap), lipgloss.NewStyle())
		L.add(text, st)
	}
	nameSt := lipgloss.NewStyle()
	if sel {
		nameSt = nameSt.Bold(true)
	}
	switch r.kind {
	case rowSection:
		L.add(" ── "+r.title+" ", s.idle)
		L.add(strings.Repeat("─", max(0, iw-5-m.cw.width(r.title)-1)), s.idle)
	case rowGroup:
		lk := groupLook(r.cs)
		L.add(" ", lipgloss.NewStyle())
		L.add(lk.glyph, s.color(lk.role))
		gw := nameW
		if n := m.cw.width(r.group); n > gw {
			gw = max(4, min(n, iw-4-m.cw.width(lk.word)))
		}
		L.add(" "+m.cw.pad(m.cw.fit(r.group, gw), gw)+" ", s.idle)
		L.add(lk.word, s.color(lk.role))
	case rowContainer:
		bar()
		branch := "├ "
		if r.last {
			branch = "└ "
		}
		L.add("  ", lipgloss.NewStyle())
		L.add(branch, s.idle)
		L.add(m.cw.pad(m.cw.fit(r.ctr.Service, nameW-2), nameW-2)+" ", nameSt)
		lk := ctrLook(r.ctr)
		if p := m.pending[r.target()]; p != nil {
			lk = look{spinner[m.spin%len(spinner)], roleWarn, p.verb + "…"}
		}
		L.add(lk.word, s.color(lk.role))
		right(fmtPorts(ctrPorts(r.ctr)), s.accent, L.width(m.cw))
	case rowProc:
		p := r.proc
		if ln.sub == 0 {
			bar()
			lk := procLook(p)
			if pd := m.pending[p.Name]; pd != nil {
				lk = look{spinner[m.spin%len(spinner)], roleWarn, pd.verb + "…"}
			}
			L.add(lk.glyph, s.color(lk.role))
			// the name column, unless the state word wouldn't fit after it
			nw := nameW
			if ww := m.cw.width(lk.word); ww > 0 && 3+nw+1+ww > iw {
				nw = max(4, min(m.cw.width(p.Name), iw-4-ww))
			}
			L.add(" "+m.cw.pad(m.cw.fit(p.Name, nw), nw), nameSt)
			if lk.word != "" {
				L.add(" "+lk.word, s.color(lk.role))
			}
			if ps := p.PortNumbers(); len(ps) > 0 {
				right(fmtPorts(ps), s.accent, L.width(m.cw))
			}
			break
		}
		bar()
		L.add("  ", lipgloss.NewStyle())
		tag, tagSt := "", s.idle
		switch {
		case p.Changed:
			tag, tagSt = "◆ changed", s.warn
		case !p.Running() && len(p.Ports) == 0 && len(p.LastPorts) > 0:
			tag = "(" + fmtPorts(p.LastPorts) + ")"
		case p.Kind == core.KindAdhoc && p.Running():
			tag = fmtDur(p.Uptime(m.now))
		}
		room := iw - 3 - 1
		if tag != "" {
			room -= m.cw.width(tag) + 2
		}
		L.add(m.cw.fit(p.Cmd, max(1, room)), s.idle)
		right(tag, tagSt, L.width(m.cw))
	}
	return L.render(m.cw, iw)
}

// fitPorts keeps whole ports that fit in w, marking dropped ones with "…".
func (c cells) fitPorts(text string, w int) string {
	if c.width(text) <= w {
		return text
	}
	if !strings.HasPrefix(text, ":") {
		return c.fit(text, w)
	}
	parts := strings.Fields(text)
	for n := len(parts) - 1; n > 0; n-- {
		s := strings.Join(parts[:n], " ") + " …"
		if c.width(s) <= w {
			return s
		}
	}
	return ""
}

// outputPane renders the header and the output of the selected row.
func (m *Model) outputPane(l layout) []string {
	iw, ih := l.rightW-2, l.paneH-2
	r := m.selected()
	title := "output"
	var inner []string
	if r != nil {
		title = r.target()
		if r.kind == rowProc {
			title = r.proc.Name
		}
	}
	o := m.outs[m.active]
	if o != nil && o.prev {
		title += " · previous run"
	}
	if r == nil {
		return m.box(title, nil, l.rightW, l.paneH, m.focus == focusOutput)
	}
	h1, h2 := m.header(r)
	inner = append(inner, " "+m.cw.fit(h1, iw-2), " "+m.cw.fit(h2, iw-2), m.sty.idle.Render(strings.Repeat("─", iw)))
	rowsH := max(0, ih-3)
	if o == nil {
		return m.box(title, inner, l.rightW, l.paneH, m.focus == focusOutput)
	}
	v := m.outView(o, iw, rowsH)
	rows, lineOf := o.visible(v)
	gw := m.gutterWidth(o)
	curLine := -1
	if o.cur >= 0 && o.cur < len(o.matches) {
		curLine = o.matches[o.cur].line
	}
	for i, row := range rows {
		g := " "
		if m.nums {
			num := ""
			if lineOf[i] >= 0 && !o.isMarker(lineOf[i]) {
				num = strconv.Itoa(lineOf[i] + 1)
			}
			st := m.sty.idle
			if lineOf[i] >= 0 && lineOf[i] == curLine {
				st = m.sty.accent.Bold(true)
			}
			g = " " + st.Render(strings.Repeat(" ", gw-3-len(num))+num) + "  "
		}
		inner = append(inner, g+row)
	}
	if len(rows) == 0 && rowsH > 0 {
		inner = append(inner, "  "+m.emptyText(r, o))
	}
	// markers at the right end of the first or last output row
	mark := func(idx int, text string) {
		for len(inner) <= idx {
			inner = append(inner, "")
		}
		text = " " + text + " "
		keep := iw - m.cw.width(text) - 1
		base, _ := m.cw.upTo(inner[idx], max(0, keep))
		inner[idx] = m.cw.pad(base+sgrReset, keep) + m.sty.accent.Bold(true).Render(text) + " "
	}
	if o.trimmed && !o.follow && rowsH > 1 {
		mark(3, "↑ older lines left the scrollback")
	}
	if n := o.newLines(); n > 0 && rowsH > 0 {
		marker := "↓ " + strconv.Itoa(n) + " new line"
		if n != 1 {
			marker += "s"
		}
		mark(3+rowsH-1, marker)
	}
	return m.box(title, inner, l.rightW, l.paneH, m.focus == focusOutput)
}

// gutterWidth is the cells taken by line numbers: " 1204  ".
func (m *Model) gutterWidth(o *outState) int {
	if !m.nums {
		return 1
	}
	return max(4, len(strconv.Itoa(o.total()))) + 3
}

// outView is the output area of o for a pane inner width iw.
func (m *Model) outView(o *outState, iw, h int) view {
	return view{width: max(1, iw-m.gutterWidth(o)-1), height: h, wrap: m.wrap, sty: m.sty, cw: m.cw}
}

// curView is the output area of the active output in the current layout.
func (m *Model) curView(o *outState) view {
	l := m.layout()
	w := l.rightW
	if w == 0 {
		w = m.width
	}
	return m.outView(o, w-2, max(1, l.paneH-5))
}

func (m *Model) emptyText(r *row, o *outState) string {
	s := m.sty
	switch {
	case o.err != "":
		return s.failed.Render(o.err)
	case o.prev:
		return s.idle.Render("no previous run")
	case r.kind == rowContainer:
		return s.idle.Render("no output (docker logs " + r.ctr.Name + ")")
	case r.proc != nil && !r.proc.HasSession:
		return s.idle.Render("not running — space starts it")
	}
	return s.idle.Render("no output yet")
}

// header returns the two header lines of the output pane.
func (m *Model) header(r *row) (string, string) {
	s := m.sty
	sep := s.idle.Render(" · ")
	var p1, p2 []string
	if r.kind == rowContainer {
		c := r.ctr
		lk := ctrLook(c)
		p1 = append(p1, s.color(lk.role).Render(lk.glyph+" "+lk.word))
		if ps := ctrPorts(c); len(ps) > 0 {
			p1 = append(p1, s.accent.Render(fmtPorts(ps)))
		}
		p1 = append(p1, c.Name)
		if c.Status != "" {
			p1 = append(p1, s.idle.Render(c.Status))
		}
		if r.proc != nil {
			p2 = append(p2, "container of "+r.proc.Name, "compose project "+c.Project)
		} else {
			p2 = append(p2, "not owned by a process (read-only)", "compose project "+c.Project)
		}
		p2 = append(p2, "docker logs")
		return strings.Join(p1, sep), s.idle.Render(strings.Join(p2, " · "))
	}
	p := r.proc
	lk := procLook(p)
	word := p.StateText()
	if p.State == core.StateCrashed && p.ExitCode != nil {
		word = "crashed " + strconv.Itoa(*p.ExitCode)
	}
	if pd := m.pending[p.Name]; pd != nil {
		lk = look{spinner[m.spin%len(spinner)], roleWarn, ""}
		word = pd.verb + "…"
	}
	role := lk.role
	if p.DockerState != "" && pd(m, p) == nil {
		role = stateRole(p)
	}
	p1 = append(p1, s.color(role).Render(word))
	if o := m.outs[m.active]; p.State == core.StateAbsent && o != nil && !o.prev && len(o.lines) > 0 {
		p1 = append(p1, s.idle.Render("last run output"))
	}
	if p.DockerState != "" {
		p1 = append(p1, s.color(lk.role).Render(lk.glyph+" "+lk.word))
	}
	if p.Changed {
		p1 = append(p1, s.warn.Render("◆ changed, r applies it"))
	}
	if p.Running() {
		p1 = append(p1, "pid "+strconv.Itoa(p.PID), "up "+fmtDur(p.Uptime(m.now)))
	}
	if ps := p.PortNumbers(); len(ps) > 0 {
		p1 = append(p1, s.accent.Render(fmtPorts(ps)))
	} else if len(p.LastPorts) > 0 {
		p1 = append(p1, s.idle.Render("last "+fmtPorts(p.LastPorts)))
	}
	p1 = append(p1, "cwd "+p.Cwd, p.Cmd)

	if p.Description != "" {
		p2 = append(p2, s.idle.Render(p.Description))
	}
	if p.Kind == core.KindAdhoc {
		p2 = append(p2, s.idle.Render("ad-hoc"))
	}
	if len(p.EnvKeys) > 0 {
		p2 = append(p2, s.idle.Render("env "+strings.Join(p.EnvKeys, " ")))
	}
	for _, pr := range p.Problems {
		p2 = append(p2, s.failed.Render("✗ "+pr))
	}
	if m.st != nil {
		for _, w := range m.st.Warnings {
			if rest, ok := strings.CutPrefix(w, p.Name+": "); ok {
				p2 = append(p2, s.warn.Render("▲ "+rest))
			}
		}
	}
	return strings.Join(p1, sep), strings.Join(p2, sep)
}

func pd(m *Model, p *core.Process) *pendingAction { return m.pending[p.Name] }

// stateRole colours the bare process state (the header shows it next to
// the containers' state).
func stateRole(p *core.Process) role {
	switch p.State {
	case core.StateRunning:
		return roleRunning
	case core.StateExited:
		return roleDone
	case core.StateCrashed, core.StateDead:
		return roleFailed
	}
	return roleIdle
}
