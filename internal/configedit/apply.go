package configedit

import (
	"fmt"
	"reflect"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/labsbit/rov/internal/config"
)

// Apply applies op to the rov.yaml held in src and returns the new content.
// It is pure: nothing is read or written. An empty src is an empty file; Add
// then creates the processes mapping.
//
// The result is validated with the same rules as config.Parse, so Apply
// fails (with a *config.ValidationError) when the file has problems the op
// doesn't fix. A no-op — a Set to the current values, a Move to where the
// entry already is — returns src unchanged.
func Apply(src []byte, op Op) ([]byte, error) {
	return apply(config.FileName, src, op)
}

func apply(path string, src []byte, op Op) (_ []byte, err error) {
	// A layout the text edit doesn't expect must fail the edit, never crash
	// the CLI or the TUI.
	defer func() {
		if r := recover(); r != nil {
			err = internalError(path, op, fmt.Sprintf("panic: %v", r))
		}
	}()
	before, err := config.Parse(path, src)
	if before == nil {
		return nil, err
	}
	pl, err := resolve(before.Processes, op)
	if err != nil {
		return nil, err
	}
	out := src
	if !sameProcesses(before.Processes, pl.procs) {
		d, err := parseDoc(src)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if out, err = d.edit(op, pl, before.Processes); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if detail := d.lostComment(op, pl, out); detail != "" {
			return nil, internalError(path, op, detail)
		}
	}

	// Check the text edit did exactly what the plan says, and nothing else.
	after, verr := config.Parse(path, out)
	switch {
	case after == nil:
		return nil, internalError(path, op, verr.Error())
	case !sameProcesses(after.Processes, pl.procs):
		return nil, internalError(path, op, fmt.Sprintf("got processes %v, want %v", after.Processes, pl.procs))
	case after.LogDir != before.LogDir || after.LogMax != before.LogMax:
		return nil, internalError(path, op, "the log settings changed")
	}
	if verr != nil {
		return nil, verr
	}
	return out, nil
}

// lostComment checks that the comment lines of src outside the entry the op
// edits are all in out, in the same order, and those of a moved entry too
// (in their own order). It returns what went missing, or "".
func (d *doc) lostComment(op Op, pl plan, out []byte) string {
	inEntry := func(l int) bool { return false }
	if _, add := op.(Add); !add && pl.from >= 0 && pl.from < len(d.entries) {
		e := d.entries[pl.from]
		inEntry = func(l int) bool { return l >= e.start && l <= e.end }
	}
	var outside, inside []string
	for l, line := range d.lines {
		if !isComment(line) {
			continue
		}
		if inEntry(l) {
			inside = append(inside, line)
		} else {
			outside = append(outside, line)
		}
	}
	var got []string
	for line := range strings.SplitAfterSeq(string(out), "\n") {
		if isComment(line) {
			got = append(got, line)
		}
	}
	if c, ok := subsequence(outside, got); !ok {
		return fmt.Sprintf("the comment %q would be lost", strings.TrimSpace(c))
	}
	if _, move := op.(Move); move {
		if c, ok := subsequence(inside, got); !ok {
			return fmt.Sprintf("the comment %q would be lost", strings.TrimSpace(c))
		}
	}
	return ""
}

// subsequence reports whether want appears in got in order, comparing lines
// without their line endings; if not, it returns the first line missing.
func subsequence(want, got []string) (string, bool) {
	trim := func(s string) string { return strings.TrimRight(s, "\r\n") }
	j := 0
	for _, w := range want {
		for j < len(got) && trim(got[j]) != trim(w) {
			j++
		}
		if j == len(got) {
			return w, false
		}
		j++
	}
	return "", true
}

func internalError(path string, op Op, detail string) error {
	return fmt.Errorf("%s was left alone: rov could not apply %T to this layout without touching other entries (%s); edit the file by hand", path, op, detail)
}

func sameProcesses(a, b []config.Process) bool {
	norm := func(ps []config.Process) []config.Process {
		if len(ps) == 0 {
			return nil
		}
		out := slices.Clone(ps)
		for i := range out {
			if len(out[i].Env) == 0 {
				out[i].Env = nil
			}
		}
		return out
	}
	return reflect.DeepEqual(norm(a), norm(b))
}

