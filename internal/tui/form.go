package tui

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"charm.land/bubbles/v2/cursor"
	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"

	"github.com/labsbit/rov/internal/config"
	"github.com/labsbit/rov/internal/configedit"
	"github.com/labsbit/rov/internal/core"
)

// The new / edit / save forms. Each field is a huh field (Input, Text,
// Select) laid out by the form itself: a label column, the field, and its
// validation error right under it in the failed colour.

type formKind int

const (
	formNew       formKind = iota // n: a new rov.yaml process
	formEditCfg                   // e on a config process
	formEditAdhoc                 // e on an ad-hoc process: its stored definition
	formSave                      // s: an ad-hoc process into rov.yaml
)

type fieldKind int

const (
	fieldInput fieldKind = iota
	fieldText
	fieldSelect
	fieldCheck
	fieldFixed // read-only value
)

type formField struct {
	id    string // name, cmd, cwd, env, desc, docker, pos, check
	label string
	kind  fieldKind
	huh   huh.Field
	value string // fieldFixed
	check bool   // fieldCheck
	hint  string // dim line under the field
	ph    string // fieldInput: the placeholder, shown dim while empty
	err   string
	// initial is the value the form opened with (or reloaded to): an edit
	// changes only the fields that differ from it.
	initial string
	// build makes the huh field for a value (to reset it after a reload).
	build func(value string) huh.Field
}

func (f *formField) get() string {
	switch f.kind {
	case fieldFixed:
		return f.value
	case fieldCheck:
		if f.check {
			return "yes"
		}
		return ""
	}
	if v, ok := f.huh.GetValue().(string); ok {
		return v
	}
	return ""
}

type form struct {
	kind    formKind
	title   string
	orig    string // the process edited or saved
	running bool
	fields  []*formField
	focus   int
	showAll bool // after a save attempt: show every error
	touched map[string]bool
	notice  string // warn line ("rov.yaml changed on disk — reloaded")
	err     string // failed line
	saving  bool

	baseline []byte
	cfgNames []string // names in rov.yaml (uniqueness)
	adhoc    []string // ad-hoc names
	root     string

	sty   *styles
	cw    cells
	width int
}

const (
	formLabelW = 13
	formMaxW   = 68
)

func (f *form) innerWidth() int { return max(20, f.width-4) }
func (f *form) fieldWidth() int { return max(8, f.innerWidth()-formLabelW-3) }

func (f *form) resize(w, h int) {
	f.width = min(formMaxW, max(30, w-4))
	for _, fd := range f.fields {
		if fd.huh != nil {
			fd.huh = fd.huh.WithWidth(f.fieldWidth())
		}
	}
}

func (f *form) restyle(s *styles) {
	f.sty = s
	for _, fd := range f.fields {
		if fd.huh != nil {
			fd.huh = fd.huh.WithTheme(huhTheme(s))
		}
	}
}

// huhTheme styles huh fields with the palette: no frames (the form draws
// its own), accent prompt and cursor.
func huhTheme(s *styles) huh.Theme {
	return huh.ThemeFunc(func(bool) *huh.Styles {
		t := huh.ThemeBase(s.p.dark)
		plain := lipgloss.NewStyle()
		for _, fs := range []*huh.FieldStyles{&t.Focused, &t.Blurred} {
			fs.Base = plain
			fs.Title = plain
			fs.Description = s.idle
			fs.ErrorIndicator = plain
			fs.ErrorMessage = s.failed
			fs.SelectSelector = plain
			fs.Option = plain
			fs.SelectedOption = plain
			fs.UnselectedOption = plain
			fs.TextInput.Prompt = plain
			fs.TextInput.Text = plain
			fs.TextInput.Placeholder = s.idle
			fs.TextInput.Cursor = s.accent
			fs.TextInput.CursorText = plain
		}
		t.Focused.PrevIndicator = s.accent.SetString("‹ ")
		t.Focused.NextIndicator = s.accent.SetString(" ›")
		t.Blurred.PrevIndicator = s.idle.SetString("‹ ")
		t.Blurred.NextIndicator = s.idle.SetString(" ›")
		return t
	})
}

