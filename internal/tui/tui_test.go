package tui

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/exp/golden"
	"github.com/charmbracelet/x/exp/teatest/v2"

	"github.com/labsbit/rov/internal/core"
	"github.com/labsbit/rov/internal/testenv"
	"github.com/labsbit/rov/internal/tmux"
)

// The TUI is tested through teatest: a real Bubble Tea program over the
// fake backend, driven by key presses, with its view snapshotted against
// golden files at 140×35 and 70×20, on a dark and a light background
// (testdata/<Test>/<size>/<dark|light>.golden hold the view with its
// colours, <size>.txt the plain text). Update them with -update.

var sizes = []struct{ w, h int }{{140, 35}, {70, 20}}

// harness runs one program.
type harness struct {
	t  *testing.T
	f  *fakeBackend
	tm *teatest.TestModel
}

func start(t *testing.T, f *fakeBackend, w, h int, dark bool) *harness {
	t.Helper()
	prof := colorprofile.TrueColor
	m := New(f, Options{
		Now:     func() time.Time { return testenv.SampleTime },
		Home:    f.home,
		Manual:  true,
		Dark:    &dark,
		Profile: &prof,
	})
	tm := teatest.NewTestModel(t, m, teatest.WithInitialTermSize(w, h))
	hs := &harness{t: t, f: f, tm: tm}
	t.Cleanup(func() {
		_ = tm.Quit()
		tm.WaitFinished(t, teatest.WithFinalTimeout(5*time.Second))
	})
	// wait for the first poll of every source
	hs.wait(func(v string) bool {
		return strings.Contains(v, "gateway") && strings.Contains(v, "old-mock") && strings.Contains(v, ":8080")
	})
	return hs
}

// view returns the current rendered view (with colours).
func (h *harness) view() string {
	ch := make(chan string, 1)
	h.tm.Send(snapshotMsg{ch})
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		h.t.Fatal("no view")
	}
	return ""
}

// wait returns the view once cond holds for its plain text and it stopped
// changing.
func (h *harness) wait(cond func(plain string) bool) string {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	prev := ""
	for {
		v := h.view()
		if cond(ansi.Strip(v)) && v == prev {
			return v
		}
		prev = v
		if time.Now().After(deadline) {
			h.t.Fatalf("view never matched:\n%s", ansi.Strip(v))
		}
		time.Sleep(60 * time.Millisecond)
	}
}

func (h *harness) waitFor(text string) string {
	h.t.Helper()
	return h.wait(func(v string) bool { return strings.Contains(v, text) })
}

// keys sends key presses: single characters, or names like "tab", "esc",
// "enter", "space", "ctrl+s", "shift+tab".
func (h *harness) keys(keys ...string) {
	for _, k := range keys {
		h.tm.Send(keyMsg(k))
	}
}

func (h *harness) typeText(s string) { h.tm.Type(s) }

func keyMsg(k string) tea.KeyPressMsg {
	switch k {
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "space":
		return tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	case "backspace":
		return tea.KeyPressMsg{Code: tea.KeyBackspace}
	case "ctrl+s":
		return tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl}
	case "ctrl+u":
		return tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}
	case "up":
		return tea.KeyPressMsg{Code: tea.KeyUp}
	case "down":
		return tea.KeyPressMsg{Code: tea.KeyDown}
	}
	r := []rune(k)[0]
	return tea.KeyPressMsg{Code: r, Text: k}
}

// snap compares a view with the golden files of the current test.
func snap(t *testing.T, v string) {
	t.Helper()
	golden.RequireEqual(t, v)
}

// plainGolden compares the plain text of a view with <dir>.txt, shared by
// the dark and light runs of one size.
func plainGolden(t *testing.T, name, v string) {
	t.Helper()
	path := filepath.Join("testdata", name+".txt")
	plain := ansi.Strip(v)
	if f := flag.Lookup("update"); f != nil && f.Value.String() == "true" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(plain), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != plain {
		t.Fatalf("%s differs:\n--- want\n%s\n--- got\n%s", path, want, plain)
	}
}

