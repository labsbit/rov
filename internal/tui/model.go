package tui

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/fsnotify/fsnotify"

	"github.com/labsbit/rov/internal/core"
	"github.com/labsbit/rov/internal/docker"
	"github.com/labsbit/rov/internal/ports"
	"github.com/labsbit/rov/internal/tmux"
)

// Poll rates and timeouts (spec: tmux 1 s, /proc 2 s, docker 5 s).
const (
	tmuxEvery    = 1 // seconds
	portsEvery   = 2 // seconds
	dockerEvery  = 5 // seconds
	pollTimeout  = 4 * time.Second
	actionTimout = 3 * time.Minute
	flashFor     = 3 * time.Second
)

// Options configure a Model. The zero value is the real TUI.
type Options struct {
	// Now is the clock (time.Now).
	Now func() time.Time
	// Home shortens paths in the top bar (the user's home directory).
	Home string
	// Manual turns polling off: sources refresh once at start and then only
	// on refreshMsg. Tests drive the model this way.
	Manual bool
	// Dark forces the palette; nil asks the terminal for its background.
	Dark *bool
	// Profile forces the colour profile; nil takes the terminal's.
	Profile *colorprofile.Profile
}

type focusPane int

const (
	focusList focusPane = iota
	focusOutput
)

// Model is the TUI.
type Model struct {
	b    Backend
	opts Options

	// sources, latest of each
	sessions   []tmux.Session
	snap       *ports.Snapshot
	containers []docker.Container
	dockerErr  error
	tmuxErr    error
	portsErr   error
	cfgBytes   []byte // rov.yaml as last read: the baseline of edits
	st         *core.Status

	rows    []row
	selKey  string
	selIdx  int
	listTop int // first visible line of the list

	focus  focusPane
	width  int
	height int
	sty    *styles
	cw     cells // the renderer's width method
	dark   bool
	prof   colorprofile.Profile
	now    time.Time

	outs   map[string]*outState
	active string // key of the outState shown
	wrap   bool
	nums   bool

	pending map[string]*pendingAction // by process name or container target
	spin    int
	spinOn  bool

	flash   flash
	search  *searchInput
	confirm *confirmState
	form    *form
	help    bool
	helpTop int

	// polling
	tick                int
	pollSeq             int // id of the last tmux poll issued
	dockerSeq           int
	dockerSeen          int // id of the last docker poll answered
	tmuxBusy, portsBusy bool
	dockerBusy          bool
	watch               *fsnotify.Watcher
	watchCh             chan struct{}
	quitting            bool
	exec                bool // a foreground process (attach) owns the terminal
}

// pendingAction is an action in flight on a row: the row shows a spinner
// until the action is done and a later poll has confirmed it.
type pendingAction struct {
	verb      string // "starting", "stopping", …
	done      bool
	afterPoll int // clear on the first tmux poll with an id ≥ this
	afterDock int // container actions: the docker poll instead
}

type flash struct {
	text  string
	err   bool
	until time.Time
}

// New returns the TUI model for a backend.
func New(b Backend, opts Options) *Model {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Home == "" {
		opts.Home, _ = os.UserHomeDir()
	}
	dark := true
	if opts.Dark != nil {
		dark = *opts.Dark
	}
	prof := colorprofile.TrueColor
	if opts.Profile != nil {
		prof = *opts.Profile
	}
	m := &Model{
		b: b, opts: opts,
		outs:    map[string]*outState{},
		pending: map[string]*pendingAction{},
		wrap:    true,
		nums:    true,
		dark:    dark,
		prof:    prof,
		sty:     newStyles(dark, prof),
		now:     opts.Now(),
		selIdx:  0,
	}
	m.recompose()
	return m
}