// huhKeys: tab/shift+tab/enter/up/down are the form's (navigation), enter
// in the env box is a new line, and selects don't filter.
func huhKeys() *huh.KeyMap {
	k := huh.NewDefaultKeyMap()
	none := key.NewBinding(key.WithDisabled())
	k.Quit = none
	k.Text.NewLine = key.NewBinding(key.WithKeys("enter", "alt+enter", "ctrl+j"))
	k.Text.Next, k.Text.Submit, k.Text.Prev, k.Text.Editor = none, none, none, none
	k.Select.Filter, k.Select.SetFilter, k.Select.ClearFilter = none, none, none
	k.Select.Next, k.Select.Submit, k.Select.Prev = none, none, none
	k.Input.Next, k.Input.Submit, k.Input.Prev = none, none, none
	return k
}

func (m *Model) newForm(kind formKind, title string) *form {
	f := &form{kind: kind, title: title, touched: map[string]bool{}, sty: m.sty, cw: m.cw, baseline: m.cfgBytes, root: m.b.Root()}
	f.cfgNames = m.b.Config().Names()
	if m.st != nil {
		for _, p := range m.st.Processes {
			if p.Kind == core.KindAdhoc {
				f.adhoc = append(f.adhoc, p.Name)
			}
		}
	}
	f.width = min(formMaxW, max(30, m.width-4))
	return f
}

func (f *form) input(id, label, value, placeholder string) *formField {
	return f.add(&formField{id: id, label: label, kind: fieldInput, ph: placeholder, build: func(value string) huh.Field {
		v := value
		return huh.NewInput().Prompt("").Value(&v).Placeholder(placeholder).CharLimit(4096)
	}}, value)
}

func (f *form) text(id, label, value, hint string) *formField {
	return f.add(&formField{id: id, label: label, kind: fieldText, hint: hint, build: func(value string) huh.Field {
		v := value
		return huh.NewText().Value(&v).Lines(2).ShowLineNumbers(false).CharLimit(8192)
	}}, value)
}

func (f *form) choice(id, label string, opts []huh.Option[string], value string) *formField {
	return f.add(&formField{id: id, label: label, kind: fieldSelect, build: func(value string) huh.Field {
		v := value
		return huh.NewSelect[string]().Options(opts...).Value(&v).Inline(true)
	}}, value)
}

func (f *form) fixed(id, label, value, hint string) *formField {
	return f.add(&formField{id: id, label: label, kind: fieldFixed, value: value, hint: hint}, value)
}

func (f *form) checkbox(id, label string, on bool) *formField {
	fd := &formField{id: id, label: label, kind: fieldCheck, check: on}
	f.fields = append(f.fields, fd)
	return fd
}

func (f *form) add(fd *formField, value string) *formField {
	if fd.build != nil {
		fd.huh = f.dress(fd.build(value))
	}
	fd.initial = value
	f.fields = append(f.fields, fd)
	return fd
}

func (f *form) dress(h huh.Field) huh.Field {
	return h.WithTheme(huhTheme(f.sty)).WithKeyMap(huhKeys()).WithWidth(f.fieldWidth())
}

// reset gives a field a new value (and initial value), keeping its focus.
func (f *form) reset(fd *formField, value string) tea.Cmd {
	fd.initial = value
	if fd.build == nil {
		fd.value = value
		return nil
	}
	fd.huh = f.dress(fd.build(value))
	if f.focus >= 0 && f.fields[f.focus] == fd {
		return fd.huh.Focus()
	}
	return nil
}

// changed reports whether the user changed field id.
func (f *form) changed(id string) bool {
	fd := f.field(id)
	return fd != nil && strings.TrimSpace(fd.get()) != strings.TrimSpace(fd.initial)
}