// scenario runs fn in every size and profile and snapshots what it returns
// (one view per name).
func scenario(t *testing.T, setup func(f *fakeBackend), fn func(h *harness) map[string]string) {
	for _, sz := range sizes {
		for _, dark := range []bool{true, false} {
			bg := "light"
			if dark {
				bg = "dark"
			}
			size := fmt.Sprintf("%dx%d", sz.w, sz.h)
			t.Run(size+"/"+bg, func(t *testing.T) {
				f := newFake(t)
				f.writeLog("gateway", gatewayLog(40))
				if setup != nil {
					setup(f)
				}
				h := start(t, f, sz.w, sz.h, dark)
				views := fn(h)
				for name, v := range views {
					t.Run(name, func(t *testing.T) {
						snap(t, v)
						if dark {
							plainGolden(t, strings.TrimSuffix(t.Name(), "/dark/"+name)+"/"+name, v)
						} else {
							plainGolden(t, strings.TrimSuffix(t.Name(), "/light/"+name)+"/"+name, v)
						}
					})
				}
			})
		}
	}
}

// gatewayLog is a coloured gateway log of n lines.
func gatewayLog(n int) []string {
	var out []string
	for i := range n {
		ts := fmt.Sprintf("08:%02d:%02d", 1+i/60, i%60)
		switch {
		case i%10 == 7:
			out = append(out, fmt.Sprintf("\x1b[2m%s\x1b[0m \x1b[33mWRN\x1b[0m upstream slow \x1b[36mprovider\x1b[0m=serper p95=%dms", ts, 700+i))
		case i%5 == 1:
			out = append(out, fmt.Sprintf("\x1b[2m%s\x1b[0m \x1b[32mINF\x1b[0m \x1b[1mprovider ready\x1b[0m name=brave latency=%dms", ts, 10+i))
		default:
			out = append(out, fmt.Sprintf("\x1b[2m%s\x1b[0m \x1b[32mINF\x1b[0m search q=\"rust tui\" took=%dms results=%d", ts, 100+i, i%9))
		}
	}
	return out
}

func TestSelection(t *testing.T) {
	scenario(t, nil, func(h *harness) map[string]string {
		first := h.waitFor("─ infra ")
		h.keys("j", "j", "j") // infra → clickhouse → redis → gateway
		return map[string]string{"infra": first, "gateway": h.waitFor("─ gateway ")}
	})
}

func TestFocusSwitch(t *testing.T) {
	scenario(t, nil, func(h *harness) map[string]string {
		h.keys("j", "j", "j") // gateway
		h.waitFor("search gateway")
		h.keys("l")
		out := h.waitFor("OUTPUT")
		h.keys("tab")
		list := h.waitFor("LIST")
		return map[string]string{"output": out, "list": list}
	})
}

func TestFollowPause(t *testing.T) {
	scenario(t, func(f *fakeBackend) { f.writeLog("gateway", gatewayLog(120)) }, func(h *harness) map[string]string {
		h.keys("j", "j", "j", "l")
		h.waitFor("120  ")
		h.keys("k", "k", "k", "k", "k")
		h.f.appendLog("gateway", []string{"\x1b[31mERR\x1b[0m late line one", "\x1b[31mERR\x1b[0m late line two"})
		paused := h.waitFor("↓ 2 new lines")
		h.keys("G")
		resumed := h.waitFor("late line two")
		if strings.Contains(ansi.Strip(resumed), "new lines") {
			h.t.Error("marker still shown after G")
		}
		return map[string]string{"paused": paused, "resumed": resumed}
	})
}

func TestSearchHighlight(t *testing.T) {
	scenario(t, nil, func(h *harness) map[string]string {
		h.keys("j", "j", "j", "/")
		h.typeText("provider")
		typing := h.waitFor("/provider")
		h.keys("enter")
		// the first match at or below the top of the window
		found := h.wait(func(v string) bool { return counter(v) > 0 })
		first := counter(ansi.Strip(found))
		h.keys("n", "n")
		want := (first+1)%12 + 1
		next := h.wait(func(v string) bool { return counter(v) == want })
		// the highlight is reverse video over the line's own colours
		if !strings.Contains(found, "\x1b[7m") {
			h.t.Error("no reverse-video match in the view")
		}
		return map[string]string{"typing": typing, "found": found, "next": next}
	})
}

// counter reads the current match of the "3/12" search counter (0 if none).
func counter(v string) int {
	lines := strings.Split(v, "\n")
	last := lines[len(lines)-1]
	i := strings.Index(last, "/12")
	if i < 0 {
		return 0
	}
	j := i
	for j > 0 && last[j-1] >= '0' && last[j-1] <= '9' {
		j--
	}
	n := 0
	fmt.Sscanf(last[j:i], "%d", &n)
	return n
}