// Messages.
type (
	tickMsg     time.Time
	spinMsg     struct{}
	refreshMsg  struct{} // poll every source now
	watchMsg    struct{} // rov.yaml changed on disk
	sessionsMsg struct {
		id       int
		sessions []tmux.Session
		err      error
		cfgData  []byte
		cfgRead  bool
	}
	portsMsg struct {
		snap *ports.Snapshot
		err  error
	}
	dockerMsg struct {
		id         int
		containers []docker.Container
		err        error
	}
	configMsg struct {
		data []byte
		read bool
	}
	actionDoneMsg struct {
		key, label string // pending key; "<name>" or "<owner>/<service>" for the flash
		msg        string
		err        error
		container  bool
	}
	attachDoneMsg struct {
		name string
		err  error
	}
	noopMsg struct{}
)

// Init starts polling, the clock, the rov.yaml watch and asks for the
// terminal's background.
func (m *Model) Init() tea.Cmd {
	cmds := []tea.Cmd{m.pollTmux(), m.pollPorts(), m.pollDocker()}
	if !m.opts.Manual {
		// Bubble Tea counts cells with wcwidth until the terminal reports
		// Unicode core mode; ask, and follow its switch (ModeReportMsg).
		cmds = append(cmds, tea.Raw(ansi.RequestModeUnicodeCore))
		if graphemeTmux(os.Getenv) {
			// tmux 3.4+ lays out emoji sequences as grapheme clusters but
			// doesn't answer the query: report the mode on its behalf, so
			// the renderer and the layout both count graphemes.
			cmds = append(cmds, func() tea.Msg {
				return tea.ModeReportMsg{Mode: ansi.ModeUnicodeCore, Value: ansi.ModeSet}
			})
		}
	}
	if m.opts.Dark == nil {
		cmds = append(cmds, tea.RequestBackgroundColor)
	}
	if !m.opts.Manual {
		cmds = append(cmds, tickCmd(), m.startWatch())
	}
	return tea.Batch(cmds...)
}

func tickCmd() tea.Cmd {
	return tea.Every(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func spinCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return spinMsg{} })
}

// pollTmux reloads rov.yaml (a stat, a read when changed) and lists the
// sessions.
func (m *Model) pollTmux() tea.Cmd {
	if m.tmuxBusy {
		return nil
	}
	m.tmuxBusy = true
	m.pollSeq++
	id, b, read := m.pollSeq, m.b, m.cfgBytes == nil
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		changed, _ := b.Reload()
		msg := sessionsMsg{id: id}
		if changed || read {
			msg.cfgData, _ = os.ReadFile(b.ConfigPath())
			msg.cfgRead = true
		}
		msg.sessions, msg.err = b.Sessions(ctx)
		return msg
	}
}

func (m *Model) pollPorts() tea.Cmd {
	if m.portsBusy {
		return nil
	}
	m.portsBusy = true
	b := m.b
	return func() tea.Msg {
		snap, err := b.ScanPorts()
		return portsMsg{snap, err}
	}
}

func (m *Model) pollDocker() tea.Cmd {
	if m.dockerBusy {
		return nil
	}
	m.dockerBusy = true
	m.dockerSeq++
	id, b := m.dockerSeq, m.b
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		cs, err := b.Containers(ctx)
		return dockerMsg{id, cs, err}
	}
}

// reloadConfig is the rov.yaml watch's reaction.
func (m *Model) reloadConfig() tea.Cmd {
	b := m.b
	return func() tea.Msg {
		changed, _ := b.Reload()
		if !changed {
			return configMsg{}
		}
		data, _ := os.ReadFile(b.ConfigPath())
		return configMsg{data: data, read: true}
	}
}

// startWatch watches the directory of rov.yaml (editors and configedit
// replace the file, so watching the file itself would lose it). Without
// fsnotify the 1 s poll still reloads it.
func (m *Model) startWatch() tea.Cmd {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil
	}
	path := m.b.ConfigPath()
	if err := w.Add(dirOf(path)); err != nil {
		w.Close()
		return nil
	}
	m.watch = w
	m.watchCh = make(chan struct{}, 1)
	base := baseOf(path)
	go func() {
		for {
			select {
			case ev, ok := <-w.Events:
				if !ok {
					close(m.watchCh)
					return
				}
				if baseOf(ev.Name) == base {
					select {
					case m.watchCh <- struct{}{}:
					default:
					}
				}
			case _, ok := <-w.Errors:
				if !ok {
					close(m.watchCh)
					return
				}
			}
		}
	}()
	return m.waitWatch()
}

