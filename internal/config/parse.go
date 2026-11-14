package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Problem is one thing wrong with a config file.
type Problem struct {
	// Line is the 1-based line in the file, 0 when unknown.
	Line int
	// Process is the entry the problem belongs to, "" for top-level ones.
	Process string
	// Msg says what is wrong, phrased so the reader knows what to fix.
	Msg string
}

// ValidationError lists every problem found in a config, in line order.
type ValidationError struct {
	Path     string
	Problems []Problem
}

// Error lists the problems, one "path:line: …" per line.
func (e *ValidationError) Error() string {
	var b strings.Builder
	for i, p := range e.Problems {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(e.Path)
		if p.Line > 0 {
			fmt.Fprintf(&b, ":%d", p.Line)
		}
		b.WriteString(": ")
		if p.Process != "" {
			fmt.Fprintf(&b, "process %q: ", p.Process)
		}
		b.WriteString(p.Msg)
	}
	return b.String()
}

func (e *ValidationError) add(line int, process, msg string) {
	e.Problems = append(e.Problems, Problem{Line: line, Process: process, Msg: msg})
}

func (e *ValidationError) orNil() error {
	if len(e.Problems) == 0 {
		return nil
	}
	sort.SliceStable(e.Problems, func(i, j int) bool { return e.Problems[i].Line < e.Problems[j].Line })
	return e
}

// Load reads and parses the config file at path. See Parse.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(path, data)
}

// Parse decodes and validates a rov.yaml held in data. path is used in error
// messages and recorded as Config.Path.
//
// Invalid YAML returns a nil Config. Any other problem returns a
// *ValidationError listing all of them together with a Config holding
// everything that could be read, so that an editor can still work on a file
// with mistakes in it.
func Parse(path string, data []byte) (*Config, error) {
	d := decoder{cfg: Default(path), errs: &ValidationError{Path: path}}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: invalid YAML: %s", path, strings.TrimPrefix(err.Error(), "yaml: "))
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		d.errs.add(extra.Line, "", "the file holds more than one YAML document (remove the extra '---')")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: invalid YAML: %s", path, strings.TrimPrefix(err.Error(), "yaml: "))
	}

	if len(doc.Content) > 0 {
		root := deref(doc.Content[0])
		switch {
		case isNull(root):
		case root.Kind != yaml.MappingNode:
			return nil, fmt.Errorf("%s:%d: the top level must be a mapping of log_dir, log_max and processes", path, root.Line)
		default:
			d.top(root)
		}
	}
	d.uniqueNames()
	return d.cfg, d.errs.orNil()
}

// decoder walks the node tree, filling cfg and collecting problems instead of
// stopping at the first one.
type decoder struct {
	cfg   *Config
	errs  *ValidationError
	lines []entryLine // where each process starts, for uniqueness errors
}

var (
	topKeys     = []string{"log_dir", "log_max", "processes"}
	processKeys = []string{"cmd", "cwd", "env", "description", "docker"}
)

type entryLine struct {
	name string
	line int
}

func (d *decoder) top(root *yaml.Node) {
	seen := map[string]int{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		k, v := root.Content[i], deref(root.Content[i+1])
		key, ok := d.key(k, "")
		if !ok {
			continue
		}
		if prev, dup := seen[key]; dup {
			d.errs.add(k.Line, "", fmt.Sprintf("%s is set twice (first on line %d)", key, prev))
			continue
		}
		seen[key] = k.Line
		switch key {
		case "log_dir":
			if s, ok := d.scalar(v, "", "log_dir"); ok {
				if err := ValidLogDir(s); err != nil {
					d.errs.add(v.Line, "", err.Error())
				} else {
					d.cfg.LogDir = s
				}
			}
		case "log_max":
			if s, ok := d.scalar(v, "", "log_max"); ok {
				n, err := ParseSize(s)
				if err != nil {
					d.errs.add(v.Line, "", "log_max: "+err.Error())
				} else {
					d.cfg.LogMax = n
				}
			}
		case "processes":
			d.processes(v)
		default:
			msg := fmt.Sprintf("unknown key %q", key)
			if v.Kind == yaml.MappingNode && hasKey(v, "cmd") {
				msg += " (processes go under processes:)"
			} else if s := suggest(key, topKeys); s != "" {
				msg += fmt.Sprintf(" (did you mean %q?)", s)
			} else {
				msg += " (expected " + strings.Join(topKeys, ", ") + ")"
			}
			d.errs.add(k.Line, "", msg)
		}
	}
}

