package tui

import (
	"bufio"
	"context"
	"errors"
	"io"
	"io/fs"
	"sort"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/labsbit/rov/internal/logs"
)

// logLine is one output line: as stored (.ansi, SGR kept) and as plain text
// for search. The plain form is computed off the UI goroutine. A marker is
// a line of rov's own (a container's stream ended), without a line number.
type logLine struct {
	ansi, plain string
	marker      bool
}

func newLogLine(s string) logLine { return logLine{ansi: s, plain: stripSGR(s)} }

func markerLine(text string) logLine {
	return logLine{ansi: "\x1b[2m" + text + sgrReset, plain: text, marker: true}
}

// batch is what a loader delivers at once.
type batch struct {
	lines []logLine
	err   error
	// reset says the lines replace everything (a one-shot load).
	reset bool
	// start, when hasStart, is the number of the run's lines before the
	// first line of this batch: line numbers are those of the log files.
	start    int
	hasStart bool
}

// match is one search hit: an absolute line number and a span of its plain
// text.
type match struct {
	line int
	sp   span
}

// outKind is where an output comes from.
type outKind int

const (
	outProcess   outKind = iota // <log_dir>/<name>.ansi, followed
	outContainer                // docker logs --follow
)

// outState is the output pane of one row: its lines while it is selected,
// and — kept while other rows are selected — its scroll position, follow
// state and search.
type outState struct {
	key  string
	kind outKind
	name string // process name, or the container name for docker logs
	prev bool   // showing the previous run

	lines []logLine
	base  int // absolute number of lines[0] (lines dropped from the front)
	err   string

	gen    int // loader generation; batches of other generations are dropped
	cancel context.CancelFunc
	ch     <-chan batch
	active bool
	// run is the RunID of the process run shown (runKnown once seen); a
	// different one in a later Status means the process started again.
	run      string
	runKnown bool

	// A container's stream ends when the container stops: ended is set, a
	// marker line shows it, and once a docker poll issued after the end
	// (id > endDock) sees the container running, the stream picks up
	// again from endedAt (since) under the lines already shown.
	ended   bool
	endedAt time.Time
	endDock int
	since   time.Time

	// View: following the end, or paused with the top at (top, topRow).
	follow    bool
	top       int // absolute line
	topRow    int // row within that line when wrapped
	pauseMark int // total lines when follow paused: what came after is new
	// trimmed: the line the paused view was anchored on fell out of the
	// scrollback; the view is pinned to the oldest line kept.
	trimmed bool

	// Search.
	query   string
	fold    bool
	matches []match
	cur     int // index into matches, -1 none

	// Rendered rows per absolute line, for one layout (cacheKey).
	cache    map[int][]string
	cacheKey cacheKey
}

// isMarker reports whether line abs is one of rov's marker lines.
func (o *outState) isMarker(abs int) bool {
	l, ok := o.line(abs)
	return ok && l.marker
}

type cacheKey struct {
	width   int
	wrap    bool
	cw      cells
	search  string
	fold    bool
	palette bool
}

func newOutState(key string, kind outKind, name string) *outState {
	return &outState{key: key, kind: kind, name: name, follow: true, cur: -1}
}

// total is the absolute number of lines seen (dropped ones included).
func (o *outState) total() int { return o.base + len(o.lines) }

// newLines is the count of lines that arrived since follow paused.
func (o *outState) newLines() int {
	if o.follow {
		return 0
	}
	return max(0, o.total()-o.pauseMark)
}

func (o *outState) line(abs int) (logLine, bool) {
	i := abs - o.base
	if i < 0 || i >= len(o.lines) {
		return logLine{}, false
	}
	return o.lines[i], true
}

// clear drops the lines (a new run, or the other run selected).
func (o *outState) clear() {
	o.lines, o.base, o.err = nil, 0, ""
	o.matches, o.cur = nil, -1
	o.top, o.topRow = 0, 0
	o.trimmed = false
	o.ended, o.since = false, time.Time{}
	o.cache = nil
}

