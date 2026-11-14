// Package configedit edits rov.yaml in place, keeping comments, key order,
// quoting and the layout of every entry it doesn't touch.
//
// The CLI (rov config add|set|rm|mv) and the TUI forms share it. Edits are
// values (Op), so one can be re-applied to a file that changed on disk in the
// meantime (see Write).
//
// # How the file is edited
//
// yaml.v3 can round-trip a node tree, but its encoder re-indents the whole
// document, drops the column alignment of line comments and of flow values
// ("tracking:    { cmd: … }") and rewrites "{ cmd: x }" as "{cmd: x}". So
// instead of re-encoding the document, Apply uses the node tree only to locate
// things — the processes mapping, each entry and each field, by line — and
// splices text:
//
//   - Lines outside the edited entry are copied byte for byte. A no-op edit
//     returns the input unchanged.
//   - An entry is its key line, its value lines and the comment lines directly
//     above it (its head comment). Remove and move carry the head comment
//     along; blank lines and comments separated by a blank line stay put.
//   - Set rewrites only the fields it changes. A changed field is rendered by
//     yaml.v3 (so quoting is always correct) at the entry's indentation, and
//     keeps its trailing comment, at the same column where it fits. A
//     block env map is edited variable by variable, so comments on the
//     variables that stay survive; an env that reorders them is rendered
//     afresh.
//   - A new entry is flow style ("name: { cmd: … }") when it has only cmd,
//     aligned with the other one-line entries, and block style otherwise. A
//     flow entry that gains a field becomes block style; one that doesn't
//     stays flow.
//
// Every result is parsed again and compared with the intended change before
// it is returned, so an edit can fail but never silently alter another entry.
//
// Not supported (an error says so): processes, or the whole file, written as
// a flow mapping ("processes: { a: … }"), and edits that would break an alias.
package configedit

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/labsbit/rov/internal/config"
)

// Op is one edit of rov.yaml: Add, Set, Remove or Move.
type Op interface {
	// touches lists, for conflict detection, the processes whose entries the
	// op changes and those its position refers to.
	touches() (edits, refs []string)
}

// Add inserts a new process at Pos.
type Add struct {
	Proc config.Process
	Pos  Position
}

// Set changes fields of the process Name.
type Set struct {
	Name  string
	Patch Patch
}

// Remove deletes the process Name and its head comment.
type Remove struct {
	Name string
}

// Move moves the process Name to Pos, with its comments.
type Move struct {
	Name string
	Pos  Position
}

// Patch lists the fields Set changes. A nil field is left alone; a pointer to
// the empty value removes the field from the entry (cmd can't be removed).
type Patch struct {
	Cmd, Cwd, Description, Docker *string
	Env                           *[]config.EnvVar
	// Rename gives the process a new name. Its position and comments stay.
	Rename *string
}

// Position says where Add puts a new entry and Move puts an existing one. At
// most one field may be set; the zero value means Last. Up and Down apply to
// Move only and are no-ops at the ends.
type Position struct {
	First, Last, Up, Down bool
	// After names the process to place the entry after.
	After string
}

func (o Add) touches() (edits, refs []string)    { return nonEmpty(o.Proc.Name), nonEmpty(o.Pos.After) }
func (o Remove) touches() (edits, refs []string) { return nonEmpty(o.Name), nil }
func (o Move) touches() (edits, refs []string)   { return nonEmpty(o.Name), nonEmpty(o.Pos.After) }
func (o Set) touches() (edits, refs []string) {
	if o.Patch.Rename != nil {
		return nonEmpty(o.Name, *o.Patch.Rename), nil
	}
	return nonEmpty(o.Name), nil
}

func nonEmpty(names ...string) []string {
	return slices.DeleteFunc(names, func(s string) bool { return s == "" })
}

// String names the position as the CLI flags do: "first", "after api", ….
func (p Position) String() string {
	switch {
	case p.First:
		return "first"
	case p.Up:
		return "up"
	case p.Down:
		return "down"
	case p.After != "":
		return "after " + p.After
	}
	return "last"
}

func (p Position) check(move bool) error {
	n := 0
	for _, b := range []bool{p.First, p.Last, p.Up, p.Down, p.After != ""} {
		if b {
			n++
		}
	}
	if n > 1 {
		return errors.New("give one position: first, last, up, down or after <name>")
	}
	if !move && (p.Up || p.Down) {
		return errors.New("up and down only move an existing process; use first, last or after <name>")
	}
	return nil
}