// rebase takes the fresh rov.yaml entry after a conflict: fields the user
// left alone show the new values, edited ones keep the user's input, and
// the next save changes only what the user changed.
func (f *form) rebase(fresh config.Process) tea.Cmd {
	values := map[string]string{"name": fresh.Name, "cmd": fresh.Cmd, "cwd": formCwd(fresh.Cwd), "env": envText(fresh.Env), "desc": fresh.Description, "docker": fresh.Docker}
	var cmds []tea.Cmd
	for _, fd := range f.fields {
		v, ok := values[fd.id]
		if !ok {
			continue
		}
		if f.changed(fd.id) {
			fd.initial = v
		} else {
			cmds = append(cmds, f.reset(fd, v))
		}
	}
	return tea.Batch(cmds...)
}

func (f *form) field(id string) *formField {
	for _, fd := range f.fields {
		if fd.id == id {
			return fd
		}
	}
	return nil
}

func (f *form) val(id string) string {
	if fd := f.field(id); fd != nil {
		return fd.get()
	}
	return ""
}

// start focuses the first editable field. A prefilled form (edit, save)
// shows its problems at once; a new one once a field was left.
func (f *form) start() tea.Cmd {
	f.showAll = f.kind != formNew
	f.focus = -1
	return f.move(1)
}

// move focuses the next (dir 1) or previous (-1) editable field.
func (f *form) move(dir int) tea.Cmd {
	var cmds []tea.Cmd
	if f.focus >= 0 && f.focus < len(f.fields) {
		cur := f.fields[f.focus]
		f.touched[cur.id] = true
		if cur.huh != nil {
			cmds = append(cmds, cur.huh.Blur())
		}
	}
	n := len(f.fields)
	for i := 1; i <= n; i++ {
		j := ((f.focus+dir*i)%n + n) % n
		if f.fields[j].kind != fieldFixed {
			f.focus = j
			break
		}
	}
	if fd := f.fields[f.focus]; fd.huh != nil {
		cmds = append(cmds, fd.huh.Focus())
	}
	f.validate()
	return tea.Batch(cmds...)
}

// update passes a message to the focused huh field.
func (f *form) update(msg tea.Msg) tea.Cmd {
	if _, ok := msg.(cursor.BlinkMsg); ok {
		// a steady cursor: nothing animates but spinners
		return nil
	}
	if f.focus < 0 || f.focus >= len(f.fields) || f.fields[f.focus].huh == nil {
		return nil
	}
	fd := f.fields[f.focus]
	mdl, cmd := fd.huh.Update(msg)
	if hf, ok := mdl.(huh.Field); ok {
		fd.huh = hf
	}
	f.validate()
	return cmd
}

// validate sets each field's error: the spec's inline validation.
func (f *form) validate() bool {
	ok := true
	set := func(id, msg string) {
		if fd := f.field(id); fd != nil {
			fd.err = msg
			if msg != "" {
				ok = false
			}
		}
	}
	name := strings.TrimSpace(f.val("name"))
	nameErr := ""
	if f.kind != formEditAdhoc {
		if name == "" {
			nameErr = "the name is required"
		} else if config.ValidName(name) != nil {
			nameErr = "use a-z, 0-9, '-' and '_', starting with a letter or digit"
		} else if name != f.orig || f.kind == formNew {
			switch {
			case f.kind == formEditCfg && f.running && name != f.orig:
				nameErr = f.orig + " is running — stop it first to rename it"
			case slices.Contains(f.cfgNames, name) && !(f.kind == formEditCfg && name == f.orig):
				nameErr = name + " is already in rov.yaml"
			case slices.Contains(f.adhoc, name) && !(f.kind == formSave && name == f.orig):
				nameErr = name + " is taken by an ad-hoc process"
			}
		}
	}
	set("name", nameErr)
	if strings.TrimSpace(f.val("cmd")) == "" {
		set("cmd", "the command is required")
	} else {
		set("cmd", "")
	}
	cwd := strings.TrimSpace(f.val("cwd"))
	cwdErr := cwdProblem(f.root, cwd)
	if cwdErr != "" && f.kind == formEditAdhoc && cwd == strings.TrimSpace(f.initial("cwd")) && filepath.IsAbs(cwd) {
		// an ad-hoc process spawned outside the root by an older rov
		if fi, err := os.Stat(cwd); err == nil && fi.IsDir() {
			cwdErr = ""
		}
	}
	set("cwd", cwdErr)
	if _, err := parseEnvLines(f.val("env")); err != nil {
		set("env", err.Error())
	} else {
		set("env", "")
	}
	return ok
}

