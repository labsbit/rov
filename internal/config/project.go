package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Project is the directory tree rov manages: the root, its config and where
// its logs, locks and tmux server live.
type Project struct {
	// Root is the absolute, symlink-free project root.
	Root string
	// Name is the base name of Root.
	Name string
	// Config is never nil: without rov.yaml it is Default(ConfigPath()).
	Config *Config
	// HasConfig reports whether rov.yaml exists at Root.
	HasConfig bool
}

// FindRoot resolves the project root for dir: the nearest ancestor (or dir
// itself) holding rov.yaml, else the nearest one holding .git (a directory, or
// a file in worktrees and submodules). The root is absolute with symlinks
// resolved, so every path to a project names the same tmux server.
func FindRoot(dir string) (root string, hasConfig bool, err error) {
	start, err := filepath.Abs(dir)
	if err == nil {
		start, err = filepath.EvalSymlinks(start)
	}
	if err != nil {
		return "", false, fmt.Errorf("project directory %s: %w", dir, err)
	}
	if fi, err := os.Stat(start); err != nil || !fi.IsDir() {
		return "", false, fmt.Errorf("project directory %s is not a directory", dir)
	}
	if r, ok := walkUp(start, func(d string) bool { return isFile(filepath.Join(d, FileName)) }); ok {
		return r, true, nil
	}
	if r, ok := walkUp(start, func(d string) bool { return exists(filepath.Join(d, ".git")) }); ok {
		return r, false, nil
	}
	return "", false, fmt.Errorf("no %s in %s or any parent, and it is not inside a git repository (create one with rov init)", FileName, start)
}

// FindProject resolves the project for dir (see FindRoot) and loads its
// config. A config with problems returns the error from Load; use FindRoot
// to get at the project anyway, e.g. to fix the file.
func FindProject(dir string) (*Project, error) {
	root, has, err := FindRoot(dir)
	if err != nil {
		return nil, err
	}
	p := &Project{Root: root, Name: filepath.Base(root), HasConfig: has}
	if !has {
		p.Config = Default(p.ConfigPath())
		return p, nil
	}
	cfg, err := Load(p.ConfigPath())
	if err != nil {
		return nil, err
	}
	p.Config = cfg
	return p, nil
}

// ConfigPath returns the absolute path of rov.yaml, whether or not it exists.
func (p *Project) ConfigPath() string { return filepath.Join(p.Root, FileName) }

// LogDir returns the absolute log directory.
func (p *Project) LogDir() string {
	dir := DefaultLogDir
	if p.Config != nil && p.Config.LogDir != "" {
		dir = p.Config.LogDir
	}
	if filepath.IsAbs(dir) {
		return filepath.Clean(dir)
	}
	return filepath.Join(p.Root, dir)
}

// LockDir returns the absolute directory of the per-process and config locks.
func (p *Project) LockDir() string { return filepath.Join(p.LogDir(), ".lock") }

// SocketName returns the name of the project's tmux server socket (for
// tmux -L): "rov-<basename>-<hash8>", where hash8 is the first 8 hex digits
// of the SHA-256 of Root. The base name is reduced to [a-z0-9_-] and kept
// short, because tmux socket paths are limited in length.
func (p *Project) SocketName() string {
	sum := sha256.Sum256([]byte(p.Root))
	return "rov-" + sanitize(filepath.Base(p.Root)) + "-" + hex.EncodeToString(sum[:])[:8]
}

func sanitize(base string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(base) {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-'
		if !ok {
			r = '-'
		}
		if r == '-' && dash {
			continue
		}
		dash = r == '-'
		b.WriteRune(r)
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-")
	}
	if s == "" {
		s = "root"
	}
	return s
}

func walkUp(dir string, match func(string) bool) (string, bool) {
	for {
		if match(dir) {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func isFile(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.Mode().IsRegular()
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, os.ErrNotExist)
}
