package tui

import (
	"context"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/labsbit/rov/internal/configedit"
	"github.com/labsbit/rov/internal/core"
)

// key handles a key press in the current mode.
func (m *Model) key(k tea.KeyPressMsg) tea.Cmd {
	ks := k.String()
	if m.exec {
		// attaching: the keys are tmux's
		return nil
	}
	if ks == "ctrl+c" {
		m.quitting = true
		return nil
	}
	switch {
	case m.form != nil:
		return m.formKey(k)
	case m.confirm != nil:
		return m.confirmKey(ks)
	case m.help:
		switch ks {
		case "?", "esc", "q", "enter":
			m.help, m.helpTop = false, 0
		case "j", "down":
			m.helpTop++
		case "k", "up":
			m.helpTop = max(0, m.helpTop-1)
		}
		return nil
	case m.search != nil:
		return m.searchKey(k)
	}

	// both panes
	switch ks {
	case "q":
		m.quitting = true
		return nil
	case "?":
		m.help = true
		return nil
	case "tab":
		m.toggleFocus()
		return nil
	case "space":
		return m.toggleRun()
	case "r":
		return m.restart()
	case "x":
		return m.askRemove()
	case "a":
		return m.attach()
	case "/":
		m.focus = focusOutput
		m.search = newSearchInput(m.sty, m.outs[m.active])
		return m.search.input.Focus()
	case "#":
		m.nums = !m.nums
		return nil
	case "w":
		m.wrap = !m.wrap
		return nil
	case "p":
		return m.togglePrev()
	}
	if m.focus == focusList {
		return m.listKey(ks)
	}
	return m.outputKey(ks)
}

func (m *Model) toggleFocus() {
	if m.focus == focusList {
		m.focus = focusOutput
	} else {
		m.focus = focusList
	}
}

func (m *Model) listKey(ks string) tea.Cmd {
	switch ks {
	case "j", "down":
		m.moveSel(1)
	case "k", "up":
		m.moveSel(-1)
	case "g", "home":
		m.selectEdge(false)
	case "G", "end":
		m.selectEdge(true)
	case "l", "right", "enter":
		m.focus = focusOutput
	case "n":
		return m.openNew()
	case "e":
		return m.openEdit()
	case "s":
		return m.openSave()
	case "J":
		return m.move(false)
	case "K":
		return m.move(true)
	case "D":
		return m.askDelete()
	}
	return nil
}

func (m *Model) outputKey(ks string) tea.Cmd {
	o := m.outs[m.active]
	switch ks {
	case "h", "left", "esc":
		m.focus = focusList
		return nil
	}
	if o == nil {
		return nil
	}
	v := m.curView(o)
	switch ks {
	case "j", "down":
		o.scroll(1, v)
	case "k", "up":
		o.scroll(-1, v)
	case "ctrl+d":
		o.scroll(max(1, v.height/2), v)
	case "ctrl+u":
		o.scroll(-max(1, v.height/2), v)
	case "ctrl+f", "pgdown":
		o.scroll(max(1, v.height-1), v)
	case "ctrl+b", "pgup":
		o.scroll(-max(1, v.height-1), v)
	case "g", "home":
		o.home(v)
	case "G", "end":
		o.resume()
	case "n":
		if !o.jump(1, v) && o.query != "" {
			m.setFlash("no match for "+o.query, false)
		}
	case "N":
		if !o.jump(-1, v) && o.query != "" {
			m.setFlash("no match for "+o.query, false)
		}
	}
	return nil
}

// togglePrev switches the output of a process between its current and its
// previous run.
func (m *Model) togglePrev() tea.Cmd {
	o := m.outs[m.active]
	if o == nil || o.kind != outProcess {
		return nil
	}
	m.deactivate(o)
	o.prev = !o.prev
	o.clear()
	o.resume()
	return m.activate(o)
}

// --- actions ---------------------------------------------------------------

// run starts an action in the background, with a spinner on its row.
func (m *Model) run(key, label, verb string, container bool, fn func(ctx context.Context) (string, error)) tea.Cmd {
	if m.pending[key] != nil {
		m.setFlash(label+" is busy", false)
		return nil
	}
	spin := m.startPending(key, verb)
	act := func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), actionTimout)
		defer cancel()
		msg, err := fn(ctx)
		return actionDoneMsg{key: key, label: label, msg: msg, err: err, container: container}
	}
	return tea.Batch(spin, act)
}

