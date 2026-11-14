// Package pipe implements `rov __pipe`, the log writer tmux's pipe-pane feeds
// with the raw output of one pane.
//
// It turns a terminal byte stream into two line-aligned files:
//
//	<name>.log   plain text, every escape sequence removed
//	<name>.ansi  the same lines with SGR (colour/attribute) sequences kept
//
// Line N of one file is always line N of the other, including across
// rotation, so search and scroll positions translate directly.
//
// # Line discipline
//
// The same rules produce both files:
//
//   - "\n" and "\r\n" end a line. A bare "\r" rewinds: only the text after
//     the last bare "\r" survives. A "\r" directly followed by "\n" is a line
//     end, also when the two arrive in different reads.
//   - Clear screen (ESC[2J, ESC[3J, ESC c, and cursor-home followed by
//     ESC[J / ESC[0J) writes the marker line [ClearMarker], dim in .ansi.
//     Text pending on the current line is ended first. Consecutive clears
//     with no line in between collapse into one marker, and no marker is
//     written before the first line of a run: an empty file has no boundary
//     worth marking (Vite clears the screen on startup).
//   - .ansi keeps SGR only, .log keeps no escape sequences. Cursor movement,
//     erase-line, modes (alternate screen included), OSC (OSC 8 hyperlinks
//     keep their text, which is ordinary output between two OSCs), DCS, SOS,
//     PM, APC and charset selection are dropped, as are C0 controls other
//     than tab (backspace and BEL included). CAN and SUB abort a sequence.
//     Sequences may be split across reads arbitrarily.
//   - Every .ansi line is self-contained: SGR state is tracked, re-emitted
//     (normalised) before the first character of each line and reset at the
//     end of a line that changed it, so the TUI can render any line alone.
//     SGR changes not followed by text on the same line emit nothing.
//   - Invalid UTF-8 becomes U+FFFD.
//   - A line longer than [MaxLineBytes] of plain text (or whose .ansi form
//     grows past four times that) is split: the first part becomes a line
//     and the text continues on the next one. Memory stays bounded whatever
//     the input; nothing is lost.
//   - A partial line is flushed as a line after [Options.FlushAfter] of
//     silence, so prompts such as "Continue? [y/N]" are visible. If output
//     then continues, the continuation starts a new line (both files stay
//     aligned); a line terminator arriving straight after the flush only
//     ends the flushed line and does not add an empty one.
//
// # Files and rotation
//
// When writing a line would push either file past [Options.Max], both files
// rotate at that line (<name>.log → <name>.log.1, <name>.ansi →
// <name>.ansi.1, replacing older .1 files), so each file stays within the cap.
// [PrepareRun] moves a finished run to <name>.prev.{log,ansi} before a start.
//
// A writer keeps its file descriptors for its whole life. If [PrepareRun]
// moves the files away while an old writer still runs (a restart, before
// tmux re-establishes pipe-pane and the old pipe sees EOF), the old writer's
// last lines land in the moved files, which is the previous run where they
// belong. It never reopens by path: before rotating it checks that the paths
// still name its own files and, if not, stops rotating and drops output once
// the cap is reached, so it cannot touch the new run. The check, the renames
// and the reopen of a rotation hold <dir>/.lock/<name>.rotate, as PrepareRun
// does while it moves the files, so the two never interleave.
package pipe
