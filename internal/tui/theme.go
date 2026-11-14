package tui

import (
	"image/color"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
)

// palette is the spec's colour table, resolved for one background. Every
// role has a dark and a light value; the program downsamples truecolor to
// 256 or 16 colours when the terminal has fewer, and meaning never depends
// on colour alone (glyphs and words carry it).
type palette struct {
	dark bool
	mono bool // no colours at all: reverse video and bold carry the marks

	accent    color.Color // focused border, selection bar, ports, mode pill
	running   color.Color // ●, up
	warn      color.Color // ▲ degraded, starting…, ◆ changed, conflicts
	failed    color.Color // ✗, crashed N, validation errors
	done      color.Color // ✓ exited
	idle      color.Color // ○, secondary text, line numbers, unfocused borders
	selection color.Color // background of the selected row
	pillFg    color.Color // text on accent (mode pill, current match)
	text      color.Color // primary text; nil = the terminal's own
}

// tone is one role's colour in each profile: 16 colours, 256 colours and
// truecolor, for a dark and a light background.
type tone struct {
	dark16, dark256   int
	darkHex           string
	light16, light256 int
	lightHex          string
}

var tones = struct {
	accent, running, warn, failed, done, idle, selection, pillFg tone
}{
	accent:    tone{14, 80, "#5fd7d7", 6, 30, "#00788a"},
	running:   tone{10, 114, "#87d787", 2, 28, "#2e7d32"},
	warn:      tone{11, 221, "#ffd75f", 3, 136, "#a66a00"},
	failed:    tone{9, 210, "#ff8787", 1, 160, "#c62828"},
	done:      tone{12, 110, "#8fa6c8", 4, 60, "#4a6285"},
	idle:      tone{8, 242, "#6c7086", 8, 245, "#8a8f98"},
	selection: tone{0, 236, "#262a33", 7, 255, "#e8ecf2"},
	pillFg:    tone{0, 234, "#1a1d23", 15, 231, "#ffffff"},
}

func (t tone) color(dark bool, prof colorprofile.Profile) color.Color {
	c := lipgloss.Complete(prof)
	if dark {
		return c(lipgloss.ANSIColor(t.dark16), lipgloss.ANSIColor(t.dark256), lipgloss.Color(t.darkHex))
	}
	return c(lipgloss.ANSIColor(t.light16), lipgloss.ANSIColor(t.light256), lipgloss.Color(t.lightHex))
}

func newPalette(dark bool, prof colorprofile.Profile) palette {
	col := func(t tone) color.Color { return t.color(dark, prof) }
	return palette{
		dark:      dark,
		mono:      prof < colorprofile.ANSI,
		accent:    col(tones.accent),
		running:   col(tones.running),
		warn:      col(tones.warn),
		failed:    col(tones.failed),
		done:      col(tones.done),
		idle:      col(tones.idle),
		selection: col(tones.selection),
		pillFg:    col(tones.pillFg),
	}
}

// styles are the palette as Lip Gloss styles.
type styles struct {
	p palette

	accent, running, warn, failed, done, idle lipgloss.Style
	bold                                      lipgloss.Style
	pill                                      lipgloss.Style
	key                                       lipgloss.Style // key in a hint
	label                                     lipgloss.Style // label in a hint
}

func newStyles(dark bool, prof colorprofile.Profile) *styles {
	p := newPalette(dark, prof)
	fg := func(c color.Color) lipgloss.Style { return lipgloss.NewStyle().Foreground(c) }
	return &styles{
		p:       p,
		accent:  fg(p.accent),
		running: fg(p.running),
		warn:    fg(p.warn),
		failed:  fg(p.failed),
		done:    fg(p.done),
		idle:    fg(p.idle),
		bold:    lipgloss.NewStyle().Bold(true),
		pill:    pill(p),
		key:     lipgloss.NewStyle(),
		label:   fg(p.idle),
	}
}

func pill(p palette) lipgloss.Style {
	st := lipgloss.NewStyle().Bold(true).Padding(0, 1)
	if p.mono {
		return st.Reverse(true)
	}
	return st.Foreground(p.pillFg).Background(p.accent)
}

// matchStyle and curMatchStyle are raw SGR for search matches: reverse
// video over the line's own colours, the current match in the accent.
func (s *styles) matchSGR() string { return "\x1b[7m" }

func (s *styles) curMatchSGR() string {
	if s.p.mono || s.p.accent == nil || s.p.pillFg == nil {
		return "\x1b[1;4;7m"
	}
	r, g, b := rgb(s.p.accent)
	fr, fg, fb := rgb(s.p.pillFg)
	return "\x1b[27;1;38;2;" + itoa(fr) + ";" + itoa(fg) + ";" + itoa(fb) + ";48;2;" + itoa(r) + ";" + itoa(g) + ";" + itoa(b) + "m"
}

func rgb(c color.Color) (int, int, int) {
	r, g, b, _ := c.RGBA()
	return int(r >> 8), int(g >> 8), int(b >> 8)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// color returns the foreground style of a role.
func (s *styles) color(r role) lipgloss.Style {
	switch r {
	case roleAccent:
		return s.accent
	case roleRunning:
		return s.running
	case roleWarn:
		return s.warn
	case roleFailed:
		return s.failed
	case roleDone:
		return s.done
	case roleIdle:
		return s.idle
	}
	return lipgloss.NewStyle()
}