// cwdProblem checks a working dir as typed: empty (the root), relative to
// the root, or absolute inside it; it must be an existing directory. The
// message is short — one line, the path as typed.
func cwdProblem(root, cwd string) string {
	rel, ok := rootRel(root, cwd)
	if !ok {
		return "outside the project root: " + cwd
	}
	fi, err := os.Stat(filepath.Join(root, rel))
	switch {
	case err != nil:
		return "no such directory: " + cwd
	case !fi.IsDir():
		return "not a directory: " + cwd
	}
	return ""
}

// rootRel returns a typed working dir relative to the root ("" for the
// root itself) and whether it lies inside it.
func rootRel(root, cwd string) (string, bool) {
	if cwd == "" || cwd == "." {
		return "", true
	}
	dir := cwd
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(root, dir)
	}
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		dir = r
	}
	rel, err := filepath.Rel(root, filepath.Clean(dir))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if rel == "." {
		rel = ""
	}
	return rel, true
}

// initial is the value field id opened with.
func (f *form) initial(id string) string {
	if fd := f.field(id); fd != nil {
		return fd.initial
	}
	return ""
}

// parseEnvLines reads one KEY=VALUE per line; blank lines are ignored.
func parseEnvLines(s string) ([]config.EnvVar, error) {
	var out []config.EnvVar
	seen := map[string]bool{}
	for i, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			return nil, errors.New("line " + itoa(i+1) + ": use KEY=VALUE")
		}
		if err := config.ValidEnvKey(k); err != nil {
			return nil, errors.New("line " + itoa(i+1) + ": " + err.Error())
		}
		if seen[k] {
			return nil, errors.New(k + " is set twice")
		}
		seen[k] = true
		out = append(out, config.EnvVar{Key: k, Value: v})
	}
	return out, nil
}

func envText(env []config.EnvVar) string {
	parts := make([]string, len(env))
	for i, e := range env {
		parts[i] = e.String()
	}
	return strings.Join(parts, "\n")
}

