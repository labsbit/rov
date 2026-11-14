package tui

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/labsbit/rov/internal/core"
	"github.com/labsbit/rov/internal/logs"
	"github.com/labsbit/rov/internal/testenv"
	"github.com/labsbit/rov/internal/tmux"
)

// Regression tests for the review of the live behaviour: widths, runs,
// container streams, failed polls, scrollback, the top and bottom bars and
// the forms.

// editSession returns the fake's sessions with name changed by fn (nil
// removes it); options are copied, so earlier snapshots stay as they were.
func editSession(f *fakeBackend, name string, fn func(s *tmux.Session)) {
	f.mu.Lock()
	var out []tmux.Session
	for _, s := range f.sessions {
		s.Options = maps.Clone(s.Options)
		if s.Name == name {
			if fn == nil {
				continue
			}
			fn(&s)
		}
		out = append(out, s)
	}
	f.mu.Unlock()
	f.setSessions(out)
}

// restartSession makes name a new run started in the same second: a new
// pane pid (an older rov's session has no run id) or a new @rov_run_id.
func restartSession(f *fakeBackend, name string, byRunID bool) {
	editSession(f, name, func(s *tmux.Session) {
		if byRunID {
			s.Options[core.OptRunID] = s.Options[core.OptRunID] + "1"
		} else {
			s.PanePID += 1000
		}
	})
}

func (h *harness) refresh() { h.tm.Send(refreshMsg{}) }

// checkRows checks that every row of a view is exactly w cells under c and
// that each pane row ends in a border.
func checkRows(t *testing.T, v string, w int, c cells) {
	t.Helper()
	rows := strings.Split(v, "\n")
	for i, r := range rows {
		if n := c.width(r); n != w {
			t.Errorf("row %d is %d cells, want %d: %q", i, n, w, ansi.Strip(r))
		}
		if i > 0 && i < len(rows)-1 {
			if p := ansi.Strip(r); !strings.HasSuffix(p, "│") && !strings.HasSuffix(p, "╮") && !strings.HasSuffix(p, "╯") {
				t.Errorf("row %d lost its right border: %q", i, p)
			}
		}
	}
}

// Rows with emoji keep the pane width under both of the renderer's width
// methods, in every layout and with an overlay open.
func TestEmojiRowsKeepWidth(t *testing.T) {
	for _, sz := range []struct{ w, h int }{{140, 35}, {99, 30}, {79, 24}, {49, 20}} {
		t.Run(fmt.Sprintf("%dx%d", sz.w, sz.h), func(t *testing.T) {
			f := newFake(t)
			f.writeLog("gateway", []string{"before", emojiLine + emojiLine, "\x1b[31m" + emojiLine + "\x1b[0m", "after"})
			h := start(t, f, sz.w, sz.h, true)
			h.keys("j", "j", "j", "l") // gateway, output focused (the only pane below 50)
			v := h.waitFor("after")
			checkRows(t, v, sz.w, wc)
			if !strings.Contains(ansi.Strip(v), "EMOJI") {
				t.Fatalf("no emoji line:\n%s", ansi.Strip(v))
			}
			// the terminal reports Unicode core mode: grapheme widths
			h.tm.Send(tea.ModeReportMsg{Mode: ansi.ModeUnicodeCore, Value: ansi.ModeReset})
			h.wait(func(string) bool { return true })
			v = h.view()
			checkRows(t, v, sz.w, gr)
			h.keys("?")
			checkRows(t, h.waitFor("keys"), sz.w, gr)
		})
	}
}

// A restart within the same second is a new run: the pane clears and
// follows it — by pane pid for an older rov's session, by run id otherwise.
func TestRestartSameSecondClears(t *testing.T) {
	for _, byRunID := range []bool{false, true} {
		t.Run(fmt.Sprintf("runid=%v", byRunID), func(t *testing.T) {
			f := newFake(t)
			f.writeLog("gateway", gatewayLog(120))
			if byRunID {
				editSession(f, "gateway", func(s *tmux.Session) { s.Options[core.OptRunID] = "1700000000000000001" })
			}
			h := start(t, f, 140, 35, true)
			h.keys("j", "j", "j", "l")
			h.waitFor(" 120  ")
			h.keys("k", "k", "k") // paused
			h.waitFor(" 117  ")
			f.writeLog("gateway", []string{"new run line"})
			restartSession(f, "gateway", byRunID)
			h.refresh()
			v := h.waitFor("new run line")
			if p := ansi.Strip(v); strings.Contains(p, "results=") || !strings.Contains(p, "   1  new run line") {
				t.Errorf("old run still shown, or not from line 1:\n%s", p)
			}
			f.appendLog("gateway", []string{"second line"})
			h.waitFor("second line") // following again
		})
	}
}