// append adds a batch of lines, extends the search and drops the oldest
// lines beyond the scrollback.
func (o *outState) append(ls []logLine) {
	for _, l := range ls {
		abs := o.total()
		o.lines = append(o.lines, l)
		if o.query != "" {
			for _, sp := range findAll(l.plain, o.query, o.fold) {
				o.matches = append(o.matches, match{abs, sp})
			}
		}
	}
	if drop := len(o.lines) - logs.ScrollbackLines; drop > 0 {
		kept := make([]logLine, logs.ScrollbackLines)
		copy(kept, o.lines[drop:])
		o.lines = kept
		o.base += drop
		i := sort.Search(len(o.matches), func(i int) bool { return o.matches[i].line >= o.base })
		o.matches = o.matches[i:]
		if o.cur >= 0 {
			o.cur = max(-1, o.cur-i)
		}
		if o.top < o.base {
			// The paused view's own line is gone: show the oldest kept.
			o.top, o.topRow = o.base, 0
			o.trimmed = !o.follow
		}
		for k := range o.cache {
			if k < o.base {
				delete(o.cache, k)
			}
		}
	}
}

// setSearch runs query over every loaded line.
func (o *outState) setSearch(query string) {
	o.query, o.fold = query, smartCase(query)
	o.matches, o.cur = nil, -1
	if query == "" {
		return
	}
	for i := range o.lines {
		for _, sp := range findAll(o.lines[i].plain, query, o.fold) {
			o.matches = append(o.matches, match{o.base + i, sp})
		}
	}
}

// spansOf returns the matches on line abs and the index of the current one
// among them (-1 if it is elsewhere).
func (o *outState) spansOf(abs int) (spans []span, cur int) {
	cur = -1
	i := sort.Search(len(o.matches), func(i int) bool { return o.matches[i].line >= abs })
	for j := i; j < len(o.matches) && o.matches[j].line == abs; j++ {
		if j == o.cur {
			cur = len(spans)
		}
		spans = append(spans, o.matches[j].sp)
	}
	return spans, cur
}

// view is the layout of the output area for one frame.
type view struct {
	width  int // content cells per row (after the gutter)
	height int // rows
	wrap   bool
	sty    *styles
	cw     cells
}

// rows returns the rendered rows of line abs: highlighted, then laid out.
// Rows without the current match are cached per layout.
func (o *outState) rows(abs int, v view) []string {
	l, ok := o.line(abs)
	if !ok {
		return nil
	}
	spans, cur := o.spansOf(abs)
	key := cacheKey{v.width, v.wrap, v.cw, o.query, o.fold, v.sty.p.dark}
	if cur < 0 {
		if o.cacheKey != key || o.cache == nil {
			o.cache, o.cacheKey = map[int][]string{}, key
		}
		if r, ok := o.cache[abs]; ok {
			return r
		}
	}
	s := l.ansi
	if len(spans) > 0 {
		s = highlight(s, spans, cur, v.sty.matchSGR(), v.sty.curMatchSGR())
	}
	r := layoutLine(s, v.width, v.wrap, v.cw)
	if cur < 0 {
		if len(o.cache) > 4096 {
			clear(o.cache)
		}
		o.cache[abs] = r
	}
	return r
}

func (o *outState) rowCount(abs int, v view) int {
	if !v.wrap {
		return 1
	}
	return max(1, len(o.rows(abs, v)))
}

// pos is a position in the output: a line and a row within it.
type pos struct{ line, row int }

func (a pos) less(b pos) bool { return a.line < b.line || a.line == b.line && a.row < b.row }

// bottom is the top position that shows the last line on the last row.
func (o *outState) bottom(v view) pos {
	if len(o.lines) == 0 {
		return pos{o.base, 0}
	}
	need := v.height
	for abs := o.total() - 1; abs >= o.base; abs-- {
		n := o.rowCount(abs, v)
		if n >= need {
			return pos{abs, n - need}
		}
		need -= n
	}
	return pos{o.base, 0}
}

// topPos is where the visible window starts.
func (o *outState) topPos(v view) pos {
	b := o.bottom(v)
	if o.follow {
		return b
	}
	p := pos{o.top, o.topRow}
	if p.line < o.base {
		p = pos{o.base, 0}
	}
	if b.less(p) {
		return b
	}
	return p
}

// pause stops following, keeping what is on screen.
func (o *outState) pause(v view) {
	if !o.follow {
		return
	}
	p := o.bottom(v)
	o.follow = false
	o.top, o.topRow = p.line, p.row
	o.pauseMark = o.total()
}

// resume follows the end again.
func (o *outState) resume() {
	o.follow = true
	o.pauseMark = 0
	o.trimmed = false
}

