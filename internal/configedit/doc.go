package configedit

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// doc is rov.yaml as lines, with the yaml.v3 nodes used to find things in
// them. Line numbers in this file are 0-based indexes into lines.
type doc struct {
	lines      []string // each with its line ending
	eol        string   // "\n" or "\r\n", used for new lines
	noFinalEOL bool     // the input didn't end with a line ending

	hasProcs  bool // the root mapping has a processes key
	procLine  int  // line of the processes key
	procCol   int  // its indentation
	inlineVal bool // "processes: ~" or "processes: {}": value on the key line

	entries []*entry
	indent  int // indentation of entry keys
	step    int // indentation step inside an entry
	braces  int // cached braceSpace: 1 "{ x }", -1 "{x}", 0 not computed yet

	// The processes block: lines[procLine+1 : regionEnd] when hasProcs.
	regionEnd int
}

// entry is one process in the file.
type entry struct {
	name     string
	key, val *yaml.Node
	start    int // first line, including the head comment
	keyLine  int
	end      int // last line (inclusive)
	// oneLine: every node of the value starts on the key line and the lines
	// after it, up to end, are only comments and blank lines.
	oneLine bool
}

// singleLineFlow reports whether the entry is "name: { … }" on one line. The
// entry may still own lines below it — comments indented deeper than the key,
// and blank lines — which an edit of the value leaves alone.
func (e *entry) singleLineFlow() bool {
	return e.val.Kind == yaml.MappingNode && e.val.Style&yaml.FlowStyle != 0 &&
		e.val.Line-1 == e.keyLine && e.oneLine
}

// block reports whether the entry's value is a block mapping, or empty, so
// that fields live on lines of their own below the key.
func (e *entry) block(d *doc) bool {
	if e.val.Kind == yaml.MappingNode {
		return e.val.Style&yaml.FlowStyle == 0
	}
	return isNull(e.val) && !hasInlineValue(d.lines[e.keyLine], e.key)
}

var errUnsupported = errors.New("rov can't edit this layout")

func parseDoc(src []byte) (*doc, error) {
	s := string(src)
	d := &doc{eol: "\n", step: 2, indent: 2}
	if strings.Contains(s, "\r\n") {
		d.eol = "\r\n"
	}
	// yaml.v3 also breaks lines at a lone CR, NEL, LS and PS; its line
	// numbers would not match the lines here.
	if strings.IndexFunc(strings.ReplaceAll(s, "\r\n", "\n"), func(r rune) bool {
		return r == '\r' || r == '\u0085' || r == '\u2028' || r == '\u2029'
	}) >= 0 {
		return nil, fmt.Errorf("%w: rov.yaml has a line break other than LF or CRLF (a lone CR, NEL, LS or PS)", errUnsupported)
	}
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += d.eol
		d.noFinalEOL = true
	}
	d.lines = strings.SplitAfter(s, "\n")
	d.lines = d.lines[:len(d.lines)-1] // SplitAfter leaves "" after the last \n

	var top yaml.Node
	if err := yaml.Unmarshal(src, &top); err != nil {
		return nil, fmt.Errorf("rov.yaml is not valid YAML: %s", strings.TrimPrefix(err.Error(), "yaml: "))
	}
	if len(top.Content) == 0 || isNull(top.Content[0]) {
		return d, nil // empty or comments only
	}
	root := top.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, errors.New("the top level of rov.yaml must be a mapping")
	}
	if root.Style&yaml.FlowStyle != 0 {
		return nil, fmt.Errorf("%w: the top level of rov.yaml is a flow mapping ({…}); write it in block style", errUnsupported)
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if k.Value != "processes" {
			continue
		}
		d.hasProcs = true
		d.procLine = k.Line - 1
		d.procCol = k.Column - 1
		return d, d.processes(k, v)
	}
	return d, nil
}