// view renders the form box.
func (f *form) view() string {
	s := f.sty
	iw := f.innerWidth()
	fw := f.fieldWidth()
	var lines []string
	lines = append(lines, "")
	errLine := func(id, msg string) {
		ls := f.cw.wrap("✗ "+msg, fw)
		if id == "cwd" {
			// a path: one line, cut with "…"
			ls = []string{f.cw.fit("✗ "+msg, fw)}
		}
		for _, l := range ls {
			lines = append(lines, strings.Repeat(" ", formLabelW+2)+s.failed.Render(l))
		}
	}
	for i, fd := range f.fields {
		focused := i == f.focus
		mark := " "
		lst := s.idle
		if focused {
			mark = s.accent.Render("▌")
			lst = s.accent.Bold(true)
		}
		var body []string
		switch fd.kind {
		case fieldCheck:
			box := "[ ]"
			if fd.check {
				box = "[x]"
			}
			st := lipgloss.NewStyle()
			if focused {
				st = s.accent.Bold(true)
			}
			lines = append(lines, mark+" "+st.Render(box)+" "+fd.label)
			continue
		case fieldFixed:
			body = []string{fd.value}
		case fieldInput:
			if focused {
				body = []string{fd.huh.View()}
			} else if v := fd.get(); v != "" {
				// show the start of the value, not where the cursor left it
				body = []string{f.cw.fit(v, fw)}
			} else {
				body = []string{s.idle.Render(f.cw.fit(fd.ph, fw))}
			}
		default:
			body = strings.Split(strings.TrimRight(fd.huh.View(), "\n"), "\n")
		}
		for j, b := range body {
			label := strings.Repeat(" ", formLabelW)
			if j == 0 {
				label = lst.Render(f.cw.pad(fd.label, formLabelW))
			}
			lines = append(lines, mark+" "+label+f.cw.fit(b, fw))
		}
		if fd.hint != "" {
			lines = append(lines, strings.Repeat(" ", formLabelW+2)+s.idle.Render(f.cw.fit(fd.hint, fw)))
		}
		if fd.err != "" && (f.showAll || f.touched[fd.id] || (focused && fd.kind != fieldInput)) {
			errLine(fd.id, fd.err)
		}
	}
	lines = append(lines, "")
	if f.notice != "" {
		lines = append(lines, " "+s.warn.Render(f.cw.fit("▲ "+f.notice, iw-2)))
	}
	if f.err != "" {
		for _, l := range f.cw.wrap("✗ "+f.err, iw-2) {
			lines = append(lines, " "+s.failed.Render(l))
		}
	}
	foot := s.key.Render("esc") + " " + s.label.Render("cancel") + "   " + s.key.Render("ctrl+s") + " " + s.label.Render("save")
	if f.saving {
		foot = s.warn.Render("saving…")
	}
	lines = append(lines, strings.Repeat(" ", max(0, iw-f.cw.width(foot)-2))+foot)
	for i := range lines {
		lines[i] = f.cw.pad(f.cw.fit(lines[i], iw), iw)
	}
	return strings.Join(boxLines(s, f.cw, f.title, lines, iw+2), "\n")
}

// boxLines frames lines (iw wide) in an accent rounded box.
func boxLines(s *styles, c cells, title string, lines []string, w int) []string {
	bc := s.accent
	iw := w - 2
	fill := max(0, iw-3-c.width(title))
	out := []string{bc.Render("╭─ ") + lipgloss.NewStyle().Bold(true).Render(title) + bc.Render(" "+strings.Repeat("─", fill)+"╮")}
	for _, l := range lines {
		out = append(out, bc.Render("│")+l+bc.Render("│"))
	}
	return append(out, bc.Render("╰"+strings.Repeat("─", iw)+"╯"))
}

// formKey handles keys while a form is open.
func (m *Model) formKey(k tea.KeyPressMsg) tea.Cmd {
	f := m.form
	if f.saving {
		if k.String() == "esc" {
			m.form = nil
		}
		return nil
	}
	ks := k.String()
	cur := f.fields[f.focus]
	switch ks {
	case "esc":
		m.form = nil
		return nil
	case "ctrl+s":
		return m.saveForm()
	case "tab", "down":
		if ks == "down" && cur.kind == fieldText {
			break
		}
		return f.move(1)
	case "shift+tab", "up":
		if ks == "up" && cur.kind == fieldText {
			break
		}
		return f.move(-1)
	case "enter":
		if cur.kind == fieldText {
			break
		}
		if cur.kind == fieldCheck {
			cur.check = !cur.check
			return nil
		}
		if f.focus == lastEditable(f) {
			return m.saveForm()
		}
		return f.move(1)
	case "space", "x":
		if cur.kind == fieldCheck {
			cur.check = !cur.check
			return nil
		}
	}
	if cur.kind == fieldCheck {
		return nil
	}
	return f.update(k)
}

func lastEditable(f *form) int {
	for i := len(f.fields) - 1; i >= 0; i-- {
		if f.fields[i].kind != fieldFixed {
			return i
		}
	}
	return -1
}

// --- opening the forms ------------------------------------------------------

