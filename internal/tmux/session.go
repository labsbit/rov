package tmux

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Session is one tmux session and its first pane, as listed by
// [Client.Sessions].
type Session struct {
	Name string
	// PaneID is the pane's id ("%3"). It is stable for the life of the
	// session (respawn-pane reuses the pane) and is the target for every
	// pane operation: the "=" exact-match prefix only works for sessions.
	PaneID string
	// PanePID is the pid of the pane's process, a session leader. For a dead
	// pane it's the pid the process had.
	PanePID int
	// Dead reports that the pane's process has exited (remain-on-exit keeps
	// the pane).
	Dead bool
	// DeadStatus is the exit status of a dead pane, nil when tmux doesn't
	// know it (the process was killed by a signal).
	DeadStatus *int
	// DeadSignal is the signal that killed a dead pane's process, 0 if none
	// or unknown.
	DeadSignal int
	// Created is when the session was created.
	Created time.Time
	// ServerPID is the pid of the tmux server (the parent of every pane
	// process).
	ServerPID int
	// Options holds the user options asked for, decoded (see EncodeValue);
	// an unset option is "".
	Options map[string]string
}

// Option returns the session's value of the user option name ("" if unset).
func (s Session) Option(name string) string { return s.Options[name] }

const (
	fieldSep = "\t"
	// paneFields come before the options in each row.
	paneFields = "#{session_name}\t#{pane_id}\t#{pane_pid}\t#{pane_dead}\t#{pane_dead_status}\t#{pane_dead_signal}\t#{session_created}\t#{pid}"
	nPane      = 8
)

// Sessions lists every session of the server with its first pane and the
// given user options (names like "@rov_cmd"), in one tmux call. A server
// that isn't running has no sessions: that is not an error.
//
// A pane tmux reports dead without an exit status or signal has usually not
// been reaped (see reapZombies): Sessions then makes the server reap it and
// lists again, so callers see the real status.
func (c *Client) Sessions(ctx context.Context, options ...string) ([]Session, error) {
	list, err := c.listSessions(ctx, options)
	if err != nil || unreapedServer(list) == 0 {
		return list, err
	}
	return c.reapZombies(ctx, list, options), nil
}

func (c *Client) listSessions(ctx context.Context, options []string) ([]Session, error) {
	format := paneFields
	for _, o := range options {
		format += fieldSep + "#{" + o + "}"
	}
	out, err := c.Run(ctx, Cmd{"list-panes", "-a", "-F", format})
	if errors.Is(err, ErrNoServer) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parseSessions(out, options), nil
}

// parseSessions parses list-panes rows, keeping the first pane of each
// session. Rows that don't have the expected fields (a session name with a
// tab in it, created by hand) are skipped.
func parseSessions(out string, options []string) []Session {
	var list []Session
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, fieldSep)
		if len(f) != nPane+len(options) || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		s := Session{Name: f[0], PaneID: f[1], Dead: f[3] == "1", Options: make(map[string]string, len(options))}
		s.PanePID, _ = strconv.Atoi(f[2])
		if s.Dead && f[4] != "" {
			if n, err := strconv.Atoi(f[4]); err == nil {
				s.DeadStatus = &n
			}
		}
		if s.Dead {
			s.DeadSignal, _ = strconv.Atoi(f[5])
		}
		if secs, err := strconv.ParseInt(f[6], 10, 64); err == nil && secs > 0 {
			s.Created = time.Unix(secs, 0)
		}
		s.ServerPID, _ = strconv.Atoi(f[7])
		for i, o := range options {
			s.Options[o] = DecodeValue(f[nPane+i])
		}
		list = append(list, s)
	}
	return list
}

// Session returns the named session, or ok=false when it doesn't exist.
func (c *Client) Session(ctx context.Context, name string, options ...string) (s Session, ok bool, err error) {
	list, err := c.Sessions(ctx, options...)
	if err != nil {
		return Session{}, false, err
	}
	for _, s := range list {
		if s.Name == name {
			return s, true, nil
		}
	}
	return Session{}, false, nil
}

// SessionTarget returns the exact-match target for a session: "=name:".
// The trailing colon makes it valid where tmux expects a pane or window.
func SessionTarget(name string) string { return "=" + name + ":" }

// NewSession describes a detached session to create.
type NewSession struct {
	Name string
	Dir  string // working directory of the first pane
	// Width and Height set the detached size; tmux shrinks the window as
	// soon as a smaller client attaches. Detached sessions default to 80x24,
	// which would hard-wrap the output that reaches the log.
	Width, Height int
	Env           []string // KEY=VALUE pairs for the pane (-e)
	Command       string   // shell command for the pane; "" runs the shell
}