// edit applies the plan to the text. procs is the list before the op, in
// entry order.
func (d *doc) edit(op Op, pl plan, procs []config.Process) ([]byte, error) {
	if len(procs) != len(d.entries) {
		return nil, fmt.Errorf("%w: found %d entries but %d processes", errUnsupported, len(d.entries), len(procs))
	}
	head := slices.Clone(d.lines[:min(d.procLine+1, len(d.lines))])
	tailStart := d.regionEnd

	switch op := op.(type) {
	case Add:
		body := d.renderEntry(op.Proc, "")
		if !d.hasProcs {
			out := slices.Clone(d.lines)
			out = append(out, "processes:"+d.eol)
			return d.join(append(out, body...)), nil
		}
		if d.inlineVal {
			// "processes: {}" → "processes:", keeping a trailing comment.
			line := head[d.procLine]
			code, comment := splitComment(line)
			k := d.procCol
			colon := colonAfter(code, k)
			if colon < 0 {
				return nil, fmt.Errorf("%w: line %d", errUnsupported, d.procLine+1)
			}
			head[d.procLine] = appendComment(code[:colon+1], comment) + d.eol
		}
		r := d.region()
		r.insert(pl.to, body)
		return d.join(head, r.lines(), d.lines[tailStart:]), nil

	case Remove:
		r := d.region()
		r.remove(pl.from)
		return d.join(head, r.lines(), d.lines[tailStart:]), nil

	case Move:
		r := d.region()
		if pl.swap {
			r.bodies[pl.from], r.bodies[pl.to] = r.bodies[pl.to], r.bodies[pl.from]
		} else {
			body := r.bodies[pl.from]
			r.remove(pl.from)
			r.insert(pl.to, body)
		}
		return d.join(head, r.lines(), d.lines[tailStart:]), nil

	case Set:
		body, err := d.setEntry(d.entries[pl.from], procs[pl.from], pl.procs[pl.from])
		if err != nil {
			return nil, err
		}
		r := d.region()
		r.bodies[pl.from] = body
		return d.join(head, r.lines(), d.lines[tailStart:]), nil
	}
	return nil, fmt.Errorf("unknown op %T", op)
}

func (d *doc) join(parts ...[]string) []byte {
	var b strings.Builder
	for _, p := range parts {
		for _, l := range p {
			b.WriteString(l)
		}
	}
	s := b.String()
	if d.noFinalEOL {
		s = strings.TrimSuffix(s, d.eol)
	}
	return []byte(s)
}

// setEntry returns the new lines of entry e, changed from old to p.
func (d *doc) setEntry(e *entry, old, p config.Process) ([]string, error) {
	body := slices.Clone(d.lines[e.start : e.end+1])
	k := e.keyLine - e.start
	gains := false
	had := present(old)
	for f := range present(p) {
		if !had[f] {
			gains = true
		}
	}

	switch {
	case e.block(d):
		var err error
		if body, err = d.setBlock(e, old, p); err != nil {
			return nil, err
		}
	case e.singleLineFlow() && !gains:
		// Rewrite the value in place, in its own key order and brace style.
		var order []string
		for i := 0; i+1 < len(e.val.Content); i += 2 {
			order = append(order, e.val.Content[i].Value)
		}
		line := body[k]
		code, comment := splitComment(line)
		vs := byteCol(line, e.val.Column-1)
		flow := d.flowText(flowMapping(p, order), spacedAt(line, vs))
		body[k] = withComment(code[:vs]+flow, code, comment) + d.eol
	default:
		// Anything else ("name: ~", a flow mapping over several lines, or a
		// flow entry gaining fields) is rendered afresh under its head comment.
		_, comment := splitComment(body[k])
		renamed := p
		renamed.Name = old.Name // renamed below, like the other styles
		body = append(body[:k:k], d.renderEntry(renamed, comment)...)
	}

	if p.Name != old.Name {
		line, err := d.renameKey(body[k], e, p.Name)
		if err != nil {
			return nil, err
		}
		body[k] = line
	}
	return body, nil
}

// renameKey replaces the key on an entry's key line. A value on the same
// line keeps its column if it was aligned, else its single space.
func (d *doc) renameKey(line string, e *entry, name string) (string, error) {
	code, comment := splitComment(line)
	ks := byteCol(code, e.key.Column-1)
	colon := colonAfter(code, ks)
	if colon < 0 {
		return "", fmt.Errorf("%w: can't find the key of %q on line %d", errUnsupported, e.name, e.keyLine+1)
	}
	prefix := code[:ks] + d.keyText(name) + ":"
	rest := code[colon+1:]
	value := strings.TrimLeft(rest, " ")
	if value == "" {
		return appendComment(prefix, comment) + d.eol, nil
	}
	pad := len(rest) - len(value)
	if vs := colon + 1 + pad; pad > 1 || vs == d.alignCol() {
		pad = max(1, vs-len(prefix))
	}
	newCode := prefix + strings.Repeat(" ", pad) + value
	return withComment(newCode, code, comment) + d.eol, nil
}