// scroll moves the window by n rows (negative: up). Scrolling up pauses
// follow; reaching the bottom resumes it.
func (o *outState) scroll(n int, v view) {
	if n == 0 || len(o.lines) == 0 {
		return
	}
	if n < 0 {
		o.pause(v)
	} else if o.follow {
		return
	}
	o.trimmed = false
	p := o.topPos(v)
	for ; n < 0; n++ {
		if p.row > 0 {
			p.row--
		} else if p.line > o.base {
			p.line--
			p.row = o.rowCount(p.line, v) - 1
		} else {
			break
		}
	}
	b := o.bottom(v)
	for ; n > 0 && p.less(b); n-- {
		if p.row+1 < o.rowCount(p.line, v) {
			p.row++
		} else {
			p.line++
			p.row = 0
		}
	}
	o.top, o.topRow = p.line, p.row
	if !p.less(b) {
		o.resume()
	}
}

// home shows the first line.
func (o *outState) home(v view) {
	o.pause(v)
	o.top, o.topRow = o.base, 0
}

// reveal scrolls so line abs is about a third down the window.
func (o *outState) reveal(abs int, v view) {
	o.pause(v)
	p := pos{abs, 0}
	for need := v.height / 3; need > 0 && p.line > o.base; {
		p.line--
		n := o.rowCount(p.line, v)
		if n > need {
			p.row = n - need
			break
		}
		need -= n
	}
	o.top, o.topRow = p.line, p.row
	if b := o.bottom(v); b.less(p) {
		o.top, o.topRow = b.line, b.row
	}
}

// jump moves the current match by dir (+1 next, -1 previous), wrapping, and
// reveals it.
func (o *outState) jump(dir int, v view) bool {
	if len(o.matches) == 0 {
		return false
	}
	switch {
	case o.cur < 0 && dir > 0:
		// first match at or after the top of the window
		top := o.topPos(v).line
		o.cur = sort.Search(len(o.matches), func(i int) bool { return o.matches[i].line >= top })
		if o.cur >= len(o.matches) {
			o.cur = 0
		}
	case o.cur < 0:
		o.cur = len(o.matches) - 1
	default:
		o.cur = (o.cur + dir + len(o.matches)) % len(o.matches)
	}
	o.reveal(o.matches[o.cur].line, v)
	return true
}

// visible returns the rows of the window and the absolute line of each
// (-1 for a continuation row).
func (o *outState) visible(v view) (rows []string, lineOf []int) {
	p := o.topPos(v)
	for abs := p.line; abs < o.total() && len(rows) < v.height; abs++ {
		rs := o.rows(abs, v)
		if !v.wrap && len(rs) > 1 {
			rs = rs[:1]
		}
		start := 0
		if abs == p.line {
			start = min(p.row, len(rs)-1)
		}
		for i := start; i < len(rs) && len(rows) < v.height; i++ {
			rows = append(rows, rs[i])
			if i == 0 {
				lineOf = append(lineOf, abs)
			} else {
				lineOf = append(lineOf, -1)
			}
		}
	}
	return rows, lineOf
}

// Loaders deliver batches over a channel; waitBatch turns the next one into
// a message. Cancelling the context ends the loader and closes the channel.

type linesMsg struct {
	key string
	gen int
	b   batch
}

type loaderDoneMsg struct {
	key string
	gen int
}

func waitBatch(key string, gen int, ch <-chan batch) tea.Cmd {
	return func() tea.Msg {
		b, ok := <-ch
		if !ok {
			return loaderDoneMsg{key, gen}
		}
		return linesMsg{key, gen, b}
	}
}

// maxBatch bounds the lines of one message.
const maxBatch = 8192

// pump collects lines from in into batches on out until in closes or ctx
// is done.
func pump(ctx context.Context, in <-chan string, out chan<- batch, start *atomic.Int64) {
	for {
		var l string
		var ok bool
		select {
		case l, ok = <-in:
		case <-ctx.Done():
			return
		}
		if !ok {
			return
		}
		// the start was set before the first line was sent
		bt := batch{}
		if v := start.Swap(-1); v >= 0 {
			bt.start, bt.hasStart = int(v), true
		}
		b := []logLine{newLogLine(l)}
	drain:
		for len(b) < maxBatch {
			select {
			case l, ok := <-in:
				if !ok {
					break drain
				}
				b = append(b, newLogLine(l))
			default:
				break drain
			}
		}
		bt.lines = b
		select {
		case out <- bt:
		case <-ctx.Done():
			return
		}
	}
}