// down, then up: absent → running is a new run too.
func TestStartAfterDownClears(t *testing.T) {
	f := newFake(t)
	f.writeLog("gateway", gatewayLog(20))
	var gw tmux.Session
	for _, s := range f.sessions {
		if s.Name == "gateway" {
			gw = s
		}
	}
	h := start(t, f, 140, 35, true)
	h.keys("j", "j", "j")
	h.waitFor("results=")
	editSession(f, "gateway", nil) // rov down
	h.refresh()
	v := h.waitFor("absent")
	if !strings.Contains(ansi.Strip(v), "absent · last run output") {
		t.Errorf("header of an absent process with old output:\n%s", ansi.Strip(v))
	}
	f.writeLog("gateway", []string{"fresh start"})
	gw.Options = maps.Clone(gw.Options)
	gw.Options[core.OptStarted] = fmt.Sprint(testenv.SampleTime.Unix())
	f.mu.Lock()
	ss := append(slices.Clone(f.sessions), gw)
	f.mu.Unlock()
	f.setSessions(ss)
	h.refresh()
	v = h.waitFor("fresh start")
	if strings.Contains(ansi.Strip(v), "results=") || strings.Contains(ansi.Strip(v), "last run output") {
		t.Errorf("old run kept after the start:\n%s", ansi.Strip(v))
	}
}

// A restart while another row is selected shows the new run, following,
// when the row is selected again; without a restart the position is kept.
func TestRestartWhileUnselected(t *testing.T) {
	f := newFake(t)
	f.writeLog("gateway", gatewayLog(120))
	h := start(t, f, 140, 35, true)
	h.keys("j", "j", "j", "l")
	h.waitFor(" 120  ")
	for range 10 {
		h.keys("k")
	}
	h.waitFor(" 110  ")
	h.keys("h", "j", "k") // away and back: same run, same place
	v := h.waitFor("─ gateway ")
	if p := ansi.Strip(v); strings.Contains(p, " 120  ") || !strings.Contains(p, " 110  ") {
		t.Fatalf("position not kept across reselection:\n%s", p)
	}
	h.keys("j") // tracking
	h.waitFor("─ tracking ")
	f.writeLog("gateway", []string{"after restart"})
	restartSession(f, "gateway", false)
	h.refresh()
	h.wait(func(string) bool { return true })
	h.keys("k")
	v = h.waitFor("after restart")
	if strings.Contains(ansi.Strip(v), "new line") {
		t.Errorf("still paused after the restart:\n%s", ansi.Strip(v))
	}
	f.appendLog("gateway", []string{"more"})
	h.waitFor("   2  more")
}

// A container's stream ends when it stops: a marker says so, and the
// stream picks up again (from the end on) once a later poll sees the
// container running.
func TestContainerStreamResumes(t *testing.T) {
	f := newFake(t)
	f.dockerLogs["sag-redis"] = "first run line\n"
	h := start(t, f, 140, 35, true)
	h.keys("j", "j") // infra/redis
	h.waitFor("stream ended (container stopped)")
	f.dockerMore["sag-redis"] = "second run line\n"
	h.refresh()
	v := h.waitFor("second run line")
	p := ansi.Strip(v)
	if strings.Index(p, "first run line") > strings.Index(p, "stream ended") || strings.Index(p, "stream ended") > strings.Index(p, "second run line") {
		t.Errorf("order of the lines:\n%s", p)
	}
	var calls []logCall
	for _, c := range f.containerLogCalls() {
		if c.container == "sag-redis" {
			calls = append(calls, c)
		}
	}
	if len(calls) < 2 || !calls[0].since.IsZero() || calls[1].since.IsZero() {
		t.Errorf("docker logs calls = %+v, want a tail, then one --since", calls)
	}
}

// A failed docker poll keeps the containers and the selection; the top bar
// says what failed.
func TestDockerPollErrorKeepsRows(t *testing.T) {
	f := newFake(t)
	h := start(t, f, 140, 35, true)
	h.keys("j", "j") // infra/redis
	h.waitFor("─ infra/redis ")
	f.setContainers(nil, errors.New("docker ps: context deadline exceeded"))
	h.refresh()
	v := h.waitFor("docker: docker ps: context deadline exceeded")
	if p := ansi.Strip(v); !strings.Contains(p, "─ infra/redis ") || !strings.Contains(p, "└ redis") {
		t.Errorf("rows or selection lost after a failed poll:\n%s", p)
	}
	f.setContainers(testenv.SampleInputs(t).Containers, nil)
	h.refresh()
	h.wait(func(v string) bool { return !strings.Contains(v, "deadline exceeded") })
}

