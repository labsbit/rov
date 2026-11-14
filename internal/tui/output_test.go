package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"github.com/labsbit/rov/internal/logs"
)

func testView(w, h int, wrap bool) view {
	return view{width: w, height: h, wrap: wrap, sty: newStyles(true, colorprofile.TrueColor)}
}

func linesOf(n int, f func(i int) string) []logLine {
	out := make([]logLine, n)
	for i := range out {
		out[i] = newLogLine(f(i))
	}
	return out
}

func plainRows(rows []string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = ansi.Strip(r)
	}
	return out
}

func TestOutputFollowPauseResume(t *testing.T) {
	o := newOutState("p:x", outProcess, "x")
	v := testView(20, 3, true)
	o.append(linesOf(10, func(i int) string { return fmt.Sprintf("line %d", i+1) }))
	rows, _ := o.visible(v)
	if got := plainRows(rows); strings.Join(got, "|") != "line 8|line 9|line 10" {
		t.Fatalf("following shows %q", got)
	}
	o.scroll(-2, v)
	if o.follow {
		t.Fatal("scrolling up kept following")
	}
	o.append(linesOf(3, func(i int) string { return "new" }))
	if n := o.newLines(); n != 3 {
		t.Errorf("new lines = %d, want 3", n)
	}
	rows, _ = o.visible(v)
	if got := plainRows(rows); got[0] != "line 6" {
		t.Errorf("paused window moved: %q", got)
	}
	// scrolling back down to the end resumes
	o.scroll(100, v)
	if !o.follow || o.newLines() != 0 {
		t.Errorf("follow %v new %d after scrolling to the end", o.follow, o.newLines())
	}
	o.home(v)
	if rows, _ = o.visible(v); ansi.Strip(rows[0]) != "line 1" || o.follow {
		t.Errorf("home: %q follow %v", plainRows(rows), o.follow)
	}
	o.resume()
	if rows, _ = o.visible(v); ansi.Strip(rows[2]) != "new" {
		t.Errorf("resume: %q", plainRows(rows))
	}
}

func TestOutputWrapRowsAndNumbers(t *testing.T) {
	o := newOutState("p:x", outProcess, "x")
	v := testView(5, 4, true)
	o.append([]logLine{newLogLine("abcdefghij"), newLogLine("xy")})
	rows, lineOf := o.visible(v)
	if got := strings.Join(plainRows(rows), "|"); got != "abcde|fghij|xy" {
		t.Fatalf("rows = %q", got)
	}
	if fmt.Sprint(lineOf) != "[0 -1 1]" {
		t.Errorf("lineOf = %v (a continuation row has no number)", lineOf)
	}
	// bottom of a window smaller than one wrapped line starts mid-line
	v.height = 1
	if p := o.bottom(v); p != (pos{1, 0}) {
		t.Errorf("bottom = %+v", p)
	}
	v.height = 2
	if p := o.bottom(v); p != (pos{0, 1}) {
		t.Errorf("bottom = %+v", p)
	}
	// without wrap, one row per line
	nv := testView(5, 4, false)
	rows, _ = o.visible(nv)
	if got := strings.Join(plainRows(rows), "|"); got != "abcd…|xy" {
		t.Errorf("no wrap: %q", got)
	}
}

func TestOutputSearch(t *testing.T) {
	o := newOutState("p:x", outProcess, "x")
	v := testView(40, 3, true)
	o.append(linesOf(30, func(i int) string {
		if i%10 == 3 {
			return fmt.Sprintf("\x1b[31mERROR\x1b[0m at %d", i)
		}
		return fmt.Sprintf("ok %d", i)
	}))
	o.setSearch("error") // smart case: matches ERROR
	if len(o.matches) != 3 {
		t.Fatalf("matches = %v", o.matches)
	}
	o.home(v)
	if !o.jump(1, v) || o.cur != 0 {
		t.Fatalf("jump: cur %d", o.cur)
	}
	rows, _ := o.visible(v)
	found := false
	for _, r := range rows {
		if strings.Contains(r, "ERROR") && strings.Contains(r, "\x1b[1;4;7m") || strings.Contains(r, "48;2;") {
			found = true
		}
	}
	if !found {
		t.Errorf("current match not highlighted in %q", rows)
	}
	o.jump(-1, v)
	if o.cur != 2 {
		t.Errorf("previous from the first wraps to the last: %d", o.cur)
	}
	// new lines extend the search
	o.append([]logLine{newLogLine("another error")})
	if len(o.matches) != 4 {
		t.Errorf("appended line not searched: %d", len(o.matches))
	}
	o.setSearch("ERROR") // upper case: exact
	if len(o.matches) != 3 {
		t.Errorf("exact search: %d", len(o.matches))
	}
}

// The scrollback keeps the last 50 000 lines; numbers stay absolute and
// rendering a frame touches only the visible lines.
func TestOutputScrollbackAndCost(t *testing.T) {
	o := newOutState("p:x", outProcess, "x")
	v := testView(60, 30, true)
	for b := 0; b < 6; b++ {
		o.append(linesOf(10_000, func(i int) string {
			return fmt.Sprintf("\x1b[32mINF\x1b[0m batch %d line %d with some text to wrap around the pane width", b, i)
		}))
	}
	if len(o.lines) != logs.ScrollbackLines || o.base != 60_000-logs.ScrollbackLines {
		t.Fatalf("kept %d lines, base %d", len(o.lines), o.base)
	}
	start := time.Now()
	for range 100 {
		o.visible(v)
	}
	if el := time.Since(start); el > 500*time.Millisecond {
		t.Errorf("100 frames took %v", el)
	}
	if n := len(o.cache); n > 3*v.height {
		t.Errorf("cache holds %d lines for a %d-row window", n, v.height)
	}
	_, lineOf := o.visible(v)
	if last := lineOf[len(lineOf)-2]; last != 59_999 && lineOf[len(lineOf)-1] != 59_999 {
		t.Errorf("last line number %v", lineOf[len(lineOf)-2:])
	}
}