func (m *Model) waitWatch() tea.Cmd {
	ch := m.watchCh
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		if _, ok := <-ch; !ok {
			return noopMsg{}
		}
		// let a burst of events (temp file, rename) settle
		time.Sleep(50 * time.Millisecond)
		return watchMsg{}
	}
}

func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i+1]
	}
	return "."
}

func baseOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[i+1:]
	}
	return path
}

// close releases the watcher and the loaders.
func (m *Model) close() {
	if m.watch != nil {
		m.watch.Close()
		m.watch = nil
	}
	for _, o := range m.outs {
		if o.cancel != nil {
			o.cancel()
			o.cancel = nil
		}
	}
}

// graphemeTmux reports whether the TUI runs in a tmux pane of version 3.4
// or later (tmux sets TERM_PROGRAM and TERM_PROGRAM_VERSION in its panes),
// which measures emoji sequences — 👍🏽, 👨‍👩‍👧, ❤️ — as one grapheme of
// two cells, not by wcwidth.
func graphemeTmux(getenv func(string) string) bool {
	if getenv("TERM_PROGRAM") != "tmux" {
		return false
	}
	v := strings.TrimPrefix(getenv("TERM_PROGRAM_VERSION"), "next-")
	major, rest, _ := strings.Cut(v, ".")
	minor := strings.TrimRightFunc(rest, func(r rune) bool { return r < '0' || r > '9' })
	ma, err1 := strconv.Atoi(major)
	mi, err2 := strconv.Atoi(minor)
	if err1 != nil || err2 != nil {
		return false
	}
	return ma > 3 || ma == 3 && mi >= 4
}

// setWidthMethod changes how text is measured; laid-out rows are redone.
func (m *Model) setWidthMethod(wm ansi.Method) {
	m.cw = cells(wm)
	if m.form != nil {
		m.form.cw = m.cw
	}
}

func (m *Model) restyle() {
	m.sty = newStyles(m.dark, m.prof)
	if m.form != nil {
		m.form.restyle(m.sty)
	}
}

// recompose builds the Status from the latest sources and brings the rows,
// the selection, pending actions and the output pane in line with it.
func (m *Model) recompose() {
	st := core.Compose(m.b.Inputs(m.sessions, m.snap, m.containers, m.dockerErr))
	m.st = st
	m.rows = buildRows(st)
	m.selIdx = reselect(m.rows, m.selKey, m.selIdx)
	if m.selIdx >= 0 {
		m.selKey = m.rows[m.selIdx].key
	} else {
		m.selKey = ""
	}
	m.refreshForm()
}

// selected returns the selected row, or nil.
func (m *Model) selected() *row {
	if m.selIdx < 0 || m.selIdx >= len(m.rows) {
		return nil
	}
	return &m.rows[m.selIdx]
}

// moveSel moves the selection by n selectable rows.
func (m *Model) moveSel(n int) {
	if len(m.rows) == 0 {
		return
	}
	i := m.selIdx
	step := 1
	if n < 0 {
		step, n = -1, -n
	}
	for ; n > 0; n-- {
		j := i + step
		for j >= 0 && j < len(m.rows) && !m.rows[j].selectable() {
			j += step
		}
		if j < 0 || j >= len(m.rows) {
			break
		}
		i = j
	}
	m.selectIdx(i)
}

func (m *Model) selectIdx(i int) {
	if i < 0 || i >= len(m.rows) || !m.rows[i].selectable() {
		return
	}
	m.selIdx, m.selKey = i, m.rows[i].key
}