func TestSpawnAppears(t *testing.T) {
	var full []tmux.Session
	scenario(t, func(f *fakeBackend) {
		full = f.sessions
		var without []tmux.Session
		for _, s := range f.sessions {
			if s.Name != "mock-stripe" {
				without = append(without, s)
			}
		}
		f.sessions = without
	}, func(h *harness) map[string]string {
		before := h.view()
		if strings.Contains(ansi.Strip(before), "mock-stri") {
			h.t.Fatal("mock-stripe listed before it was spawned")
		}
		h.f.setSessions(full) // an agent ran rov spawn mock-stripe
		h.tm.Send(refreshMsg{})
		return map[string]string{"spawned": h.waitFor("mock-stri")}
	})
}

func TestContainerSubRows(t *testing.T) {
	scenario(t, func(f *fakeBackend) {
		f.dockerLogs["sag-redis"] = "1:M 07 Nov 2026 08:00:01.000 * Ready to accept connections tcp\n1:M 07 Nov 2026 08:10:01.000 * \x1b[1mDB saved on disk\x1b[0m\n"
	}, func(h *harness) map[string]string {
		h.keys("j", "j") // infra → clickhouse → redis
		v := h.waitFor("saved on disk")
		return map[string]string{"redis": v}
	})
}

func TestRemoveConfirm(t *testing.T) {
	scenario(t, nil, func(h *harness) map[string]string {
		h.keys("G") // last selectable: the unowned container; up to mock-stripe
		h.keys("k")
		h.waitFor("─ mock-stripe ")
		h.keys("x")
		ask := h.waitFor("remove mock-stripe?")
		h.keys("y")
		h.waitFor("mock-stripe: removed")
		if got := h.f.actionLog(); len(got) != 1 || got[0] != "rm mock-stripe" {
			h.t.Errorf("actions = %q", got)
		}
		return map[string]string{"confirm": ask}
	})
}

func TestFormValidation(t *testing.T) {
	scenario(t, nil, func(h *harness) map[string]string {
		h.keys("j", "j", "j") // gateway: the new entry goes after it
		h.waitFor("search gateway")
		h.keys("n")
		h.waitFor("new process")
		h.typeText("Bad Name")
		h.keys("tab")
		bad := h.waitFor("use a-z")
		h.keys("esc")
		h.waitFor("LIST")
		h.keys("e")
		h.waitFor("edit gateway")
		h.keys("tab", "ctrl+u", "tab")
		edit := h.waitFor("the command is required")
		h.keys("ctrl+s")
		h.waitFor("the command is required")
		h.keys("esc")
		return map[string]string{"new": bad, "edit": edit}
	})
}

func TestNewProcessSaved(t *testing.T) {
	f := newFake(t)
	h := start(t, f, 140, 35, true)
	h.keys("j", "j", "j", "n") // after gateway
	h.waitFor("new process")
	h.typeText("mock-s3")
	h.keys("tab")
	h.typeText("go run ./tools/mock-s3")
	h.keys("tab", "tab")
	h.typeText("AWS_REGION=eu-central-1")
	h.keys("tab")
	h.typeText("local S3")
	h.keys("ctrl+s")
	got := waitFile(t, f.ConfigPath(), func(s string) bool { return strings.Contains(s, "mock-s3") })
	want := strings.Replace(sampleYAML, "  tracking:", `  mock-s3:
    cmd: go run ./tools/mock-s3
    env:
      AWS_REGION: eu-central-1
    description: local S3
  tracking:`, 1)
	if got != want {
		t.Errorf("rov.yaml =\n%s\nwant\n%s", got, want)
	}
	h.waitFor("mock-s3: started")
	if a := f.actionLog(); len(a) != 1 || a[0] != "up mock-s3" {
		t.Errorf("actions = %q", a)
	}
}

func TestReorder(t *testing.T) {
	scenario(t, nil, func(h *harness) map[string]string {
		h.keys("j", "j", "j") // gateway
		h.waitFor("search gateway")
		h.keys("J")
		yaml := waitFile(h.t, h.f.ConfigPath(), func(s string) bool {
			return strings.Index(s, "tracking:") < strings.Index(s, "gateway:")
		})
		want := strings.Replace(sampleYAML, "  gateway:\n    cmd: just gateway\n    description: search gateway\n  tracking:    { cmd: just tracking }\n",
			"  tracking:    { cmd: just tracking }\n  gateway:\n    cmd: just gateway\n    description: search gateway\n", 1)
		if yaml != want {
			h.t.Errorf("after J rov.yaml =\n%s\nwant\n%s", yaml, want)
		}
		moved := h.wait(func(v string) bool {
			return strings.Index(v, "tracking") < strings.Index(v, "▌● gateway") && strings.Contains(v, "─ gateway ")
		})
		h.keys("K")
		waitFile(h.t, h.f.ConfigPath(), func(s string) bool { return s == sampleYAML })
		back := h.wait(func(v string) bool { return strings.Index(v, "▌● gateway") < strings.Index(v, "● tracking") })
		return map[string]string{"moved": moved, "back": back}
	})
}