// Line numbers are those of the log files (the .1 file, then the current
// one), also when only the last 50 000 lines are loaded.
func TestLineNumbersAbsolute(t *testing.T) {
	f := newFake(t)
	var a, b []string
	for i := 1; i <= 30000; i++ {
		a = append(a, fmt.Sprintf("old %d", i))
		b = append(b, fmt.Sprintf("cur %d", 30000+i))
	}
	if err := os.WriteFile(filepath.Join(f.LogDir(), "gateway.ansi.1"), []byte(strings.Join(a, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.writeLog("gateway", b)
	h := start(t, f, 140, 35, true)
	h.keys("j", "j", "j", "l")
	h.waitFor("60000  cur 60000")
	h.keys("g")
	h.waitFor("10001  old 10001")
}

// A paused view stays on its line while older lines leave the scrollback;
// once its own line has left, it shows the oldest kept and says so.
func TestPausedViewAnchored(t *testing.T) {
	o := newOutState("k", outProcess, "x")
	v := testView(40, 10, true)
	o.append(linesOf(30000, func(i int) string { return fmt.Sprintf("line %d", i) }))
	for range 5 {
		o.scroll(-1, v)
	}
	top := o.topPos(v).line
	want, _ := o.line(top)
	o.append(linesOf(30000, func(i int) string { return fmt.Sprintf("more %d", i) }))
	if got, _ := o.line(o.topPos(v).line); got.plain != want.plain || o.trimmed {
		t.Fatalf("paused view moved: top %q, want %q (trimmed %v)", got.plain, want.plain, o.trimmed)
	}
	o.append(linesOf(logs.ScrollbackLines, func(i int) string { return fmt.Sprintf("flood %d", i) }))
	if p := o.topPos(v); p.line != o.base || !o.trimmed {
		t.Fatalf("anchor gone: top %d base %d trimmed %v", p.line, o.base, o.trimmed)
	}
	o.resume()
	if o.trimmed {
		t.Error("trimmed after resume")
	}
}

// The renamed process started meanwhile: the rename is refused at save
// time.
func TestRenameRecheckedOnSave(t *testing.T) {
	f := newFake(t)
	editSession(f, "tracking", nil) // not running when the form opens
	var tracking tmux.Session
	for _, s := range testenv.SampleInputs(t).Sessions {
		if s.Name == "tracking" {
			tracking = s
		}
	}
	h := start(t, f, 140, 35, true)
	for range 4 { // infra, clickhouse, redis, gateway, tracking
		h.keys("j")
	}
	h.waitFor("─ tracking ")
	h.keys("e")
	h.waitFor("edit tracking")
	h.keys("ctrl+u")
	h.typeText("tracker")
	f.mu.Lock()
	ss := append(slices.Clone(f.sessions), tracking)
	f.mu.Unlock()
	f.setSessions(ss) // an agent ran rov up tracking
	h.refresh()
	h.wait(func(string) bool { return true })
	h.keys("ctrl+s")
	h.waitFor("stop it first")
	if b, _ := os.ReadFile(f.ConfigPath()); string(b) != sampleYAML {
		t.Errorf("rov.yaml changed:\n%s", b)
	}
}

// The path is shortened in the middle to keep the socket; below 100
// columns neither shows.
func TestTopBarPathAndSocket(t *testing.T) {
	f := newFake(t)
	m := New(f, Options{Now: func() time.Time { return testenv.SampleTime }, Manual: true, Home: "/nowhere"})
	m.width, m.height = 140, 35
	root := f.Root()
	m.opts.Home = ""
	bar := ansi.Strip(m.topBar())
	if !strings.Contains(bar, root) || !strings.Contains(bar, "tmux "+f.Socket()) {
		t.Errorf("140 columns: %q", bar)
	}
	// a root too long for the bar: middle-ellipsis, socket kept
	long := "/very/long/path/to/some/deeply/nested/checkout/of/the/project/that/goes/on" + root
	f.root = long
	bar = ansi.Strip(m.topBar())
	if !strings.Contains(bar, "…") || !strings.Contains(bar, "myapp") || !strings.Contains(bar, "tmux "+f.Socket()) {
		t.Errorf("long root at 140: %q", bar)
	}
	if n := wc.width(bar); n != 140 {
		t.Errorf("bar is %d cells", n)
	}
	m.width = 99
	if bar = ansi.Strip(m.topBar()); strings.Contains(bar, "tmux") || strings.Contains(bar, "/very") {
		t.Errorf("99 columns: %q", bar)
	}
}

// The Docker field offers every compose project seen (stopped containers
// included), those of rov.yaml, the current value, and none.
func TestDockerOptions(t *testing.T) {
	f := newFake(t)
	m := New(f, Options{Now: func() time.Time { return testenv.SampleTime }, Manual: true})
	m.containers = f.containers
	var got []string
	for _, o := range m.dockerOptions("gone-project") {
		got = append(got, o.Value)
	}
	want := []string{"", "gone-project", "scratch", "search-api-gateway-local"}
	if !slices.Equal(got, want) {
		t.Errorf("options = %q, want %q", got, want)
	}
	m.containers = nil
	got = got[:0]
	for _, o := range m.dockerOptions("") {
		got = append(got, o.Value)
	}
	if !slices.Equal(got, []string{"", "search-api-gateway-local"}) {
		t.Errorf("without containers: %q", got)
	}
}

// Below 50 columns the bottom bar says how to switch panes.
func TestNarrowPaneHint(t *testing.T) {
	f := newFake(t)
	h := start(t, f, 49, 20, true)
	v := ansi.Strip(h.view())
	if last := v[strings.LastIndex(v, "\n")+1:]; !strings.Contains(last, "h/l pane") {
		t.Errorf("bottom bar: %q", last)
	}
	h.keys("l")
	v = ansi.Strip(h.waitFor("OUTPUT"))
	if last := v[strings.LastIndex(v, "\n")+1:]; !strings.Contains(last, "h/l pane") {
		t.Errorf("bottom bar, output: %q", last)
	}
}

// Working dir starts empty (placeholder "."), so typing replaces it; its
// error is one short line.
func TestFormCwd(t *testing.T) {
	f := newFake(t)
	h := start(t, f, 100, 35, true)
	h.keys("n")
	v := ansi.Strip(h.waitFor("new process"))
	if !strings.Contains(v, "Working dir  .") {
		t.Errorf("no placeholder:\n%s", v)
	}
	h.keys("tab", "tab")
	h.typeText("nope/" + strings.Repeat("x", 80))
	h.keys("tab")
	v = ansi.Strip(h.waitFor("no such directory: nope/"))
	if strings.Contains(v, ".nope") || strings.Contains(v, f.Root()) {
		t.Errorf("typed text appended to a prefill, or the root shown:\n%s", v)
	}
	for _, l := range strings.Split(v, "\n") {
		if strings.Contains(l, "no such directory") && !strings.Contains(l, "…") {
			t.Errorf("long cwd error not cut: %q", l)
		}
	}
	h.keys("esc")
}

// An ad-hoc process whose stored definition was changed while it runs
// shows ◆ changed, like an edited rov.yaml entry.
func TestAdhocChanged(t *testing.T) {
	f := newFake(t)
	editSession(f, "mock-stripe", func(s *tmux.Session) {
		s.Options[core.OptRunDef] = `{"cmd":"node scripts/mock-stripe-old.js","cwd":".","env":["STRIPE_KEY=sk_test"]}`
	})
	h := start(t, f, 140, 35, true)
	h.keys("G", "k")
	v := ansi.Strip(h.waitFor("─ mock-stripe "))
	if !strings.Contains(v, "◆ changed") {
		t.Errorf("no ◆ changed:\n%s", v)
	}
}

// docker logs lines longer than the cap are cut, not the end of the
// stream; the reader's error comes back.
func TestReadLinesLong(t *testing.T) {
	long := strings.Repeat("x", 3*maxDockerLine)
	var got []string
	err := readLines(strings.NewReader("a\n"+long+"\nb\n\nlast"), maxDockerLine, func(l string) bool {
		got = append(got, l)
		return true
	})
	if err != nil || len(got) != 5 || got[0] != "a" || len(got[1]) != maxDockerLine || got[2] != "b" || got[3] != "" || got[4] != "last" {
		t.Errorf("err %v, %d lines", err, len(got))
	}
	boom := errors.New("boom")
	err = readLines(io.MultiReader(strings.NewReader("x\n"), errReader{boom}), maxDockerLine, func(string) bool { return true })
	if !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
}

type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// The help overlay lists /, #, w and p for both panes, like the spec.
func TestHelpKeyScope(t *testing.T) {
	for _, k := range []string{"/", "#", "w", "p"} {
		found := false
		for _, h := range helpKeys {
			if h.key == k {
				found = true
				if h.section != "both" {
					t.Errorf("%s is in %q", k, h.section)
				}
			}
		}
		if !found {
			t.Errorf("%s missing from the help", k)
		}
	}
}

// tmux 3.4 and later count graphemes without saying so.
func TestGraphemeTmux(t *testing.T) {
	for env, want := range map[[2]string]bool{
		{"tmux", "3.4"}: true, {"tmux", "3.5a"}: true, {"tmux", "next-3.6"}: true, {"tmux", "4.0"}: true,
		{"tmux", "3.3a"}: false, {"tmux", ""}: false, {"iTerm.app", "3.5"}: false, {"", ""}: false,
	} {
		getenv := func(k string) string {
			switch k {
			case "TERM_PROGRAM":
				return env[0]
			case "TERM_PROGRAM_VERSION":
				return env[1]
			}
			return ""
		}
		if got := graphemeTmux(getenv); got != want {
			t.Errorf("%q: %v, want %v", env, got, want)
		}
	}
}