func (m *Model) selectEdge(last bool) {
	idx := -1
	for i := range m.rows {
		if m.rows[i].selectable() {
			if idx < 0 || last {
				idx = i
			}
			if !last {
				break
			}
		}
	}
	m.selectIdx(idx)
}

// syncOutput makes the output pane show the selected row: it starts the
// loader of a newly selected row, stops the one of the row left, and
// restarts a process's output when the process restarted.
func (m *Model) syncOutput() []tea.Cmd {
	var cmds []tea.Cmd
	r := m.selected()
	key := ""
	if r != nil {
		key = r.key
	}
	if key != m.active {
		if o := m.outs[m.active]; o != nil {
			m.deactivate(o)
		}
		m.active = key
	}
	if r == nil {
		return nil
	}
	o := m.outs[key]
	if o == nil {
		switch {
		case r.kind == rowProc:
			o = newOutState(key, outProcess, r.proc.Name)
		case r.kind == rowContainer:
			o = newOutState(key, outContainer, r.ctr.Name)
		default:
			return nil
		}
		m.outs[key] = o
	}
	switch {
	case o.kind == outProcess && r.proc != nil:
		// A new run (a restart, a start after down — also while another
		// row was selected): the old one moved to prev; show the new one
		// from its start, following. A run ending keeps its id, and a
		// session going away (id "") keeps the last run's output.
		run := r.proc.RunID
		if o.runKnown && run != "" && run != o.run {
			if o.active {
				m.deactivate(o)
			}
			o.clear()
			o.resume()
		}
		if run != "" || !o.runKnown {
			o.run, o.runKnown = run, true
		}
	case o.kind == outContainer && r.ctr != nil && o.ended && !o.active:
		// The stream ended; pick it up again once a poll issued after
		// the end sees the container running.
		if r.ctr.State == "running" && m.dockerSeen > o.endDock {
			o.ended, o.since = false, o.endedAt
		} else {
			return nil
		}
	}
	if !o.active {
		cmds = append(cmds, m.activate(o))
	}
	return cmds
}

func (m *Model) activate(o *outState) tea.Cmd {
	ctx, cancel := context.WithCancel(context.Background())
	o.gen++
	o.active, o.cancel = true, cancel
	if o.since.IsZero() {
		// a fresh load; a container stream picked up again keeps its lines
		o.lines, o.base, o.err = nil, 0, ""
		o.matches = nil
		if o.query != "" {
			o.setSearch(o.query)
		}
	}
	var ch <-chan batch
	switch {
	case o.kind == outContainer:
		ch = followDocker(ctx, m.b, o.name, o.since)
	case o.prev:
		ch = loadPrev(ctx, m.b.LogDir(), o.name)
	default:
		ch = followFile(ctx, m.b.LogDir(), o.name)
	}
	o.ch = ch
	return waitBatch(o.key, o.gen, ch)
}

// deactivate stops the loader and drops the lines, keeping the position,
// follow state and search.
func (m *Model) deactivate(o *outState) {
	if o.cancel != nil {
		o.cancel()
		o.cancel = nil
	}
	o.active, o.ch = false, nil
	o.gen++
	o.lines = nil
	o.matches = nil
	o.cache = nil
	o.ended, o.since = false, time.Time{}
}

// streamEnded marks the end of a container's log stream with a marker line;
// syncOutput picks it up again once the container runs.
func (m *Model) streamEnded(o *outState) {
	if o.cancel != nil {
		o.cancel()
		o.cancel = nil
	}
	o.active, o.ch = false, nil
	o.gen++
	text := "── stream ended (container stopped) ──"
	if o.err != "" {
		text = "── stream ended: " + o.err + " ──"
		o.err = ""
	}
	if n := len(o.lines); n == 0 || o.lines[n-1].plain != text {
		o.append([]logLine{markerLine(text)})
	}
	o.ended, o.since = true, time.Time{}
	o.endedAt, o.endDock = m.opts.Now(), m.dockerSeq
}

