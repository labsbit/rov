package tui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// The two width methods of the renderer.
const (
	wc = cells(ansi.WcWidth)
	gr = cells(ansi.GraphemeWidth)
)

const (
	red   = "\x1b[31m"
	bold  = "\x1b[1m"
	reset = "\x1b[0m"
)

func TestLayoutLineWrapKeepsColour(t *testing.T) {
	line := red + "abcdef" + reset + "gh"
	got := layoutLine(line, 4, true, wc)
	want := []string{red + "abcd" + reset, red + "ef" + reset + "gh"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %q\nwant %q", got, want)
	}
	for _, r := range got {
		if w := ansi.StringWidth(r); w > 4 {
			t.Errorf("row %q is %d wide", r, w)
		}
	}
}

func TestLayoutLineTruncate(t *testing.T) {
	got := layoutLine(bold+"hello world"+reset, 6, false, wc)
	want := []string{bold + "hello…" + reset}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows = %q, want %q", got, want)
	}
	if got := layoutLine("short", 6, false, wc); !reflect.DeepEqual(got, []string{"short"}) {
		t.Errorf("short line = %q", got)
	}
	if got := layoutLine("exactly", 7, false, wc); !reflect.DeepEqual(got, []string{"exactly"}) {
		t.Errorf("exact fit = %q", got)
	}
}

func TestLayoutLineTabsWideAndControls(t *testing.T) {
	got := layoutLine("a\tb", 20, true, wc)
	if want := []string{"a       b"}; !reflect.DeepEqual(got, want) {
		t.Errorf("tab: %q", got)
	}
	// a wide character never straddles two rows
	got = layoutLine("abc日本", 4, true, wc)
	if want := []string{"abc", "日本"}; !reflect.DeepEqual(got, want) {
		t.Errorf("wide: %q", got)
	}
	got = layoutLine("a\x07b\x1b[2Kc", 10, true, wc)
	if want := []string{"abc"}; !reflect.DeepEqual(got, want) {
		t.Errorf("controls: %q", got)
	}
	if got := layoutLine("", 10, true, wc); !reflect.DeepEqual(got, []string{""}) {
		t.Errorf("empty: %q", got)
	}
}

func TestSGRStateResets(t *testing.T) {
	var st sgrState
	st.apply("\x1b[1;31m")
	st.apply("\x1b[0;32m")
	if got := st.prefix(); got != "\x1b[32m" {
		t.Errorf("after 0;32: %q", got)
	}
	st.apply("\x1b[38;5;0m") // 0 is a colour index here, not a reset
	if got := st.prefix(); got != "\x1b[32m\x1b[38;5;0m" {
		t.Errorf("extended colour: %q", got)
	}
	st.apply("\x1b[m")
	if st.active() {
		t.Errorf("bare reset kept %q", st.prefix())
	}
}

func TestHighlightOverColour(t *testing.T) {
	line := red + "error: " + bold + "boom" + reset + " done"
	plain := stripSGR(line)
	if plain != "error: boom done" {
		t.Fatalf("plain = %q", plain)
	}
	spans := findAll(plain, "or: bo", true)
	if !reflect.DeepEqual(spans, []span{{3, 9}}) {
		t.Fatalf("spans = %v", spans)
	}
	const on, cur = "\x1b[7m", "\x1b[7;1m"
	got := highlight(line, spans, -1, on, cur)
	// match style on at "or", re-applied after the bold, line SGR replayed
	// at the end of the match (red+bold), then the line goes on.
	want := red + "err" + on + "or: " + bold + on + "bo" + reset + red + bold + "om" + reset + " done"
	if got != want {
		t.Fatalf("highlight =\n%q\nwant\n%q", got, want)
	}
	if ansi.Strip(got) != plain {
		t.Errorf("highlight changed the text: %q", ansi.Strip(got))
	}
	// the current match gets its own style; a match at the very end closes
	got = highlight("abab", findAll("abab", "ab", false), 1, on, cur)
	if want := on + "ab" + reset + cur + "ab" + reset; got != want {
		t.Errorf("cur = %q, want %q", got, want)
	}
}