func (d *doc) processes(k, v *yaml.Node) error {
	switch {
	case isNull(v) || v.Kind == yaml.MappingNode && v.Style&yaml.FlowStyle != 0 && len(v.Content) == 0:
		d.inlineVal = hasInlineValue(d.lines[d.procLine], k)
		d.regionEnd = d.indentedEnd(d.procLine, d.procCol, len(d.lines)) + 1
		// Starter files document the format in comments under processes:;
		// new entries take their indentation.
		for _, l := range d.lines[d.procLine+1 : d.regionEnd] {
			if !isBlank(l) && indentOf(l) > d.procCol {
				d.indent = indentOf(l)
				break
			}
		}
		if d.indent <= d.procCol {
			d.indent = d.procCol + 2
		}
		return nil
	case v.Kind != yaml.MappingNode:
		return errors.New("processes in rov.yaml must be a mapping")
	case v.Style&yaml.FlowStyle != 0:
		return fmt.Errorf("%w: processes is a flow mapping ({…}); write it in block style", errUnsupported)
	}

	for i := 0; i+1 < len(v.Content); i += 2 {
		ek, ev := v.Content[i], v.Content[i+1]
		if ek.Kind != yaml.ScalarNode {
			return fmt.Errorf("%w: line %d: a process name must be a plain string", errUnsupported, ek.Line)
		}
		d.entries = append(d.entries, &entry{name: ek.Value, key: ek, val: ev, keyLine: ek.Line - 1})
	}
	if len(d.entries) == 0 {
		return nil
	}
	d.indent = d.entries[0].key.Column - 1

	// Head comments: comment lines directly above the key, not indented
	// deeper than it (deeper ones trail the entry before).
	floor := d.procLine
	for _, e := range d.entries {
		e.start = e.keyLine
		for e.start-1 > floor && isComment(d.lines[e.start-1]) && indentOf(d.lines[e.start-1]) <= d.indent {
			e.start--
		}
		floor = e.keyLine
	}
	for i, e := range d.entries {
		limit := len(d.lines)
		if i+1 < len(d.entries) {
			limit = d.entries[i+1].start
		}
		e.end = d.nodeEnd(e.keyLine, d.indent, limit, e.val)
		e.oneLine = lastLine(e.val) == e.keyLine && d.onlyComments(e.keyLine+1, e.end+1)
	}
	d.regionEnd = d.entries[len(d.entries)-1].end + 1

	for _, e := range d.entries {
		if e.val.Kind == yaml.MappingNode && e.block(d) && len(e.val.Content) > 0 {
			if s := e.val.Content[0].Column - 1 - d.indent; s >= 2 && s <= 9 {
				d.step = s
			}
			break
		}
	}
	return nil
}

// indentedEnd returns the last line after from, before limit, that belongs to
// a node starting on line from at indentation col: lines indented deeper,
// with blank lines in between. It returns from when there are none.
//
// A comment line is not indented meaningfully: one at or left of col (or any
// comment after it) is inside the node when a deeper line of YAML follows,
// and left out when it trails the node, so that it stays with what comes
// next (the next entry's head comment, a section comment).
func (d *doc) indentedEnd(from, col, limit int) int {
	end := from
	low := false // a comment at or left of col since end
	for l := from + 1; l < limit; l++ {
		switch line := d.lines[l]; {
		case isBlank(line):
		case isComment(line) && (low || indentOf(line) <= col):
			low = true
		case indentOf(line) > col:
			end, low = l, false
		default:
			return end
		}
	}
	return end
}

// nodeEnd is indentedEnd for the node n whose key is on line from, extended
// to the last line any node of n starts on (a flow mapping continued at a
// shallow indentation), but not to limit.
func (d *doc) nodeEnd(from, col, limit int, n *yaml.Node) int {
	return min(max(d.indentedEnd(from, col, limit), lastLine(n)), limit-1)
}

// lastLine returns the last line (0-based) on which n or a node inside it
// starts. Aliases are not followed.
func lastLine(n *yaml.Node) int {
	last := n.Line - 1
	for _, c := range n.Content {
		last = max(last, lastLine(c))
	}
	return last
}