// setFlash shows a message instead of the key hints for 3 s.
func (m *Model) setFlash(text string, isErr bool) {
	m.flash = flash{text: text, err: isErr, until: m.opts.Now().Add(flashFor)}
}

func (m *Model) flashErr(prefix string, err error) {
	msg := err.Error()
	if prefix != "" {
		msg = prefix + ": " + msg
	}
	m.setFlash(firstLine(msg), true)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// startPending marks key busy and returns the spinner tick if it isn't
// running yet.
func (m *Model) startPending(key, verb string) tea.Cmd {
	m.pending[key] = &pendingAction{verb: verb}
	if m.spinOn {
		return nil
	}
	m.spinOn = true
	return spinCmd()
}

// Update handles a message.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.form != nil {
			m.form.resize(m.width, m.height)
		}

	case tea.BackgroundColorMsg:
		if m.opts.Dark == nil {
			m.dark = msg.IsDark()
			m.restyle()
		}

	case tea.ModeReportMsg:
		// The renderer switches to grapheme widths on this same report
		// (bubbletea's eventLoop); lay out with what it draws with.
		if msg.Mode == ansi.ModeUnicodeCore {
			switch msg.Value {
			case ansi.ModeReset, ansi.ModeSet, ansi.ModePermanentlySet:
				m.setWidthMethod(ansi.GraphemeWidth)
			}
		}

	case tea.ColorProfileMsg:
		if m.opts.Profile == nil {
			m.prof = msg.Profile
			m.restyle()
		}

	case tickMsg:
		m.now = m.opts.Now()
		m.tick++
		if !m.flash.until.IsZero() && !m.now.Before(m.flash.until) {
			m.flash = flash{}
		}
		if m.exec {
			// attached: the terminal is tmux's; poll again on return
			return m, tickCmd()
		}
		if m.tick%tmuxEvery == 0 {
			cmds = append(cmds, m.pollTmux())
		}
		if m.tick%portsEvery == 0 {
			cmds = append(cmds, m.pollPorts())
		}
		if m.tick%dockerEvery == 0 {
			cmds = append(cmds, m.pollDocker())
		}
		cmds = append(cmds, tickCmd())

	case refreshMsg:
		m.now = m.opts.Now()
		cmds = append(cmds, m.pollTmux(), m.pollPorts(), m.pollDocker())

	case spinMsg:
		m.spin++
		if len(m.pending) > 0 {
			cmds = append(cmds, spinCmd())
		} else {
			m.spinOn = false
		}

	case watchMsg:
		cmds = append(cmds, m.reloadConfig(), m.waitWatch())

	case configMsg:
		if msg.read {
			m.cfgBytes = msg.data
			if m.cfgBytes == nil {
				m.cfgBytes = []byte{}
			}
		}
		m.recompose()

	case sessionsMsg:
		m.tmuxBusy = false
		if msg.cfgRead {
			m.cfgBytes = msg.cfgData
			if m.cfgBytes == nil {
				m.cfgBytes = []byte{}
			}
		}
		m.tmuxErr = msg.err
		if msg.err == nil {
			m.sessions = msg.sessions
		}
		for k, p := range m.pending {
			if p.done && p.afterDock == 0 && msg.id >= p.afterPoll {
				delete(m.pending, k)
			}
		}
		m.recompose()

	case portsMsg:
		m.portsBusy = false
		m.portsErr = msg.err
		if msg.err == nil {
			m.snap = msg.snap
		}
		m.recompose()
		if msg.err == nil && m.st != nil {
			cmds = append(cmds, m.recordPorts())
		}

	case dockerMsg:
		m.dockerBusy = false
		m.dockerSeen = max(m.dockerSeen, msg.id)
		// A failed poll keeps the last list (rows don't vanish, the
		// selection stays); the top bar shows the error. No docker at all
		// clears it.
		if msg.err == nil || errors.Is(msg.err, docker.ErrUnavailable) {
			m.containers = msg.containers
		}
		m.dockerErr = msg.err
		for k, p := range m.pending {
			if p.done && p.afterDock > 0 && msg.id >= p.afterDock {
				delete(m.pending, k)
			}
		}
		m.recompose()

	case actionDoneMsg:
		cmds = append(cmds, m.actionDone(msg))

	case attachDoneMsg:
		m.exec = false
		if msg.err != nil {
			m.flashErr("attach "+msg.name, msg.err)
		}
		cmds = append(cmds, m.pollTmux(), m.pollPorts())

	case linesMsg:
		o := m.outs[msg.key]
		if o == nil || o.gen != msg.gen {
			break
		}
		if msg.b.reset {
			o.lines, o.base = nil, 0
		}
		if msg.b.hasStart && len(o.lines) == 0 {
			o.base = msg.b.start
		}
		if msg.b.err != nil {
			o.err = firstLine(msg.b.err.Error())
		}
		o.append(msg.b.lines)
		if o.cancel != nil {
			cmds = append(cmds, waitBatch(o.key, o.gen, o.ch))
		}

	case loaderDoneMsg:
		// The loader ended: the previous run is read, or a container's
		// stream is over (it stopped).
		if o := m.outs[msg.key]; o != nil && o.gen == msg.gen && o.kind == outContainer {
			m.streamEnded(o)
		}

	case configWrittenMsg:
		cmds = append(cmds, m.configWritten(msg))

	case tea.KeyPressMsg:
		cmds = append(cmds, m.key(msg))

	case tea.MouseMsg:
		cmds = append(cmds, m.mouse(msg))

	case tea.PasteMsg:
		if m.form != nil {
			cmds = append(cmds, m.form.update(msg))
		} else if m.search != nil {
			cmds = append(cmds, m.search.update(msg))
		}

	case snapshotMsg:
		msg.ch <- m.render()

	default:
		if m.form != nil {
			cmds = append(cmds, m.form.update(msg))
		}
	}
	if m.quitting {
		m.close()
		return m, tea.Quit
	}
	cmds = append(cmds, m.syncOutput()...)
	return m, tea.Batch(cmds...)
}