// dockerOptions are none, the current value (whether or not it has
// containers), then the compose projects found under the root — every
// container docker lists, stopped ones included — and those other rov.yaml
// entries name, sorted.
func (m *Model) dockerOptions(current string) []huh.Option[string] {
	opts := []huh.Option[string]{huh.NewOption("— none —", "")}
	if current != "" {
		opts = append(opts, huh.NewOption(current, current))
	}
	var found []string
	for _, c := range m.containers {
		found = append(found, c.Project)
	}
	for _, p := range m.b.Config().Processes {
		found = append(found, p.Docker)
	}
	slices.Sort(found)
	for _, p := range slices.Compact(found) {
		if p != "" && p != current {
			opts = append(opts, huh.NewOption(p, p))
		}
	}
	return opts
}

// formCwd is a stored working dir as the form shows it: "" for the root
// (the field's placeholder says ".").
func formCwd(cwd string) string {
	if cwd == "." {
		return ""
	}
	return cwd
}

func (m *Model) positionOptions(exclude string) []huh.Option[string] {
	opts := []huh.Option[string]{huh.NewOption("first", "first")}
	for _, n := range m.b.Config().Names() {
		if n != exclude {
			opts = append(opts, huh.NewOption("after "+n, "after "+n))
		}
	}
	return opts
}

// defaultPosition is "after the selected row": its config process, the
// owner of a selected container, else the end.
func (m *Model) defaultPosition() string {
	names := m.b.Config().Names()
	if r := m.selected(); r != nil && r.proc != nil && r.proc.Kind == core.KindConfig {
		return "after " + r.proc.Name
	}
	if len(names) == 0 {
		return "first"
	}
	return "after " + names[len(names)-1]
}

func (m *Model) openNew() tea.Cmd {
	f := m.newForm(formNew, "new process")
	f.input("name", "Name", "", "e.g. mock-s3")
	f.input("cmd", "Command", "", "shell command")
	f.input("cwd", "Working dir", "", ".")
	f.text("env", "Env", "", "(one KEY=VALUE per line)")
	f.input("desc", "Description", "", "")
	f.choice("docker", "Docker", m.dockerOptions(""), "")
	f.choice("pos", "Position", m.positionOptions(""), m.defaultPosition())
	f.checkbox("start", "start after saving", true)
	m.form = f
	return f.start()
}

func (m *Model) openEdit() tea.Cmd {
	r := m.selected()
	if r == nil || r.kind != rowProc {
		m.setFlash("e edits a process", false)
		return nil
	}
	p := r.proc
	if p.Kind == core.KindAdhoc {
		f := m.newForm(formEditAdhoc, "edit "+p.Name+" (ad-hoc)")
		f.orig, f.running = p.Name, p.Running()
		f.fixed("name", "Name", p.Name, "")
		f.input("cmd", "Command", p.Cmd, "")
		f.input("cwd", "Working dir", formCwd(p.Cwd), ".")
		f.text("env", "Env", envText(m.adhocEnv(p.Name)), "(one KEY=VALUE per line)")
		if p.Running() {
			f.checkbox("restart", "restart now", false)
		}
		m.form = f
		return f.start()
	}
	cp, ok := m.b.Config().Get(p.Name)
	if !ok {
		return nil
	}
	f := m.newForm(formEditCfg, "edit "+p.Name)
	f.orig, f.running = p.Name, p.Running()
	f.input("name", "Name", cp.Name, "")
	f.input("cmd", "Command", cp.Cmd, "")
	f.input("cwd", "Working dir", formCwd(cp.Cwd), ".")
	f.text("env", "Env", envText(cp.Env), "(one KEY=VALUE per line)")
	f.input("desc", "Description", cp.Description, "")
	f.choice("docker", "Docker", m.dockerOptions(cp.Docker), cp.Docker)
	m.form = f
	return f.start()
}

