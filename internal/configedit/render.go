package configedit

import (
	"bytes"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/labsbit/rov/internal/config"
)

// fieldOrder is the order fields are written in when rov writes them.
var fieldOrder = []string{"cmd", "cwd", "env", "description", "docker"}

func rank(name string) int {
	for i, f := range fieldOrder {
		if f == name {
			return i
		}
	}
	return -1
}

// envValue is an env value node; unlike strNode it keeps "" as "".
func envValue(v string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v}
}

// fieldValue returns field name of p as a node, or nil when it is empty.
func fieldValue(p config.Process, name string, flowEnv bool) *yaml.Node {
	switch name {
	case "cmd":
		return strNode(p.Cmd)
	case "cwd":
		return strNode(p.Cwd)
	case "description":
		return strNode(p.Description)
	case "docker":
		return strNode(p.Docker)
	case "env":
		if len(p.Env) == 0 {
			return nil
		}
		m := &yaml.Node{Kind: yaml.MappingNode}
		if flowEnv {
			m.Style = yaml.FlowStyle
		}
		for _, e := range p.Env {
			m.Content = append(m.Content, strNode(e.Key), envValue(e.Value))
		}
		return m
	}
	return nil
}

// strNode is a string scalar, or nil for "". The !!str tag makes yaml.v3
// quote values that would otherwise read back as numbers, booleans or null.
func strNode(s string) *yaml.Node {
	if s == "" {
		return nil
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s}
}

// present lists the non-empty fields of p.
func present(p config.Process) map[string]bool {
	out := map[string]bool{}
	for _, f := range fieldOrder {
		if fieldValue(p, f, false) != nil {
			out[f] = true
		}
	}
	return out
}

func onlyCmd(p config.Process) bool {
	f := present(p)
	return len(f) == 1 && f["cmd"]
}

// encode renders n as a YAML document with the file's indentation step.
func (d *doc) encode(n *yaml.Node) string {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(d.step)
	// Encoding a node tree rov built from strings can't fail.
	_ = enc.Encode(n)
	_ = enc.Close()
	return strings.TrimSuffix(b.String(), "\n")
}

// keyText renders a mapping key (quoted only if it must be).
func (d *doc) keyText(name string) string { return d.encode(strNode(name)) }

// flowText renders a flow mapping on one line, with spaces inside the braces
// when spaced: "{ cmd: x }" rather than "{cmd: x}".
func (d *doc) flowText(m *yaml.Node, spaced bool) string {
	m.Style = yaml.FlowStyle
	s := d.encode(m)
	if spaced && len(s) > 2 && s[0] == '{' && s[len(s)-1] == '}' && !strings.Contains(s, "\n") {
		s = "{ " + s[1:len(s)-1] + " }"
	}
	return s
}

// flowMapping builds the flow value of an entry from p, with keys in order.
func flowMapping(p config.Process, order []string) *yaml.Node {
	m := &yaml.Node{Kind: yaml.MappingNode, Style: yaml.FlowStyle}
	for _, f := range order {
		if v := fieldValue(p, f, true); v != nil {
			m.Content = append(m.Content, strNode(f), v)
		}
	}
	return m
}

// renderField renders "name: value" at indent, as lines. A flow mapping
// value is written with spaces inside its braces when spaced.
func (d *doc) renderField(name string, val *yaml.Node, indent int, spaced bool) []string {
	var text string
	if val.Kind == yaml.MappingNode && val.Style&yaml.FlowStyle != 0 {
		text = d.keyText(name) + ": " + d.flowText(val, spaced)
	} else {
		text = d.encode(&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{strNode(name), val}})
	}
	return d.indentLines(text, indent)
}

func (d *doc) indentLines(text string, indent int) []string {
	pad := strings.Repeat(" ", indent)
	var out []string
	for l := range strings.SplitSeq(text, "\n") {
		if l != "" {
			l = pad + l
		}
		out = append(out, l+d.eol)
	}
	return out
}

// renderEntry renders a whole entry: one flow line when p has only cmd,
// block style otherwise. comment, if any, goes at the end of the key line.
func (d *doc) renderEntry(p config.Process, comment string) []string {
	prefix := strings.Repeat(" ", d.indent) + d.keyText(p.Name) + ":"
	if onlyCmd(p) {
		line := prefix + d.alignPad(len(prefix)) + d.flowText(flowMapping(p, fieldOrder), d.braceSpace())
		return []string{appendComment(line, comment) + d.eol}
	}
	out := []string{appendComment(prefix, comment) + d.eol}
	for _, f := range fieldOrder {
		if v := fieldValue(p, f, false); v != nil {
			out = append(out, d.renderField(f, v, d.indent+d.step, d.braceSpace())...)
		}
	}
	return out
}

func appendComment(code, comment string) string {
	if comment == "" {
		return code
	}
	return strings.TrimRight(code, " \t") + " " + comment
}

// withComment puts comment after newCode where it sat after oldCode: at the
// same column if the comment was aligned with extra spaces and still fits,
// else one space after the code.
func withComment(newCode, oldCode, comment string) string {
	newCode = strings.TrimRight(newCode, " \t")
	if comment == "" {
		return newCode
	}
	trimmed := strings.TrimRight(oldCode, " \t")
	if col := len(oldCode); len(oldCode)-len(trimmed) > 1 && len(newCode) < col {
		return newCode + strings.Repeat(" ", col-len(newCode)) + comment
	}
	return newCode + " " + comment
}

// flowLines returns the single-line flow entries, with the byte offset where
// each value starts.
func (d *doc) flowLines() (lines []string, starts []int) {
	for _, e := range d.entries {
		if e.singleLineFlow() {
			l := d.lines[e.keyLine]
			lines = append(lines, l)
			starts = append(starts, byteCol(l, e.val.Column-1))
		}
	}
	return lines, starts
}

// alignPad returns the spaces between "name:" (prefixLen bytes) and a flow
// value: up to the column the file aligns its flow values at, if it does and
// the name fits, or one space.
func (d *doc) alignPad(prefixLen int) string {
	if col := d.alignCol(); col > prefixLen {
		return strings.Repeat(" ", col-prefixLen)
	}
	return " "
}

// alignCol returns the byte column at which the one-line flow entries align
// their values ("tracking:    { … }"), or 0 if they don't: it takes two
// entries at the same column, one of them padded with extra spaces.
func (d *doc) alignCol() int {
	lines, starts := d.flowLines()
	count := map[int]int{}
	padded := map[int]bool{}
	for i, s := range starts {
		count[s]++
		if s >= 2 && lines[i][s-2] == ' ' {
			padded[s] = true // more than one space after the colon
		}
	}
	best := 0
	for col, n := range count {
		if n >= 2 && padded[col] && (n > count[best] || n == count[best] && col > best) {
			best = col
		}
	}
	return best
}

// braceSpace reports whether the file writes "{ cmd: x }" rather than
// "{cmd: x}"; the spaced form when it has no flow entries.
func (d *doc) braceSpace() bool {
	if d.braces != 0 {
		return d.braces > 0
	}
	lines, starts := d.flowLines()
	votes := 0
	for i, s := range starts {
		if l := lines[i]; s+1 < len(l) && l[s] == '{' && l[s+1] != '}' {
			if l[s+1] == ' ' {
				votes++
			} else {
				votes--
			}
		}
	}
	d.braces = 1
	if votes < 0 {
		d.braces = -1
	}
	return d.braces > 0
}
