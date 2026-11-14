package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// flagDef declares one flag of a command.
type flagDef struct {
	names []string // e.g. {"-n", "--lines"}; the first is the key
	value bool     // takes a value (next argument, or after '=')
}

// args is a parsed command line: positional arguments (flags may come after
// them, as in `rov logs gateway -n 200`), flag values, and everything after
// a "--".
type args struct {
	pos      []string
	vals     map[string][]string
	rest     []string
	dashdash bool
}

// parseArgs parses argv against defs for a command that takes no command
// line of its own: "--" only ends flag parsing, and what follows it is
// positional (`rov stop -- web` stops web). A lone "-" is positional.
// Unknown flags are usage errors.
func parseArgs(argv []string, defs ...flagDef) (*args, error) {
	a, err := parseCmdArgs(argv, defs...)
	if err != nil {
		return nil, err
	}
	a.pos = append(a.pos, a.rest...)
	a.rest = nil
	return a, nil
}

// parseCmdArgs is parseArgs for a command that takes a command line after
// "--" (spawn, config add/set): the rest is kept verbatim in rest.
func parseCmdArgs(argv []string, defs ...flagDef) (*args, error) {
	a := &args{vals: map[string][]string{}}
	lookup := map[string]flagDef{}
	for _, d := range defs {
		for _, n := range d.names {
			lookup[n] = d
		}
	}
	for i := 0; i < len(argv); i++ {
		s := argv[i]
		if s == "--" {
			a.dashdash = true
			a.rest = argv[i+1:]
			break
		}
		if len(s) < 2 || s[0] != '-' {
			a.pos = append(a.pos, s)
			continue
		}
		name, val, hasVal := strings.Cut(s, "=")
		d, ok := lookup[name]
		if !ok {
			return nil, usagef("unknown flag %s", name)
		}
		key := d.names[0]
		switch {
		case !d.value && hasVal:
			return nil, usagef("%s takes no value", name)
		case !d.value:
			a.vals[key] = append(a.vals[key], "")
		case hasVal:
			a.vals[key] = append(a.vals[key], val)
		case i+1 < len(argv):
			i++
			a.vals[key] = append(a.vals[key], argv[i])
		default:
			return nil, usagef("%s needs a value", name)
		}
	}
	return a, nil
}

func (a *args) has(key string) bool { return len(a.vals[key]) > 0 }

// str returns the last value of key, "" if absent.
func (a *args) str(key string) string {
	v := a.vals[key]
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1]
}

// strPtr returns the last value of key, nil if the flag wasn't given.
func (a *args) strPtr(key string) *string {
	if !a.has(key) {
		return nil
	}
	s := a.str(key)
	return &s
}

func (a *args) all(key string) []string { return a.vals[key] }

// int returns key as a non-negative integer, def if absent.
func (a *args) int(key string, def int) (int, error) {
	if !a.has(key) {
		return def, nil
	}
	n, err := strconv.Atoi(a.str(key))
	if err != nil || n < 0 {
		return 0, usagef("%s wants a whole number, got %q", key, a.str(key))
	}
	return n, nil
}

// duration returns key as a duration ("90s", "2m", or plain seconds), def
// if absent.
func (a *args) duration(key string, def time.Duration) (time.Duration, error) {
	if !a.has(key) {
		return def, nil
	}
	return parseDuration(a.str(key))
}

func parseDuration(s string) (time.Duration, error) {
	if n, err := strconv.ParseFloat(s, 64); err == nil && n > 0 {
		return time.Duration(n * float64(time.Second)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, usagef("invalid duration %q (use e.g. 90s, 2m, or seconds)", s)
	}
	return d, nil
}

// exactly checks the number of positional arguments.
func (a *args) exactly(n int, what string) error {
	switch {
	case len(a.pos) < n:
		return usagef("missing %s", what)
	case len(a.pos) > n:
		return usagef("unexpected argument %q", a.pos[n])
	}
	return nil
}

// usageError is a command-line mistake: exit 2.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return &usageError{fmt.Sprintf(format, a...)} }

// Common flags.
var (
	fJSON = flagDef{names: []string{"--json"}}
)
