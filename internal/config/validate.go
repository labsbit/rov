package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var (
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	// Compose's own rule for project names.
	composeRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// ValidName checks a process name. Names double as tmux session names, so
// they can't hold ':' or '.' (tmux target separators) or upper case.
func ValidName(name string) error {
	switch {
	case name == "":
		return errors.New("name is empty")
	case !nameRe.MatchString(name):
		return fmt.Errorf("invalid name %q: use lower-case letters, digits, '-' and '_', starting with a letter or digit", name)
	}
	return nil
}

// ValidEnvKey checks an environment variable name.
func ValidEnvKey(key string) error {
	switch {
	case key == "":
		return errors.New("env key is empty")
	case !envKeyRe.MatchString(key):
		return fmt.Errorf("invalid env key %q: use letters, digits and '_', not starting with a digit", key)
	}
	return nil
}

// ValidDockerProject checks a compose project name as used by `docker:`.
func ValidDockerProject(project string) error {
	if !composeRe.MatchString(project) {
		return fmt.Errorf("invalid compose project %q: compose allows lower-case letters, digits, '-' and '_', starting with a letter or digit", project)
	}
	return nil
}

// validCwd checks the syntax of a working dir: relative and inside the root.
// It doesn't touch the file system; CheckCwd does.
func validCwd(cwd string) error {
	if cwd == "" {
		return nil
	}
	if filepath.IsAbs(cwd) {
		return fmt.Errorf("cwd %q must be relative to the project root", cwd)
	}
	clean := filepath.Clean(cwd)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("cwd %q leaves the project root", cwd)
	}
	return nil
}

// ValidLogDir checks log_dir: a relative path to a directory inside the
// root, not the root itself. rov deletes old log files in it (see logs.GC),
// so it must be a directory of its own.
func ValidLogDir(dir string) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("log_dir is empty (omit it for the default " + DefaultLogDir + ")")
	}
	if filepath.IsAbs(dir) {
		return fmt.Errorf("log_dir %q must be relative to the project root", dir)
	}
	switch clean := filepath.Clean(dir); {
	case clean == "..", strings.HasPrefix(clean, ".."+string(filepath.Separator)):
		return fmt.Errorf("log_dir %q leaves the project root", dir)
	case clean == ".":
		return fmt.Errorf("log_dir %q is the project root; give it a directory of its own, such as %s", dir, DefaultLogDir)
	}
	return nil
}

// CheckCwd checks that cwd is a relative path that stays inside root and
// names an existing directory there. Empty means the root itself.
func CheckCwd(root, cwd string) error {
	if err := validCwd(cwd); err != nil {
		return err
	}
	dir := filepath.Join(root, cwd)
	fi, err := os.Stat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("cwd %q does not exist in %s", cwd, root)
	case err != nil:
		return fmt.Errorf("cwd %q: %w", cwd, err)
	case !fi.IsDir():
		return fmt.Errorf("cwd %q is a file, not a directory", cwd)
	}
	return nil
}

// fieldProblem is a problem with one field of a process ("" = the entry).
type fieldProblem struct {
	field, msg string
}

// checkProcess returns the problems of p that can be seen without context:
// everything except uniqueness, which depends on the other processes.
func checkProcess(p Process) []fieldProblem {
	var out []fieldProblem
	add := func(field string, err error) {
		if err != nil {
			out = append(out, fieldProblem{field, err.Error()})
		}
	}
	add("", ValidName(p.Name))
	if strings.TrimSpace(p.Cmd) == "" {
		out = append(out, fieldProblem{"cmd", "cmd is required"})
	}
	add("cwd", validCwd(p.Cwd))
	seen := make(map[string]bool, len(p.Env))
	for _, e := range p.Env {
		add("env", ValidEnvKey(e.Key))
		if seen[e.Key] {
			out = append(out, fieldProblem{"env", fmt.Sprintf("env key %q is set twice", e.Key)})
		}
		seen[e.Key] = true
	}
	if p.Docker != "" {
		add("docker", ValidDockerProject(p.Docker))
	}
	return out
}

// Validate checks c as built in code — the same rules Parse applies, without
// line numbers. It returns a *ValidationError or nil.
func (c *Config) Validate() error {
	verr := &ValidationError{Path: c.Path}
	if err := ValidLogDir(c.LogDir); err != nil {
		verr.add(0, "", err.Error())
	}
	if c.LogMax <= 0 {
		verr.add(0, "", "log_max must be greater than 0")
	}
	seen := make(map[string]bool, len(c.Processes))
	for _, p := range c.Processes {
		for _, fp := range checkProcess(p) {
			verr.add(0, p.Name, fp.msg)
		}
		if seen[p.Name] {
			verr.add(0, p.Name, "defined more than once")
		}
		seen[p.Name] = true
	}
	return verr.orNil()
}