// A broken rov.yaml must not crash the TUI: the problem shows in the top
// bar and the tmux processes stay listed.
func TestBrokenConfig(t *testing.T) {
	f := newFake(t)
	h := start(t, f, 140, 35, true)
	f.writeConfig("processes:\n  gateway: { cmd: just gateway }\n  Bad: { cmd: x }\n")
	h.tm.Send(refreshMsg{})
	v := h.waitFor("✗ rov.yaml")
	if !strings.Contains(ansi.Strip(v), "mock-stripe") || !strings.Contains(ansi.Strip(v), "gateway") {
		t.Errorf("processes gone with a broken rov.yaml:\n%s", ansi.Strip(v))
	}
	f.writeConfig("processes: [not, a, map\n")
	h.tm.Send(refreshMsg{})
	v = h.waitFor("✗ rov.yaml")
	if !strings.Contains(ansi.Strip(v), "gateway") {
		t.Errorf("unparsable rov.yaml dropped the processes:\n%s", ansi.Strip(v))
	}
}

// Selection follows the row's identity when rows appear or disappear, and
// moves to the neighbour when the selected row goes.
func TestSelectionStable(t *testing.T) {
	f := newFake(t)
	m := New(f, Options{Now: func() time.Time { return testenv.SampleTime }, Manual: true})
	m.sessions, m.snap, m.containers = f.sessions, f.snap, f.containers
	m.recompose()
	sel := func(key string) {
		for i := range m.rows {
			if m.rows[i].key == key {
				m.selectIdx(i)
				return
			}
		}
		t.Fatalf("no row %s", key)
	}
	sel("p:mock-stripe")
	// a new ad-hoc process sorts before mock-stripe (older): selection stays
	extra := tmux.Session{Name: "early", PaneID: "%9", PanePID: 1, Options: map[string]string{core.OptKind: "adhoc", core.OptCmd: "sleep 1", core.OptStarted: "1"}}
	m.sessions = append(append([]tmux.Session(nil), m.sessions...), extra)
	m.recompose()
	if m.selKey != "p:mock-stripe" {
		t.Fatalf("selection jumped to %s", m.selKey)
	}
	// mock-stripe goes away: its neighbour is selected
	var rest []tmux.Session
	for _, s := range m.sessions {
		if s.Name != "mock-stripe" {
			rest = append(rest, s)
		}
	}
	m.sessions = rest
	m.recompose()
	if m.selKey == "p:mock-stripe" || m.selKey == "" {
		t.Fatalf("selection after removal: %q", m.selKey)
	}
	if r := m.selected(); r == nil || !r.selectable() {
		t.Fatalf("selected row %+v", r)
	}
}

// An action in flight shows a spinner and "starting…" on its row until it
// is done and a poll confirms it.
func TestPendingSpinner(t *testing.T) {
	f := newFake(t)
	f.gate = make(chan struct{})
	h := start(t, f, 140, 35, true)
	for range 7 { // infra, clickhouse, redis, gateway, tracking, worker-rust, api, panel
		h.keys("j")
	}
	h.waitFor("─ panel ")
	h.keys("space")
	v := h.waitFor("starting…")
	if !strings.ContainsAny(ansi.Strip(v), strings.Join(spinner, "")) {
		t.Errorf("no spinner glyph:\n%s", ansi.Strip(v))
	}
	close(f.gate)
	v = h.waitFor("panel: started")
	if strings.Contains(ansi.Strip(v), "starting…") {
		t.Errorf("still starting after the poll:\n%s", ansi.Strip(v))
	}
	if a := f.actionLog(); len(a) != 1 || a[0] != "up panel" {
		t.Errorf("actions = %q", a)
	}
}

// space on a container sub-row runs docker stop/start; an unowned one is
// left alone.
func TestContainerActions(t *testing.T) {
	f := newFake(t)
	h := start(t, f, 140, 35, true)
	h.keys("j", "j") // infra/redis
	h.waitFor("─ infra/redis ")
	h.keys("space")
	h.waitFor("infra/redis: stopped")
	h.keys("G") // scratch/db, unowned
	h.waitFor("─ scratch/db ")
	h.keys("space")
	h.waitFor("belongs to no process")
	if a := f.actionLog(); len(a) != 1 || a[0] != "docker stop infra/redis" {
		t.Errorf("actions = %q", a)
	}
}