func TestHighlightThenWrap(t *testing.T) {
	line := highlight("abcdefgh", []span{{2, 6}}, -1, "\x1b[7m", "")
	rows := layoutLine(line, 4, true, wc)
	// the second row starts with reverse video still on
	if len(rows) != 2 || !strings.HasPrefix(rows[1], "\x1b[7m") {
		t.Fatalf("rows = %q", rows)
	}
	if ansi.Strip(strings.Join(rows, "")) != "abcdefgh" {
		t.Errorf("text lost: %q", rows)
	}
}

func TestFindAllCase(t *testing.T) {
	if got := findAll("Error ERROR error", "error", true); len(got) != 3 {
		t.Errorf("folded: %v", got)
	}
	if got := findAll("Error ERROR error", "Error", false); len(got) != 1 {
		t.Errorf("exact: %v", got)
	}
	if got := findAll("aaaa", "aa", true); !reflect.DeepEqual(got, []span{{0, 2}, {2, 4}}) {
		t.Errorf("non-overlapping: %v", got)
	}
	if !smartCase("abc") || smartCase("aBc") {
		t.Error("smartCase")
	}
}

func TestSanitize(t *testing.T) {
	got := sanitize("10%\r100% " + red + "ok" + reset + "\x1b[2K\x1b]0;t\x07!")
	if want := "100% " + red + "ok" + reset + "!"; got != want {
		t.Errorf("sanitize = %q, want %q", got, want)
	}
}

// emojiLine has clusters whose width differs between the two methods
// (👨‍👩‍👧 6 or 2 cells, 👍🏽 4 or 2, ❤️ 1 or 2), a flag, a combining
// accent and a zero-width space.
const emojiLine = "EMOJI 😀 👍🏽 👨‍👩‍👧 ❤️ 🇩🇪 ✅ é (e+combining: é) zero\u200bwidth "

// Under either width method every row fits the width as that method
// measures it, and the rows put together are the whole line.
func TestLayoutLineEmojiWidths(t *testing.T) {
	line := strings.Repeat(emojiLine, 3)
	for _, c := range []cells{wc, gr} {
		for _, w := range []int{1, 3, 7, 10, 13, 40} {
			rows := layoutLine(red+line+reset, w, true, c)
			var all strings.Builder
			for i, r := range rows {
				if n := c.width(r); n > w || (i < len(rows)-1 && n < w-6) {
					t.Errorf("method %d width %d: row %d is %d cells: %q", c, w, i, n, r)
				}
				all.WriteString(ansi.Strip(r))
			}
			if w >= 6 && all.String() != strings.TrimRight(line, " ") {
				t.Errorf("method %d width %d: text lost:\n got %q\nwant %q", c, w, all.String(), strings.TrimRight(line, " "))
			}
		}
	}
	if wc.width("👨‍👩‍👧") == gr.width("👨‍👩‍👧") {
		t.Fatal("the test needs a cluster the methods measure differently")
	}
}

// Trailing blanks don't make rows of their own; colours after them stay.
func TestLayoutLineTrimsTrailingBlanks(t *testing.T) {
	line := "Container x  Started" + strings.Repeat(" ", 200)
	if got := layoutLine(line, 30, true, wc); !reflect.DeepEqual(got, []string{"Container x  Started"}) {
		t.Errorf("padded line: %q", got)
	}
	got := layoutLine(red+"ok   "+reset+"   \t ", 10, true, wc)
	if want := []string{red + "ok" + reset}; !reflect.DeepEqual(got, want) {
		t.Errorf("coloured padded line: %q, want %q", got, want)
	}
	if got := layoutLine("   ", 10, true, wc); !reflect.DeepEqual(got, []string{""}) {
		t.Errorf("blank line: %q", got)
	}
}