func (m *Model) openSave() tea.Cmd {
	r := m.selected()
	if r == nil || r.kind != rowProc || r.proc.Kind != core.KindAdhoc {
		m.setFlash("s saves an ad-hoc process into rov.yaml", false)
		return nil
	}
	p := r.proc
	f := m.newForm(formSave, "save "+p.Name+" into rov.yaml")
	f.orig, f.running = p.Name, p.Running()
	f.input("name", "Name", p.Name, "")
	f.input("cmd", "Command", p.Cmd, "")
	f.input("cwd", "Working dir", formCwd(p.Cwd), ".")
	f.text("env", "Env", envText(m.adhocEnv(p.Name)), "(one KEY=VALUE per line)")
	f.input("desc", "Description", "", "")
	f.choice("docker", "Docker", m.dockerOptions(""), "")
	last := "first"
	if names := m.b.Config().Names(); len(names) > 0 {
		last = "after " + names[len(names)-1]
	}
	f.choice("pos", "Position", m.positionOptions(""), last)
	if !p.Running() {
		f.checkbox("start", "start after saving", false)
	}
	m.form = f
	return f.start()
}

// adhocEnv reads the stored env of an ad-hoc session (values included;
// Status only carries the keys).
func (m *Model) adhocEnv(name string) []config.EnvVar {
	for _, s := range m.sessions {
		if s.Name != name {
			continue
		}
		raw := s.Option(core.OptEnv)
		if raw == "" {
			return nil
		}
		var kv []string
		if err := json.Unmarshal([]byte(raw), &kv); err != nil {
			return nil
		}
		out := make([]config.EnvVar, 0, len(kv))
		for _, e := range kv {
			k, v, _ := strings.Cut(e, "=")
			out = append(out, config.EnvVar{Key: k, Value: v})
		}
		return out
	}
	return nil
}

// refreshForm brings an open form in line with the latest status: whether
// the process it edits runs now decides whether it may be renamed.
func (m *Model) refreshForm() {
	f := m.form
	if f == nil || f.orig == "" || m.st == nil {
		return
	}
	p := m.st.Get(f.orig)
	running := p != nil && p.Running()
	if running != f.running {
		f.running = running
		f.validate()
	}
}

// --- saving -----------------------------------------------------------------

// configWrittenMsg reports a write of rov.yaml.
type configWrittenMsg struct {
	what  string // flash on success
	data  []byte // the file as written
	err   error
	quiet bool   // J/K: no flash on success
	start string // start this process afterwards
	form  bool   // from the open form
}