func (m *Model) toggleRun() tea.Cmd {
	r := m.selected()
	if r == nil {
		return nil
	}
	b := m.b
	switch r.kind {
	case rowContainer:
		if r.proc == nil {
			m.setFlash(r.target()+" belongs to no process: rov leaves it alone", false)
			return nil
		}
		t := r.target()
		if r.ctr.State == "running" {
			return m.run(t, t, "stopping", true, func(ctx context.Context) (string, error) { return b.ContainerStop(ctx, t) })
		}
		return m.run(t, t, "starting", true, func(ctx context.Context) (string, error) { return b.ContainerStart(ctx, t) })
	case rowProc:
		name := r.proc.Name
		if r.proc.Running() {
			return m.run(name, name, "stopping", false, func(ctx context.Context) (string, error) { return b.Stop(ctx, name) })
		}
		return m.run(name, name, "starting", false, func(ctx context.Context) (string, error) { return b.Up(ctx, name) })
	}
	return nil
}

func (m *Model) restart() tea.Cmd {
	r := m.selected()
	if r == nil {
		return nil
	}
	if r.kind != rowProc {
		m.setFlash("r restarts processes; space starts or stops a container", false)
		return nil
	}
	name, b := r.proc.Name, m.b
	return m.run(name, name, "restarting", false, func(ctx context.Context) (string, error) { return b.Restart(ctx, name) })
}

func (m *Model) attach() tea.Cmd {
	r := m.selected()
	if r == nil || r.kind != rowProc {
		m.setFlash("a attaches to a process", false)
		return nil
	}
	if !r.proc.HasSession {
		m.setFlash(r.proc.Name+" has no session: space starts it", false)
		return nil
	}
	name := r.proc.Name
	m.exec = true
	return tea.ExecProcess(m.b.AttachCommand(name), func(err error) tea.Msg { return attachDoneMsg{name, err} })
}

// --- confirmations ----------------------------------------------------------

type confirmState struct {
	question string
	yes      func() tea.Cmd
}

func (m *Model) confirmKey(ks string) tea.Cmd {
	c := m.confirm
	switch ks {
	case "y", "Y":
		m.confirm = nil
		return c.yes()
	case "n", "N", "esc", "q":
		m.confirm = nil
	}
	return nil
}

func (m *Model) askRemove() tea.Cmd {
	r := m.selected()
	if r == nil || r.kind != rowProc {
		return nil
	}
	p := r.proc
	if p.Kind != core.KindAdhoc {
		m.setFlash(p.Name+" is in rov.yaml: D deletes it there", false)
		return nil
	}
	name, b := p.Name, m.b
	m.confirm = &confirmState{
		question: "remove " + name + "?",
		yes: func() tea.Cmd {
			return m.run(name, name, "removing", false, func(ctx context.Context) (string, error) { return b.Rm(ctx, name, false) })
		},
	}
	return nil
}

func (m *Model) askDelete() tea.Cmd {
	r := m.selected()
	if r == nil || r.kind != rowProc {
		return nil
	}
	p := r.proc
	if p.Kind != core.KindConfig {
		m.setFlash(p.Name+" is ad-hoc: x removes it", false)
		return nil
	}
	name, running, b, baseline := p.Name, p.Running(), m.b, m.cfgBytes
	q := "delete " + name + " from rov.yaml?"
	if running {
		q = name + " is running — stop and delete?"
	}
	m.confirm = &confirmState{
		question: q,
		yes: func() tea.Cmd {
			return m.run(name, name, "deleting", false, func(ctx context.Context) (string, error) {
				if running {
					if _, err := b.Stop(ctx, name); err != nil {
						return "", err
					}
				}
				if _, err := b.WriteConfig(ctx, baseline, configedit.Remove{Name: name}); err != nil {
					return "", err
				}
				// A leftover session would show up as an ad-hoc process.
				_ = b.DropSession(ctx, name)
				return "deleted from rov.yaml", nil
			})
		},
	}
	return nil
}

// move moves the selected config process up or down in rov.yaml.
func (m *Model) move(up bool) tea.Cmd {
	r := m.selected()
	if r == nil || r.kind != rowProc || r.proc.Kind != core.KindConfig {
		m.setFlash("J/K move processes of rov.yaml", false)
		return nil
	}
	name, b, baseline := r.proc.Name, m.b, m.cfgBytes
	pos := configedit.Position{Down: !up, Up: up}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		data, err := b.WriteConfig(ctx, baseline, configedit.Move{Name: name, Pos: pos})
		return configWrittenMsg{what: "moved " + name, data: data, err: err, quiet: true}
	}
}

// --- search -----------------------------------------------------------------

type searchInput struct {
	input textinput.Model
}

func newSearchInput(s *styles, o *outState) *searchInput {
	ti := textinput.New()
	ti.Prompt = "/"
	ti.CharLimit = 256
	st := ti.Styles()
	st.Focused.Prompt = s.accent
	st.Cursor.Blink = false
	ti.SetStyles(st)
	if o != nil {
		ti.SetValue(o.query)
		ti.CursorEnd()
	}
	return &searchInput{input: ti}
}