func (d *decoder) processes(v *yaml.Node) {
	if isNull(v) {
		return
	}
	if v.Kind != yaml.MappingNode {
		d.errs.add(v.Line, "", "processes must be a mapping of name: { cmd: … }")
		return
	}
	for i := 0; i+1 < len(v.Content); i += 2 {
		k, pv := v.Content[i], deref(v.Content[i+1])
		name, ok := d.key(k, "")
		if !ok {
			continue
		}
		p := d.process(name, k.Line, pv)
		d.cfg.Processes = append(d.cfg.Processes, p)
		d.lines = append(d.lines, entryLine{name, k.Line})
	}
}

func (d *decoder) process(name string, line int, v *yaml.Node) Process {
	p := Process{Name: name}
	fieldLine := map[string]int{"": line}
	switch {
	case isNull(v):
	case v.Kind != yaml.MappingNode:
		d.errs.add(v.Line, name, "must be a mapping such as { cmd: … }")
	default:
		seen := map[string]bool{}
		for i := 0; i+1 < len(v.Content); i += 2 {
			k, fv := v.Content[i], deref(v.Content[i+1])
			key, ok := d.key(k, name)
			if !ok {
				continue
			}
			if seen[key] {
				d.errs.add(k.Line, name, key+" is set twice")
				continue
			}
			seen[key] = true
			fieldLine[key] = k.Line
			switch key {
			case "cmd":
				p.Cmd, _ = d.scalar(fv, name, key)
			case "cwd":
				p.Cwd, _ = d.scalar(fv, name, key)
			case "description":
				p.Description, _ = d.scalar(fv, name, key)
			case "docker":
				p.Docker, _ = d.scalar(fv, name, key)
			case "env":
				p.Env = d.env(fv, name)
			default:
				msg := fmt.Sprintf("unknown key %q", key)
				if s := suggest(key, processKeys); s != "" {
					msg += fmt.Sprintf(" (did you mean %q?)", s)
				} else {
					msg += " (expected " + strings.Join(processKeys, ", ") + ")"
				}
				d.errs.add(k.Line, name, msg)
			}
		}
	}
	for _, fp := range checkProcess(p) {
		l, ok := fieldLine[fp.field]
		if !ok {
			l = line
		}
		d.errs.add(l, name, fp.msg)
	}
	return p
}

func (d *decoder) env(v *yaml.Node, name string) []EnvVar {
	if isNull(v) {
		return nil
	}
	if v.Kind != yaml.MappingNode {
		d.errs.add(v.Line, name, "env must be a mapping of KEY: value")
		return nil
	}
	var env []EnvVar
	for i := 0; i+1 < len(v.Content); i += 2 {
		k, ev := v.Content[i], deref(v.Content[i+1])
		key, ok := d.key(k, name)
		if !ok {
			continue
		}
		val, ok := d.scalar(ev, name, "env "+key)
		if ok {
			env = append(env, EnvVar{Key: key, Value: val})
		}
	}
	return env
}

// key reads a mapping key, which must be a plain scalar.
func (d *decoder) key(k *yaml.Node, process string) (string, bool) {
	k = deref(k)
	if k.Kind != yaml.ScalarNode {
		d.errs.add(k.Line, process, "mapping keys must be plain strings")
		return "", false
	}
	return k.Value, true
}

// scalar reads a string-valued field. Numbers and booleans are taken as
// written (PORT: 3000 is "3000"); null is the empty string.
func (d *decoder) scalar(v *yaml.Node, process, field string) (string, bool) {
	if isNull(v) {
		return "", true
	}
	if v.Kind != yaml.ScalarNode {
		d.errs.add(v.Line, process, field+" must be a string")
		return "", false
	}
	return v.Value, true
}

func (d *decoder) uniqueNames() {
	first := map[string]int{}
	for _, e := range d.lines {
		if prev, dup := first[e.name]; dup {
			d.errs.add(e.line, e.name, fmt.Sprintf("defined more than once (first on line %d)", prev))
			continue
		}
		first[e.name] = e.line
	}
}

// deref follows aliases, so anchors in rov.yaml work as in any YAML file.
func deref(n *yaml.Node) *yaml.Node {
	for i := 0; n.Kind == yaml.AliasNode && n.Alias != nil && i < 32; i++ {
		n = n.Alias
	}
	return n
}

func isNull(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null"
}

func hasKey(m *yaml.Node, key string) bool {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return true
		}
	}
	return false
}