func (m *Model) saveForm() tea.Cmd {
	f := m.form
	m.refreshForm()
	f.showAll = true
	f.notice, f.err = "", ""
	if !f.validate() {
		for i, fd := range f.fields {
			if fd.err != "" {
				var cmds []tea.Cmd
				if cur := f.fields[f.focus]; cur.huh != nil {
					cmds = append(cmds, cur.huh.Blur())
				}
				f.focus = i
				if fd.huh != nil {
					cmds = append(cmds, fd.huh.Focus())
				}
				return tea.Batch(cmds...)
			}
		}
		return nil
	}
	env, _ := parseEnvLines(f.val("env"))
	name := strings.TrimSpace(f.val("name"))
	cmd := strings.TrimSpace(f.val("cmd"))
	cwd := strings.TrimSpace(f.val("cwd"))
	if rel, ok := rootRel(f.root, cwd); ok {
		cwd = rel // rov.yaml and the session store it relative to the root
	}
	b, baseline := m.b, f.baseline

	if f.kind == formEditAdhoc {
		o := core.SpawnOptions{Name: f.orig, Cmd: cmd, Dir: cwd, Env: env}
		m.form = nil
		if fd := f.field("restart"); fd != nil && fd.check {
			o.Replace = true
			return m.run(f.orig, f.orig, "restarting", false, func(ctx context.Context) (string, error) { return b.Spawn(ctx, o) })
		}
		return m.run(f.orig, f.orig, "saving", false, func(ctx context.Context) (string, error) { return b.Redefine(ctx, o) })
	}

	var op configedit.Op
	what := ""
	start := ""
	switch f.kind {
	case formNew, formSave:
		proc := config.Process{Name: name, Cmd: cmd, Cwd: cwd, Env: env, Description: strings.TrimSpace(f.val("desc")), Docker: f.val("docker")}
		op = configedit.Add{Proc: proc, Pos: parsePosition(f.val("pos"))}
		what = "rov.yaml: added " + name
		if fd := f.field("start"); fd != nil && fd.check {
			start = name
		}
	case formEditCfg:
		// only what the user changed: a field an agent edited meanwhile
		// and the user left alone is not written back
		var patch configedit.Patch
		changed := false
		str := func(dst **string, id, now string) {
			if f.changed(id) {
				v := now
				*dst, changed = &v, true
			}
		}
		str(&patch.Cmd, "cmd", cmd)
		str(&patch.Cwd, "cwd", cwd)
		str(&patch.Description, "desc", strings.TrimSpace(f.val("desc")))
		str(&patch.Docker, "docker", f.val("docker"))
		if f.changed("env") {
			patch.Env, changed = &env, true
		}
		if name != f.orig {
			patch.Rename, changed = &name, true
		}
		if !changed {
			m.form = nil
			m.setFlash("no changes", false)
			return nil
		}
		op = configedit.Set{Name: f.orig, Patch: patch}
		what = "rov.yaml: updated " + f.orig
		if patch.Rename != nil {
			what = "rov.yaml: renamed " + f.orig + " to " + name
		} else if f.running && (patch.Cmd != nil || patch.Cwd != nil || patch.Env != nil) {
			what += " — r restarts it with the change"
		}
	}
	f.saving = true
	rename := f.kind == formEditCfg && name != f.orig
	orig := f.orig
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
		defer cancel()
		data, err := b.WriteConfig(ctx, baseline, op)
		if err == nil && rename {
			// The old name's stopped session would linger as ad-hoc.
			_ = b.DropSession(ctx, orig)
		}
		return configWrittenMsg{what: what, data: data, err: err, start: start, form: true}
	}
}

func parsePosition(s string) configedit.Position {
	switch {
	case s == "first":
		return configedit.Position{First: true}
	case strings.HasPrefix(s, "after "):
		return configedit.Position{After: strings.TrimPrefix(s, "after ")}
	}
	return configedit.Position{Last: true}
}

// configWritten applies the result of a rov.yaml write.
func (m *Model) configWritten(msg configWrittenMsg) tea.Cmd {
	var conflict *configedit.ConflictError
	var rebase tea.Cmd
	switch {
	case errors.As(msg.err, &conflict):
		m.cfgBytes = conflict.Current
		if f := m.form; msg.form && f != nil {
			f.saving = false
			f.baseline = conflict.Current
			f.notice = "rov.yaml changed on disk — reloaded"
			if fresh, _ := config.Parse(m.b.ConfigPath(), conflict.Current); fresh != nil {
				f.cfgNames = fresh.Names()
				if f.kind == formEditCfg {
					if p, ok := fresh.Get(f.orig); ok {
						rebase = f.rebase(p)
					} else {
						f.err = f.orig + " was removed from rov.yaml"
					}
				}
			}
			f.validate()
		} else {
			m.setFlash("rov.yaml changed on disk — reloaded; try again", true)
		}
	case msg.err != nil:
		if msg.form && m.form != nil {
			m.form.saving = false
			m.form.err = firstLine(msg.err.Error())
		} else {
			m.flashErr("rov.yaml", msg.err)
		}
	default:
		m.cfgBytes = msg.data
		if msg.form {
			m.form = nil
		}
		if !msg.quiet {
			m.setFlash(msg.what, false)
		}
	}
	m.recompose()
	cmds := []tea.Cmd{m.pollTmux(), rebase}
	if msg.err == nil && msg.start != "" {
		name, b := msg.start, m.b
		cmds = append(cmds, m.run(name, name, "starting", false, func(ctx context.Context) (string, error) { return b.Up(ctx, name) }))
	}
	return tea.Batch(cmds...)
}