// ServerOptions are global options rov sets on its own server every time it
// creates a session (idempotent, and the server may have been restarted).
type ServerOptions struct {
	// RemainOnExit keeps a pane whose process exited, so its exit status and
	// output stay inspectable instead of the session disappearing.
	RemainOnExit bool
	// DefaultShell, if set, is the shell tmux runs pane commands with.
	DefaultShell string
}

// NewSessionCmds returns the commands that create s on a running or new
// server, after setting opts globally. The new pane's id is printed.
func NewSessionCmds(s NewSession, opts ServerOptions) []Cmd {
	cmds := []Cmd{{"start-server"}}
	if opts.RemainOnExit {
		cmds = append(cmds, Cmd{"set-option", "-g", "remain-on-exit", "on"})
	}
	if opts.DefaultShell != "" {
		cmds = append(cmds, Cmd{"set-option", "-g", "default-shell", opts.DefaultShell})
	}
	ns := Cmd{"new-session", "-d", "-s", s.Name, "-P", "-F", "#{pane_id}"}
	if s.Dir != "" {
		ns = append(ns, "-c", EscapeFormat(s.Dir))
	}
	if s.Width > 0 && s.Height > 0 {
		ns = append(ns, "-x", strconv.Itoa(s.Width), "-y", strconv.Itoa(s.Height))
	}
	for _, kv := range s.Env {
		ns = append(ns, "-e", kv)
	}
	if s.Command != "" {
		ns = append(ns, s.Command)
	}
	return append(cmds, ns)
}

// CreateSession creates s (see NewSessionCmds) and returns its pane id.
func (c *Client) CreateSession(ctx context.Context, s NewSession, opts ServerOptions) (paneID string, err error) {
	out, err := c.Run(ctx, NewSessionCmds(s, opts)...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// Respawn describes a respawn-pane -k: the pane's process (if alive) is
// killed and Command starts in the same pane.
type Respawn struct {
	Pane    string // pane id
	Dir     string
	Env     []string // KEY=VALUE pairs (-e)
	Command string
}

// RespawnCmd returns the respawn-pane command for r.
func RespawnCmd(r Respawn) Cmd {
	cmd := Cmd{"respawn-pane", "-k", "-t", r.Pane}
	if r.Dir != "" {
		cmd = append(cmd, "-c", EscapeFormat(r.Dir))
	}
	for _, kv := range r.Env {
		cmd = append(cmd, "-e", kv)
	}
	if r.Command != "" {
		cmd = append(cmd, r.Command)
	}
	return cmd
}

// SetOptionCmd returns the command setting the session user option name of
// the session target belongs to (a pane id or SessionTarget). The value is
// encoded (see EncodeValue); an empty value unsets the option.
func SetOptionCmd(target, name, value string) Cmd {
	if value == "" {
		return Cmd{"set-option", "-q", "-u", "-t", target, name}
	}
	return Cmd{"set-option", "-t", target, "--", name, EncodeValue(value)}
}

// SetOptions sets session user options in one call; see SetOptionCmd.
func (c *Client) SetOptions(ctx context.Context, target string, kv map[string]string) error {
	var cmds []Cmd
	for k, v := range kv {
		cmds = append(cmds, SetOptionCmd(target, k, v))
	}
	_, err := c.Run(ctx, cmds...)
	return err
}

// PipePaneCmd returns the command that pipes the output of pane into the
// shell command; tmux closes any existing pipe first, so its reader gets
// EOF. (Not -o: that toggles, and would switch logging off on a restart.)
// tmux refuses it for a pane whose process has exited, but not right after
// new-session or respawn-pane in the same Run: tmux reads neither output
// nor exits until the whole chain has run, so the pipe gets the first byte.
func PipePaneCmd(pane, command string) Cmd {
	return Cmd{"pipe-pane", "-t", pane, EscapeFormat(command)}
}

// SendText types text into pane literally, then Enter if enter is set.
func (c *Client) SendText(ctx context.Context, pane, text string, enter bool) error {
	cmds := []Cmd{{"send-keys", "-t", pane, "-l", "--", text}}
	if text == "" {
		cmds = nil
	}
	if enter {
		cmds = append(cmds, Cmd{"send-keys", "-t", pane, "Enter"})
	}
	_, err := c.Run(ctx, cmds...)
	return err
}

// KillSession kills the named session. A session or server that is already
// gone is not an error.
func (c *Client) KillSession(ctx context.Context, name string) error {
	_, err := c.Run(ctx, Cmd{"kill-session", "-t", "=" + name})
	if errors.Is(err, ErrNoSession) || errors.Is(err, ErrNoServer) {
		return nil
	}
	return err
}

// KillServer kills the server and every session on it. A server that isn't
// running is not an error.
func (c *Client) KillServer(ctx context.Context) error {
	_, err := c.Run(ctx, Cmd{"kill-server"})
	if errors.Is(err, ErrNoServer) {
		return nil
	}
	return err
}