// plan is the effect of an op on the list of processes.
type plan struct {
	procs []config.Process // the list afterwards
	from  int              // index of the entry the op works on (-1 for Add)
	to    int              // Add/Move: index in the list without the entry; Set: = from
	swap  bool             // Move up/down: exchange with the neighbour
}

// resolve applies op to procs, checking names and positions.
func resolve(procs []config.Process, op Op) (plan, error) {
	index := func(name string) int {
		return slices.IndexFunc(procs, func(p config.Process) bool { return p.Name == name })
	}
	missing := func(name string) error {
		names := make([]string, len(procs))
		for i, p := range procs {
			names[i] = p.Name
		}
		if len(names) == 0 {
			return fmt.Errorf("no process %q in rov.yaml (it has none)", name)
		}
		return fmt.Errorf("no process %q in rov.yaml (it has %s)", name, strings.Join(names, ", "))
	}
	// insertAt returns the index pos means in rest, the list without the entry.
	insertAt := func(rest []config.Process, self string, pos Position) (int, error) {
		switch {
		case pos.First:
			return 0, nil
		case pos.After != "":
			if pos.After == self {
				return 0, fmt.Errorf("can't place %q after itself", self)
			}
			i := slices.IndexFunc(rest, func(p config.Process) bool { return p.Name == pos.After })
			if i < 0 {
				return 0, missing(pos.After)
			}
			return i + 1, nil
		}
		return len(rest), nil
	}

	switch op := op.(type) {
	case Add:
		if err := op.Pos.check(false); err != nil {
			return plan{}, err
		}
		if err := config.ValidName(op.Proc.Name); err != nil {
			return plan{}, err
		}
		if index(op.Proc.Name) >= 0 {
			return plan{}, fmt.Errorf("process %q already exists in rov.yaml", op.Proc.Name)
		}
		to, err := insertAt(procs, op.Proc.Name, op.Pos)
		if err != nil {
			return plan{}, err
		}
		return plan{procs: slices.Insert(slices.Clone(procs), to, op.Proc), from: -1, to: to}, nil

	case Set:
		i := index(op.Name)
		if i < 0 {
			return plan{}, missing(op.Name)
		}
		p, err := op.Patch.apply(procs[i])
		if err != nil {
			return plan{}, err
		}
		if p.Name != op.Name && index(p.Name) >= 0 {
			return plan{}, fmt.Errorf("can't rename %q to %q: that name is taken", op.Name, p.Name)
		}
		out := slices.Clone(procs)
		out[i] = p
		return plan{procs: out, from: i, to: i}, nil

	case Remove:
		i := index(op.Name)
		if i < 0 {
			return plan{}, missing(op.Name)
		}
		return plan{procs: slices.Delete(slices.Clone(procs), i, i+1), from: i, to: -1}, nil

	case Move:
		if err := op.Pos.check(true); err != nil {
			return plan{}, err
		}
		i := index(op.Name)
		if i < 0 {
			return plan{}, missing(op.Name)
		}
		rest := slices.Delete(slices.Clone(procs), i, i+1)
		var to int
		swap := false
		switch {
		case op.Pos.Up:
			to, swap = max(i-1, 0), true
		case op.Pos.Down:
			to, swap = min(i+1, len(rest)), true
		default:
			var err error
			if to, err = insertAt(rest, op.Name, op.Pos); err != nil {
				return plan{}, err
			}
		}
		return plan{procs: slices.Insert(rest, to, procs[i]), from: i, to: to, swap: swap}, nil
	}
	return plan{}, fmt.Errorf("configedit: unknown op %T", op)
}

// apply returns p with the patch applied.
func (pt Patch) apply(p config.Process) (config.Process, error) {
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&p.Cmd, pt.Cmd)
	set(&p.Cwd, pt.Cwd)
	set(&p.Description, pt.Description)
	set(&p.Docker, pt.Docker)
	if pt.Env != nil {
		p.Env = slices.Clone(*pt.Env)
	}
	if pt.Rename != nil {
		if err := config.ValidName(*pt.Rename); err != nil {
			return p, err
		}
		p.Name = *pt.Rename
	}
	return p, nil
}
