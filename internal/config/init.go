package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrConfigExists is returned by Init when rov.yaml is already there.
var ErrConfigExists = errors.New(FileName + " already exists")

// StarterYAML returns the commented rov.yaml that rov init writes. It has no
// processes, so it is valid as it stands.
func StarterYAML(projectName string) []byte {
	name := strings.Join(strings.Fields(projectName), " ")
	if name == "" {
		name = "this project"
	}
	return []byte(`# rov.yaml — the processes rov runs for ` + name + `.
# Order here is the order in rov status and the TUI. Edit it by hand, in the
# TUI (n new, e edit, J/K move, D delete) or with rov config add|set|mv|rm.

log_dir: .logs                # logs and locks; must be gitignored
log_max: 64MiB                # cap per log file before it rotates

processes:
  # <name>:                   a-z, 0-9, '-', '_'; also the tmux session name
  #   cmd: just dev           required; a shell command run in the pane
  #   cwd: apps/web           optional; relative to this file
  #   env: { PORT: "3000" }   optional; extra environment
  #   description: web app    optional; shown in rov list, rov show, the TUI
  #   docker: my-compose      optional; compose project this process owns
  #
  # A process with only a command fits on one line:
  #   web: { cmd: npm run dev }
`)
}

// Init writes the starter rov.yaml into root and adds the log directory to
// root/.gitignore unless it is already ignored there. It refuses (with
// ErrConfigExists) when rov.yaml exists, and then writes nothing. It returns
// the absolute paths of the files it created or changed.
func Init(root string) (created []string, err error) {
	path := filepath.Join(root, FileName)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("%s: %w; edit it, or use rov config", path, ErrConfigExists)
	}
	if err != nil {
		return nil, err
	}
	if _, err = f.Write(StarterYAML(filepath.Base(root))); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	created = append(created, path)

	changed, err := ensureIgnored(filepath.Join(root, ".gitignore"), DefaultLogDir)
	if err != nil {
		return created, err
	}
	if changed != "" {
		created = append(created, changed)
	}
	return created, nil
}

// ensureIgnored appends "/<dir>/" to the .gitignore at path unless a line
// already ignores dir. It returns path when it wrote the file.
func ensureIgnored(path, dir string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if ignores(data, dir) {
		return "", nil
	}
	var add bytes.Buffer
	if len(data) > 0 && data[len(data)-1] != '\n' {
		add.WriteByte('\n')
	}
	add.WriteString("/" + strings.Trim(filepath.ToSlash(dir), "/") + "/\n")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return "", err
	}
	_, err = f.Write(add.Bytes())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("update %s: %w", path, err)
	}
	return path, nil
}

// ignores reports whether a .gitignore line ignores dir at the root: "dir",
// "dir/", "/dir", "/dir/", "**/dir", optionally followed by "*" or "**".
func ignores(gitignore []byte, dir string) bool {
	want := strings.Trim(filepath.ToSlash(dir), "/")
	sc := bufio.NewScanner(bytes.NewReader(gitignore))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == '!' {
			continue
		}
		line = strings.TrimPrefix(line, "**/")
		line = strings.TrimSuffix(strings.TrimSuffix(line, "**"), "*")
		if strings.Trim(line, "/") == want {
			return true
		}
	}
	return false
}