// runLoader runs produce, which sends raw lines until it returns, and
// delivers them as batches; an error comes last, then the channel closes.
// produce may call setStart (before sending the lines it numbers) with the
// number of the run's lines before them.
func runLoader(ctx context.Context, produce func(raw chan<- string, setStart func(int)) error) <-chan batch {
	raw := make(chan string, 4096)
	out := make(chan batch, 2)
	var start atomic.Int64
	start.Store(-1)
	setStart := func(n int) { start.Store(int64(n)) }
	go func() {
		done := make(chan struct{})
		go func() { pump(ctx, raw, out, &start); close(done) }()
		err := produce(raw, setStart)
		close(raw)
		<-done
		if err != nil && ctx.Err() == nil {
			select {
			case out <- batch{err: err}:
			case <-ctx.Done():
			}
		}
		close(out)
	}()
	return out
}

// followFile follows the current run of a process: the last
// ScrollbackLines lines, then new ones as they are written (across
// rotation and restarts).
func followFile(ctx context.Context, dir, name string) <-chan batch {
	return runLoader(ctx, func(raw chan<- string, setStart func(int)) error {
		err := logs.FollowNumbered(ctx, logs.Query{Dir: dir, Name: name, ANSI: true, N: logs.ScrollbackLines}, raw, setStart)
		if ctx.Err() != nil {
			return nil
		}
		return err
	})
}

// loadPrev reads the previous run once.
func loadPrev(ctx context.Context, dir, name string) <-chan batch {
	out := make(chan batch, 1)
	go func() {
		defer close(out)
		ls, total, err := logs.Tail(logs.Query{Dir: dir, Name: name, Run: logs.Prev, ANSI: true, N: logs.ScrollbackLines})
		b := batch{reset: true, err: err, start: max(0, total-len(ls)), hasStart: true}
		for _, l := range ls {
			b.lines = append(b.lines, newLogLine(l))
		}
		if errors.Is(err, fs.ErrNotExist) {
			b.err = nil
		}
		select {
		case out <- b:
		case <-ctx.Done():
		}
	}()
	return out
}

// followDocker streams `docker logs --follow` of a container: the last
// dockerTail lines, or with since everything from then on.
func followDocker(ctx context.Context, b Backend, container string, since time.Time) <-chan batch {
	return runLoader(ctx, func(raw chan<- string, _ func(int)) error {
		tail := dockerTail
		if !since.IsZero() {
			tail = -1
		}
		rc, err := b.ContainerLogs(ctx, container, tail, since, true)
		if err != nil {
			return err
		}
		stop := context.AfterFunc(ctx, func() { rc.Close() })
		defer stop()
		rerr := readLines(rc, maxDockerLine, func(l string) bool {
			select {
			case raw <- sanitize(l):
				return true
			case <-ctx.Done():
				return false
			}
		})
		cerr := rc.Close()
		if ctx.Err() != nil {
			return nil
		}
		if rerr != nil {
			return rerr
		}
		return cerr
	})
}

// maxDockerLine caps a line of docker logs; the rest of a longer line is
// dropped (the stream goes on).
const maxDockerLine = 1 << 20

// readLines calls emit with each line of r (without its newline), keeping
// at most max bytes of a line, until r ends or emit returns false. A last
// line without a newline is emitted too. It returns r's error, if any.
func readLines(r io.Reader, max int, emit func(string) bool) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var long []byte // a line longer than the buffer, so far
	for {
		chunk, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			if room := max - len(long); room > 0 {
				long = append(long, chunk[:min(len(chunk), room)]...)
			}
			continue
		}
		line := chunk
		if len(long) > 0 {
			if room := max - len(long); room > 0 {
				long = append(long, chunk[:min(len(chunk), room)]...)
			}
			line, long = long, long[:0]
		}
		if n := len(line); n > 0 && line[n-1] == '\n' {
			line = line[:n-1]
		}
		if len(line) > 0 || err == nil {
			if !emit(string(line)) {
				return nil
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// dockerTail is how many lines of a container's log to start with.
const dockerTail = 5000
