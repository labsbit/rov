package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// SocketPath returns the path of the server socket, computed the way tmux
// does: $TMUX_TMPDIR (or /tmp), then tmux-<uid>/<socket name>.
func (c *Client) SocketPath() string {
	dir := os.Getenv("TMUX_TMPDIR")
	if dir == "" {
		dir = "/tmp"
	}
	name := c.Socket
	if name == "" {
		name = "default"
	}
	return filepath.Join(dir, fmt.Sprintf("tmux-%d", os.Getuid()), name)
}

// insideServer reports whether the caller runs in a client of this very
// server ($TMUX is "<socket path>,<pid>,<session>").
func (c *Client) insideServer() bool {
	t := os.Getenv("TMUX")
	if t == "" {
		return false
	}
	path, _, _ := strings.Cut(t, ",")
	return filepath.Clean(path) == filepath.Clean(c.SocketPath())
}

// AttachArgs returns the tmux arguments (without the binary) that attach the
// terminal to session name. Inside a client of the same server it switches
// that client instead. Inside another tmux server it attaches as a nested
// client (TMUX is dropped from the environment by AttachCommand); detach
// with the inner prefix, e.g. ctrl-b ctrl-b d.
func (c *Client) AttachArgs(name string) []string {
	var args []string
	if c.Socket != "" {
		args = append(args, "-L", c.Socket)
	}
	if c.insideServer() {
		return append(args, "switch-client", "-t", "="+name)
	}
	return append(args, "attach-session", "-t", "="+name)
}

// AttachCommand returns a command that attaches the caller's terminal to
// session name and returns on detach. Stdin, stdout and stderr are the
// caller's; the TUI runs it with tea.ExecProcess.
func (c *Client) AttachCommand(name string) *exec.Cmd {
	cmd := exec.Command(c.bin(), c.AttachArgs(name)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = c.attachEnv()
	return cmd
}

func (c *Client) attachEnv() []string {
	if c.insideServer() {
		return os.Environ()
	}
	return Environ()
}

// ExecAttach replaces the current process with tmux attached to session name
// (the CLI's rov attach). It only returns on failure.
func (c *Client) ExecAttach(name string) error {
	bin, err := exec.LookPath(c.bin())
	if err != nil {
		return ErrNotInstalled
	}
	argv := append([]string{bin}, c.AttachArgs(name)...)
	return syscall.Exec(bin, argv, c.attachEnv())
}