func (si *searchInput) update(msg tea.Msg) tea.Cmd {
	var cmd tea.Cmd
	si.input, cmd = si.input.Update(msg)
	return cmd
}

func (m *Model) searchKey(k tea.KeyPressMsg) tea.Cmd {
	switch k.String() {
	case "esc":
		m.search = nil
		return nil
	case "enter":
		q := m.search.input.Value()
		m.search = nil
		o := m.outs[m.active]
		if o == nil {
			return nil
		}
		o.setSearch(q)
		if q == "" {
			return nil
		}
		v := m.curView(o)
		o.cur = -1
		if !o.jump(1, v) {
			m.setFlash("no match for "+q, false)
		}
		return nil
	}
	return m.search.update(k)
}

// --- bottom bar -------------------------------------------------------------

type hint struct{ key, label string }

func (m *Model) mode() string {
	switch {
	case m.form != nil:
		return "FORM"
	case m.confirm != nil:
		return "CONFIRM"
	case m.search != nil:
		return "SEARCH"
	case m.focus == focusOutput:
		return "OUTPUT"
	}
	return "LIST"
}

func (m *Model) hints() []hint {
	var h []hint
	switch m.mode() {
	case "FORM":
		return []hint{{"tab", "next"}, {"shift+tab", "back"}, {"ctrl+s", "save"}, {"esc", "cancel"}}
	case "SEARCH":
		return []hint{{"enter", "search"}, {"esc", "cancel"}}
	case "OUTPUT":
		if m.layout().single {
			h = append(h, hint{"h/l", "pane"})
		}
		return append(h, hint{"j/k", "scroll"}, hint{"G", "follow"}, hint{"/", "search"}, hint{"n/N", "match"}, hint{"w", "wrap"}, hint{"#", "lines"}, hint{"p", "prev run"}, hint{"space", "start/stop"}, hint{"r", "restart"}, hint{"a", "attach"}, hint{"esc", "list"}, hint{"?", "help"}, hint{"q", "quit"})
	}
	if m.layout().single {
		// one pane at a time: say how to get to the other
		h = append(h, hint{"h/l", "pane"})
	}
	h = append(h, hint{"space", "start/stop"}, hint{"r", "restart"}, hint{"n", "new"}, hint{"e", "edit"})
	if r := m.selected(); r != nil && r.kind == rowProc && r.proc.Kind == core.KindAdhoc {
		h = append(h, hint{"s", "save"}, hint{"x", "remove"})
	} else {
		h = append(h, hint{"J/K", "move"}, hint{"x", "remove"})
	}
	return append(h, hint{"/", "search"}, hint{"a", "attach"}, hint{"?", "help"}, hint{"q", "quit"})
}

func (m *Model) bottomBar() string {
	s := m.sty
	w := m.width
	pill := s.pill.Render(m.mode())
	out := " " + pill + " "
	room := w - m.cw.width(out)
	// right side: search status of the output
	right := ""
	if o := m.outs[m.active]; o != nil && o.query != "" && m.form == nil && m.confirm == nil {
		n := "0/0"
		if len(o.matches) > 0 {
			cur := "-"
			if o.cur >= 0 {
				cur = itoa(o.cur + 1)
			}
			n = cur + "/" + itoa(len(o.matches))
		}
		right = s.idle.Render(m.cw.fit("/"+o.query, 20)+" ") + s.accent.Render(n) + " "
	}
	var mid string
	switch {
	case m.search != nil:
		mid = m.search.input.View()
	case m.confirm != nil:
		mid = s.warn.Render(m.confirm.question) + "  " + s.key.Render("y") + " " + s.label.Render("yes") + "  " + s.key.Render("n") + " " + s.label.Render("no")
	case m.flash.text != "" && m.form == nil:
		st := lipgloss.NewStyle()
		if m.flash.err {
			st = s.failed
		}
		mid = st.Render(m.flash.text)
	default:
		hs := m.hints()
		keep := func(hs []hint) string {
			parts := make([]string, len(hs))
			for i, h := range hs {
				parts[i] = s.key.Render(h.key) + " " + s.label.Render(h.label)
			}
			return strings.Join(parts, "  ")
		}
		avail := room - m.cw.width(right) - 1
		for len(hs) > 2 && m.cw.width(keep(hs)) > avail {
			// drop from the end, but keep "? help" and "q quit" last
			hs = append(hs[:len(hs)-3], hs[len(hs)-2:]...)
		}
		mid = keep(hs)
	}
	avail := room - m.cw.width(right)
	if m.cw.width(mid) > avail {
		mid = m.cw.trunc(mid, max(0, avail-1), "…")
	}
	gap := max(0, avail-m.cw.width(mid))
	return out + mid + strings.Repeat(" ", gap) + right
}

