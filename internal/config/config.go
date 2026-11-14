// Package config reads and validates rov.yaml and resolves the project it
// belongs to.
//
// rov.yaml lists the pre-configured processes of a project. The order of the
// processes map is the display order, so the file is decoded through a
// yaml.Node tree rather than into a Go map. Writing the file is the job of
// package configedit, which edits the node tree to keep comments and layout.
package config

import "path/filepath"

// FileName is the name of the config file at the project root.
const FileName = "rov.yaml"

// Defaults for the top-level settings.
const (
	DefaultLogDir       = ".logs"
	DefaultLogMax int64 = 64 << 20 // 64 MiB
)

// Process is one entry of the processes map.
type Process struct {
	// Name is the map key. It doubles as the tmux session name.
	Name string
	// Cmd is the shell command run in the pane. Required.
	Cmd string
	// Cwd is the working directory as written, relative to the project root.
	// Empty means the root; see Dir.
	Cwd string
	// Env holds extra environment variables in file order.
	Env []EnvVar
	// Description is shown in rov list, rov show and the TUI.
	Description string
	// Docker names the compose project whose containers this process owns.
	Docker string
}

// EnvVar is one KEY=VALUE pair of a process environment.
type EnvVar struct {
	Key, Value string
}

// String formats the pair as KEY=VALUE.
func (e EnvVar) String() string { return e.Key + "=" + e.Value }

// Dir returns the absolute working directory of p for a project rooted at
// root.
func (p Process) Dir(root string) string {
	if p.Cwd == "" {
		return root
	}
	return filepath.Join(root, p.Cwd)
}

// Config is a decoded rov.yaml.
type Config struct {
	// Path is the file the config was read from (or would be written to).
	Path string
	// LogDir is the log directory as written; DefaultLogDir when absent.
	// Relative paths are relative to the project root; see Project.LogDir.
	LogDir string
	// LogMax is the size cap per log file in bytes; DefaultLogMax when absent.
	LogMax int64
	// Processes are the configured processes in file order.
	Processes []Process
}

// Default returns the config used when a file has no settings: no processes
// and default log settings. path is recorded as Config.Path.
func Default(path string) *Config {
	return &Config{Path: path, LogDir: DefaultLogDir, LogMax: DefaultLogMax}
}

// Get returns the process called name.
func (c *Config) Get(name string) (Process, bool) {
	if i := c.Index(name); i >= 0 {
		return c.Processes[i], true
	}
	return Process{}, false
}

// Index returns the position of the process called name, or -1.
func (c *Config) Index(name string) int {
	for i, p := range c.Processes {
		if p.Name == name {
			return i
		}
	}
	return -1
}

// Names returns the process names in file order.
func (c *Config) Names() []string {
	names := make([]string, len(c.Processes))
	for i, p := range c.Processes {
		names[i] = p.Name
	}
	return names
}