func (m *Model) recordPorts() tea.Cmd {
	// RecordPorts writes into the status it is given: hand it a copy.
	st := *m.st
	st.Processes = slices.Clone(m.st.Processes)
	b := m.b
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		_ = b.RecordPorts(ctx, &st)
		return nil
	}
}

// actionDone ends a pending action: the spinner stays until the next poll
// shows the result.
func (m *Model) actionDone(msg actionDoneMsg) tea.Cmd {
	if msg.err != nil {
		delete(m.pending, msg.key)
		m.flashErr(msg.label, msg.err)
	} else if msg.msg != "" {
		m.setFlash(msg.label+": "+msg.msg, false)
	}
	if p := m.pending[msg.key]; p != nil {
		p.done = true
		if msg.container {
			p.afterDock = m.dockerSeq + 1
		} else {
			p.afterPoll = m.pollSeq + 1
		}
	}
	var cmds []tea.Cmd
	// A poll in flight was issued before the action ended; ask again once
	// it's back (busy flags drop the request otherwise).
	if m.tmuxBusy || m.dockerBusy {
		cmds = append(cmds, tea.Tick(150*time.Millisecond, func(time.Time) tea.Msg { return refreshMsg{} }))
	}
	cmds = append(cmds, m.pollTmux(), m.pollPorts(), m.pollDocker())
	return tea.Batch(cmds...)
}

// errText is an error for display, nil-safe.
func errText(err error) string {
	if err == nil {
		return ""
	}
	var de interface{ Unwrap() error }
	if errors.As(err, &de) && errors.Is(err, docker.ErrUnavailable) {
		return "docker unavailable"
	}
	return firstLine(err.Error())
}

// snapshotMsg asks the model for its rendered view (tests).
type snapshotMsg struct{ ch chan string }