// --- help -------------------------------------------------------------------

var helpKeys = []struct{ section, key, what string }{
	{"list", "j/k ↓/↑", "next / previous row"},
	{"list", "g / G", "first / last row"},
	{"list", "l → enter", "focus output"},
	{"list", "n", "new process (rov.yaml)"},
	{"list", "e", "edit the process"},
	{"list", "s", "save ad-hoc into rov.yaml"},
	{"list", "J / K", "move down / up in rov.yaml"},
	{"list", "D", "delete from rov.yaml"},
	{"both", "space", "start / stop"},
	{"both", "r", "restart"},
	{"both", "x", "remove an ad-hoc process"},
	{"both", "a", "attach (ctrl-b d detaches)"},
	{"both", "tab", "switch pane"},
	{"both", "/", "search the output (focuses it)"},
	{"both", "#", "line numbers"},
	{"both", "w", "soft wrap"},
	{"both", "p", "previous run"},
	{"both", "q ctrl-c", "quit (processes keep running)"},
	{"output", "j/k ↓/↑", "scroll a line"},
	{"output", "ctrl-d/u", "half page down / up"},
	{"output", "ctrl-f/b", "page down / up"},
	{"output", "g / G", "top / bottom (G follows)"},
	{"output", "n N", "next / previous match"},
	{"output", "h ← esc", "focus list"},
}

func (m *Model) helpView() string {
	s := m.sty
	keyW := 10
	col := func(section string) []string {
		var out []string
		for _, h := range helpKeys {
			if h.section == section {
				out = append(out, s.accent.Render(m.cw.pad(h.key, keyW))+" "+h.what)
			}
		}
		return out
	}
	title := func(t string) string { return s.idle.Render(t) }
	left := append([]string{title("list")}, col("list")...)
	left = append(left, "", title("both panes"))
	left = append(left, col("both")...)
	right := append([]string{title("output")}, col("output")...)
	colW := keyW + 1 + 30
	var lines []string
	if m.width >= 2*colW+8 {
		for i := range max(len(left), len(right)) {
			l, r := "", ""
			if i < len(left) {
				l = left[i]
			}
			if i < len(right) {
				r = right[i]
			}
			lines = append(lines, m.cw.pad(l, colW)+"   "+r)
		}
	} else {
		lines = append(append(left, ""), right...)
	}
	// too tall: scroll with j/k
	if maxH := max(2, m.height-4); len(lines) > maxH {
		m.helpTop = min(m.helpTop, len(lines)-(maxH-1))
		more := s.idle.Render("j/k scroll")
		if m.helpTop+maxH-1 < len(lines) {
			more = s.idle.Render("↓ more · j/k scroll")
		}
		lines = append(append([]string(nil), lines[m.helpTop:m.helpTop+maxH-1]...), more)
	}
	w := 0
	for _, l := range lines {
		w = max(w, m.cw.width(l))
	}
	w = min(w, max(10, m.width-6))
	for i := range lines {
		lines[i] = " " + m.cw.pad(m.cw.fit(lines[i], w), w) + " "
	}
	return strings.Join(m.box("keys", lines, w+4, len(lines)+2, true), "\n")
}

// --- mouse ------------------------------------------------------------------

func (m *Model) mouse(msg tea.MouseMsg) tea.Cmd {
	if m.form != nil || m.help || m.confirm != nil {
		return nil
	}
	mo := msg.Mouse()
	l := m.layout()
	inList := l.leftW > 0 && mo.X < l.leftW
	switch msg := msg.(type) {
	case tea.MouseWheelMsg:
		d := 0
		switch msg.Button {
		case tea.MouseWheelUp:
			d = -1
		case tea.MouseWheelDown:
			d = 1
		}
		if d == 0 {
			return nil
		}
		if inList {
			m.moveSel(d)
			return nil
		}
		if o := m.outs[m.active]; o != nil {
			o.scroll(3*d, m.curView(o))
		}
	case tea.MouseClickMsg:
		if msg.Button != tea.MouseLeft {
			return nil
		}
		if !inList {
			if l.rightW > 0 && mo.Y >= 1 && mo.Y < 1+l.paneH {
				m.focus = focusOutput
			}
			return nil
		}
		m.focus = focusList
		i := mo.Y - 2 + m.listTop // top bar + border
		if lines := m.listLines(l); i >= 0 && i < len(lines) {
			m.selectIdx(lines[i].row)
		}
	}
	return nil
}