// A click selects the row under the cursor; the wheel scrolls the pane
// under it.
func TestMouse(t *testing.T) {
	f := newFake(t)
	f.writeLog("gateway", gatewayLog(120))
	h := start(t, f, 140, 35, true)
	// y: top bar (0), border (1), infra (2,3), clickhouse (4), redis (5), gateway (6)
	h.tm.Send(tea.MouseClickMsg{X: 5, Y: 6, Button: tea.MouseLeft})
	h.waitFor("─ gateway ")
	h.waitFor(" 120  ")
	h.tm.Send(tea.MouseWheelMsg{X: 60, Y: 10, Button: tea.MouseWheelUp})
	h.wait(func(v string) bool { return !strings.Contains(v, " 120  ") && strings.Contains(v, "LIST") })
	h.tm.Send(tea.MouseWheelMsg{X: 5, Y: 10, Button: tea.MouseWheelDown})
	h.waitFor("─ tracking ")
}

// An edit of rov.yaml on disk (an agent's rov config add) shows up through
// the file watch.
func TestConfigWatch(t *testing.T) {
	f := newFake(t)
	h := start(t, f, 140, 35, true)
	f.writeConfig(sampleYAML + "  mock-s3: { cmd: go run ./tools/mock-s3 }\n")
	h.tm.Send(watchMsg{})
	v := h.waitFor("mock-s3")
	if !strings.Contains(ansi.Strip(v), "go run ./tools/mock-s3") {
		t.Errorf("new process without its command:\n%s", ansi.Strip(v))
	}
}

// A help overlay lists the keys and closes again.
func TestHelpOverlay(t *testing.T) {
	f := newFake(t)
	h := start(t, f, 140, 35, true)
	h.keys("?")
	h.waitFor("previous run")
	h.keys("esc")
	v := h.waitFor("LIST")
	if strings.Contains(ansi.Strip(v), "─ keys ") {
		t.Error("help still open")
	}
}

// When the entry being edited changed on disk, the form stays open with the
// user's input and says so; saving again applies it to the fresh file.
func TestFormConflict(t *testing.T) {
	f := newFake(t)
	h := start(t, f, 140, 35, true)
	h.keys("j", "j", "j", "e") // gateway
	h.waitFor("edit gateway")
	h.keys("tab", "ctrl+u")
	h.typeText("just gateway-dev")
	// meanwhile an agent edits the same entry
	edited := strings.Replace(sampleYAML, "description: search gateway", "description: the gateway", 1)
	if err := os.WriteFile(f.ConfigPath(), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	h.keys("ctrl+s")
	v := h.waitFor("rov.yaml changed on disk — reloaded")
	if !strings.Contains(ansi.Strip(v), "just gateway-dev") {
		t.Errorf("the input was lost:\n%s", ansi.Strip(v))
	}
	if !strings.Contains(ansi.Strip(v), "the gateway") {
		t.Errorf("the untouched description wasn't reloaded:\n%s", ansi.Strip(v))
	}
	h.keys("ctrl+s")
	got := waitFile(t, f.ConfigPath(), func(s string) bool { return strings.Contains(s, "gateway-dev") })
	if want := strings.Replace(edited, "cmd: just gateway\n", "cmd: just gateway-dev\n", 1); got != want {
		t.Errorf("rov.yaml =\n%s\nwant\n%s", got, want)
	}
	h.waitFor("rov.yaml: updated gateway")
}

// p shows the previous run of a process and back.
func TestPrevRun(t *testing.T) {
	f := newFake(t)
	f.writeLog("gateway", gatewayLog(10))
	if err := os.WriteFile(filepath.Join(f.LogDir(), "gateway.prev.ansi"), []byte("\x1b[31mpanic: old run\x1b[0m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := start(t, f, 140, 35, true)
	h.keys("j", "j", "j", "l", "p")
	v := h.waitFor("panic: old run")
	if !strings.Contains(ansi.Strip(v), "─ gateway · previous run ") {
		t.Errorf("no previous-run title:\n%s", ansi.Strip(v))
	}
	h.keys("p")
	h.wait(func(v string) bool { return strings.Contains(v, "results=") && !strings.Contains(v, "panic: old run") })
}