// setBlock rewrites the changed fields of a block entry. Untouched fields,
// comments and blank lines are copied; a new field goes before the first
// existing field that fieldOrder puts after it.
func (d *doc) setBlock(e *entry, old, p config.Process) ([]string, error) {
	fields := d.pairs(e.val, e.keyLine, e.end)
	if !inOrder(fields, e.keyLine+1, e.end) {
		return nil, fmt.Errorf("%w: can't tell where the fields of %q start and end (line %d)", errUnsupported, e.name, e.keyLine+1)
	}
	col := d.indent + d.step
	if len(fields) > 0 {
		col = fields[0].key.Column - 1
	}
	inFile := map[string]bool{}
	for _, f := range fields {
		inFile[f.name] = true
	}
	changed := func(name string) bool {
		return !reflect.DeepEqual(fieldValue(old, name, false), fieldValue(p, name, false))
	}
	var pending []string // fields to add, in fieldOrder
	for _, f := range fieldOrder {
		if !inFile[f] && changed(f) {
			pending = append(pending, f)
		}
	}
	emitPending := func(out []string, before int) []string {
		for len(pending) > 0 && (before < 0 || rank(pending[0]) < before) {
			out = append(out, d.renderField(pending[0], fieldValue(p, pending[0], false), col, d.braceSpace())...)
			pending = pending[1:]
		}
		return out
	}

	out := slices.Clone(d.lines[e.start : e.keyLine+1])
	cursor := e.keyLine + 1
	done := map[string]bool{}
	for _, f := range fields {
		out = append(out, d.lines[cursor:f.start]...)
		if r := rank(f.name); r >= 0 {
			out = emitPending(out, r)
		}
		cursor = f.end + 1
		if rank(f.name) < 0 || done[f.name] || !changed(f.name) {
			out = append(out, d.lines[f.start:f.end+1]...)
			done[f.name] = true
			continue
		}
		done[f.name] = true
		flow := f.val.Kind == yaml.MappingNode && f.val.Style&yaml.FlowStyle != 0
		v := fieldValue(p, f.name, flow)
		if v == nil {
			continue // removed, with its head comment
		}
		out = append(out, d.lines[f.start:f.keyLine]...)
		if f.name == "env" {
			if lines, ok := d.setEnv(f, old.Env, p.Env); ok {
				out = append(out, lines...)
				continue
			}
		}
		line := d.lines[f.keyLine]
		rendered := d.renderField(f.name, v, col, flow && spacedAt(line, byteCol(line, f.val.Column-1)))
		code, comment := splitComment(line)
		rendered[0] = withComment(strings.TrimSuffix(rendered[0], d.eol), code, comment) + d.eol
		out = append(out, rendered...)
	}
	out = emitPending(out, -1)
	return append(out, d.lines[cursor:e.end+1]...), nil
}

// inOrder reports whether the fields lie one after the other within lines
// from..to (inclusive).
func inOrder(fs []*field, from, to int) bool {
	for _, f := range fs {
		if f.start < from || f.keyLine < f.start || f.end < f.keyLine || f.end > to {
			return false
		}
		from = f.end + 1
	}
	return true
}

// setEnv rewrites a block env map variable by variable, so comments on the
// variables that stay survive. It works when the new env keeps the remaining
// variables in file order and adds new ones at the end, as an edit of a
// KEY=VALUE list does; otherwise it returns false and the map is rendered
// afresh. The lines returned run from the env key line to the field's end.
func (d *doc) setEnv(f *field, old, now []config.EnvVar) ([]string, bool) {
	if f.val.Kind != yaml.MappingNode || f.val.Style&yaml.FlowStyle != 0 {
		return nil, false
	}
	vars := d.pairs(f.val, f.keyLine, f.end)
	if len(vars) == 0 || len(vars) != len(old) || !inOrder(vars, f.keyLine+1, f.end) {
		return nil, false
	}
	want := make(map[string]string, len(now))
	for _, e := range now {
		want[e.Key] = e.Value
	}
	kept := 0
	for _, v := range vars {
		if _, ok := want[v.name]; ok {
			if kept >= len(now) || now[kept].Key != v.name {
				return nil, false
			}
			kept++
		}
	}

	col := vars[0].key.Column - 1
	out := slices.Clone(d.lines[f.keyLine : f.keyLine+1])
	cursor := f.keyLine + 1
	for i, v := range vars {
		out = append(out, d.lines[cursor:v.start]...)
		cursor = v.end + 1
		val, ok := want[v.name]
		switch {
		case !ok:
			// dropped, with its head comment
		case val == old[i].Value:
			out = append(out, d.lines[v.start:v.end+1]...)
		default:
			out = append(out, d.lines[v.start:v.keyLine]...)
			line := d.lines[v.keyLine]
			rendered := d.renderField(v.name, envValue(val), col, false)
			code, comment := splitComment(line)
			rendered[0] = withComment(strings.TrimSuffix(rendered[0], d.eol), code, comment) + d.eol
			out = append(out, rendered...)
		}
	}
	for _, e := range now[kept:] {
		out = append(out, d.renderField(e.Key, envValue(e.Value), col, false)...)
	}
	return append(out, d.lines[cursor:f.end+1]...), true
}

// spacedAt reports whether the flow mapping starting at byte i of line has a
// space inside its opening brace.
func spacedAt(line string, i int) bool {
	return i+1 < len(line) && line[i] == '{' && line[i+1] == ' '
}