// onlyComments reports whether lines[from:to] are all comments or blank.
func (d *doc) onlyComments(from, to int) bool {
	for _, l := range d.lines[from:to] {
		if !isBlank(l) && !isComment(l) {
			return false
		}
	}
	return true
}

// field is one key/value pair inside an entry: a field, or an env variable.
type field struct {
	name     string
	key, val *yaml.Node
	start    int // first line, including the head comment
	keyLine  int
	end      int
}

// pairs returns the line spans of the pairs of the block mapping m, whose
// key is on line parent and which ends at line end.
func (d *doc) pairs(m *yaml.Node, parent, end int) []*field {
	if m.Kind != yaml.MappingNode {
		return nil
	}
	var fs []*field
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		fs = append(fs, &field{name: k.Value, key: k, val: v, keyLine: k.Line - 1})
	}
	if len(fs) == 0 {
		return nil
	}
	col := fs[0].key.Column - 1
	floor := parent
	for _, f := range fs {
		f.start = f.keyLine
		for f.start-1 > floor && isComment(d.lines[f.start-1]) && indentOf(d.lines[f.start-1]) <= col {
			f.start--
		}
		floor = f.keyLine
	}
	for i, f := range fs {
		limit := end + 1
		if i+1 < len(fs) {
			limit = fs[i+1].start
		}
		f.end = d.nodeEnd(f.keyLine, col, limit, f.val)
	}
	return fs
}

// hasInlineValue reports whether the key's line holds a value after the colon
// ("name: ~", "name: {}"), as opposed to nothing or only a comment.
func hasInlineValue(line string, key *yaml.Node) bool {
	code, _ := splitComment(line)
	i := colonAfter(code, byteCol(code, key.Column-1))
	return i >= 0 && strings.TrimSpace(code[i+1:]) != ""
}

// splitComment splits a line into code and trailing comment (with its
// leading "#", without the line ending). A '#' starts a comment at the start
// of the line or after whitespace, outside quoted scalars.
func splitComment(line string) (code, comment string) {
	line = strings.TrimRight(line, "\r\n")
	if i := commentStart(line); i >= 0 {
		return line[:i], line[i:]
	}
	return line, ""
}

func commentStart(line string) int {
	var quote byte
	prev := byte(0) // previous non-space byte outside quotes
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			switch {
			case quote == '"' && c == '\\':
				i++
			case c == quote && quote == '\'' && i+1 < len(line) && line[i+1] == '\'':
				i++
			case c == quote:
				quote, prev = 0, c
			}
			continue
		}
		switch {
		case c == '#' && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t'):
			return i
		case (c == '"' || c == '\'') && strings.IndexByte("\x00:,{[-?", prev) >= 0 && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t' || strings.IndexByte(",{[", line[i-1]) >= 0):
			// A quote only opens a quoted scalar where a scalar can start.
			quote = c
		case c != ' ' && c != '\t':
			prev = c
		}
	}
	return -1
}

// colonAfter returns the index of the ':' ending the mapping key that starts
// at byte from, or -1.
func colonAfter(code string, from int) int {
	if from >= len(code) {
		return -1
	}
	if q := code[from]; q == '"' || q == '\'' {
		for i := from + 1; i < len(code); i++ {
			switch {
			case q == '"' && code[i] == '\\':
				i++
			case code[i] == q && q == '\'' && i+1 < len(code) && code[i+1] == '\'':
				i++
			case code[i] == q:
				from = i + 1
				i = len(code)
			}
		}
	}
	if i := strings.IndexByte(code[from:], ':'); i >= 0 {
		return from + i
	}
	return -1
}

// byteCol converts a 0-based column in characters (as yaml.v3 counts) to a
// byte offset in line.
func byteCol(line string, col int) int {
	for i := range line {
		if col == 0 {
			return i
		}
		col--
	}
	return len(line)
}

func isBlank(line string) bool { return strings.TrimSpace(line) == "" }

func isComment(line string) bool { return strings.HasPrefix(strings.TrimLeft(line, " \t"), "#") }

func indentOf(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

func isNull(n *yaml.Node) bool { return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null" }
