// Package tmux is a thin wrapper around the tmux CLI, bound to one server
// socket (tmux -L <name>).
//
// rov has no daemon: the tmux server is the supervisor. Each process lives in
// a session of the same name with one pane, and rov keeps its own state in
// session user options (@rov_*). This package knows nothing about rov's
// semantics; package core does.
//
// Every call runs the tmux binary with a timeout and without $TMUX, so rov can
// be called from inside a pane it manages. Several tmux commands can be
// chained into one exec with [Client.Run], which keeps a TUI poll cheap.
//
// # Quirks handled here
//
//   - tmux treats an argument ending in ';' as a command separator; every
//     argument is escaped (see [EscapeArg]) so values pass through verbatim.
//   - Format output vis-encodes control characters and escapes '$' before a
//     name, so free-text option values are stored percent-encoded (see
//     [EncodeValue]); [Client.Sessions] decodes them.
//   - tmux expands formats (#S, #{…}) in the start directory (-c) of
//     new-session and respawn-pane and in the pipe-pane command, but not in
//     the pane's shell command or -e values; the constructors here escape
//     '#' where it is expanded (see [EscapeFormat]).
//   - A tmux linked with libutempter loses SIGCHLDs: while it runs the
//     utempter helper for a pane that opened or closed, SIGCHLD is SIG_DFL,
//     so a pane process exiting in that window stays an unreaped zombie and
//     tmux shows the pane dead with no status. [Client.Sessions] makes the
//     server reap it (see reapZombies).
//   - "no server running" is not an error for listing: it is an empty list.
//   - A cancelled context is reported as context.Canceled, not as tmux timing
//     out.
package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// DefaultTimeout bounds one tmux invocation.
const DefaultTimeout = 5 * time.Second

// Minimum supported version: -e on new-session and respawn-pane needs 3.2.
const (
	minMajor = 3
	minMinor = 2
)

var (
	// ErrNoServer is returned when the server isn't running.
	ErrNoServer = errors.New("tmux: no server running")
	// ErrNoSession is returned when the target session or pane doesn't exist.
	ErrNoSession = errors.New("tmux: no such session")
	// ErrNotInstalled is returned when the tmux binary can't be found.
	ErrNotInstalled = errors.New("tmux is not installed (rov needs tmux >= 3.2)")
)

// Client runs tmux commands against one server. The zero value talks to the
// default server with the default binary. A Client is safe for concurrent
// use.
type Client struct {
	// Socket is the server's socket name (tmux -L); "" is the default server.
	Socket string
	// Bin is the tmux executable; "" means "tmux" on PATH.
	Bin string
	// Timeout bounds each invocation; 0 means DefaultTimeout.
	Timeout time.Duration

	verMu   sync.Mutex
	verDone bool
	verErr  error
}

// New returns a Client for the server with the given socket name.
func New(socket string) *Client { return &Client{Socket: socket} }

func (c *Client) bin() string {
	if c.Bin == "" {
		return "tmux"
	}
	return c.Bin
}

func (c *Client) timeout() time.Duration {
	if c.Timeout <= 0 {
		return DefaultTimeout
	}
	return c.Timeout
}

// Error is a failed tmux command with its message.
type Error struct {
	Args []string
	Msg  string // tmux's stderr, trimmed
	Err  error  // ErrNoServer, ErrNoSession or the exec error
}

func (e *Error) Error() string {
	if e.Msg != "" {
		return "tmux: " + e.Msg
	}
	return "tmux: " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// Cmd is one tmux command and its arguments, e.g. {"kill-session", "-t", "=x"}.
type Cmd []string

// Run executes cmds chained in one tmux invocation (separated by ";") and
// returns tmux's standard output. Arguments are escaped, so they reach tmux
// verbatim. Running no commands is a no-op.
func (c *Client) Run(ctx context.Context, cmds ...Cmd) (string, error) {
	if len(cmds) == 0 {
		return "", nil
	}
	if err := c.CheckVersion(ctx); err != nil {
		return "", err
	}
	return c.exec(ctx, chain(cmds))
}

// chain joins cmds with ";" arguments, escaping every argument.
func chain(cmds []Cmd) []string {
	var args []string
	for i, cmd := range cmds {
		if i > 0 {
			args = append(args, ";")
		}
		for j, a := range cmd {
			if j == 0 {
				args = append(args, a) // the command name
				continue
			}
			args = append(args, EscapeArg(a))
		}
	}
	return args
}

// exec runs tmux with args (already escaped) and the socket flag.
func (c *Client) exec(parent context.Context, args []string) (string, error) {
	if err := parent.Err(); err != nil {
		return "", &Error{Args: args, Err: err}
	}
	ctx, cancel := context.WithTimeoutCause(parent, c.timeout(), errTimeout)
	defer cancel()
	full := args
	if c.Socket != "" {
		full = append([]string{"-L", c.Socket}, args...)
	}
	cmd := exec.CommandContext(ctx, c.bin(), full...)
	cmd.Env = Environ()
	cmd.Dir = "/"
	// In a process group of its own, so a Ctrl-C at the terminal reaches
	// rov (which cancels ctx: context.Canceled) rather than killing tmux
	// mid-command (an opaque "signal: interrupt").
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if err == nil {
		return stdout.String(), nil
	}
	msg := strings.TrimSpace(stderr.String())
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return "", ErrNotInstalled
	case parent.Err() != nil:
		// The caller gave up (Ctrl-C, its own deadline): not tmux's fault.
		return "", &Error{Args: args, Err: parent.Err()}
	case errors.Is(context.Cause(ctx), errTimeout):
		return "", &Error{Args: args, Err: fmt.Errorf("%s timed out after %s", strings.Join(firstWords(args), " "), c.timeout())}
	case isNoServer(msg):
		return "", &Error{Args: args, Msg: msg, Err: ErrNoServer}
	case isNoSession(msg):
		return "", &Error{Args: args, Msg: msg, Err: ErrNoSession}
	}
	if msg == "" {
		return "", &Error{Args: args, Err: err}
	}
	return "", &Error{Args: args, Msg: msg, Err: err}
}

// errTimeout is the cause of a context that ran out because of the
// Client's own timeout.
var errTimeout = errors.New("tmux timeout")

func firstWords(args []string) []string {
	if len(args) > 2 {
		return args[:2]
	}
	return args
}

// Environ returns the environment tmux runs with: the caller's, without
// TMUX and TMUX_PANE, so rov works from inside a pane (tmux refuses nested
// clients otherwise) and a new server isn't tied to the caller's.
func Environ() []string {
	env := os.Environ()
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "TMUX=") || strings.HasPrefix(kv, "TMUX_PANE=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func isNoServer(msg string) bool {
	return strings.Contains(msg, "no server running") ||
		strings.Contains(msg, "error connecting to") ||
		strings.Contains(msg, "server exited unexpectedly")
}

func isNoSession(msg string) bool {
	return strings.Contains(msg, "can't find session") ||
		strings.Contains(msg, "no such session") ||
		strings.Contains(msg, "can't find pane") ||
		strings.Contains(msg, "can't find window")
}

// CheckVersion verifies once per Client that tmux is installed and at least
// 3.2. Later calls return the first answer, unless that answer was the
// caller's context ending: then the next call asks again.
func (c *Client) CheckVersion(ctx context.Context) error {
	c.verMu.Lock()
	defer c.verMu.Unlock()
	if c.verDone {
		return c.verErr
	}
	out, err := c.exec(ctx, []string{"-V"})
	switch {
	case err == nil:
		c.verErr = checkVersion(strings.TrimSpace(out))
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return err // not an answer: don't keep it
	case errors.Is(err, ErrNotInstalled):
		c.verErr = err
	default:
		c.verErr = fmt.Errorf("can't run tmux -V: %w", err)
	}
	c.verDone = true
	return c.verErr
}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)`)

// checkVersion accepts "tmux 3.4", "tmux 3.3a", "tmux next-3.5"; versions it
// can't parse ("tmux master", "tmux openbsd-7.4") are assumed new enough.
func checkVersion(v string) error {
	m := versionRe.FindStringSubmatch(v)
	if m == nil || strings.Contains(v, "openbsd") {
		return nil
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major > minMajor || major == minMajor && minor >= minMinor {
		return nil
	}
	return fmt.Errorf("%s is too old: rov needs tmux >= %d.%d (for -e on new-session/respawn-pane)", v, minMajor, minMinor)
}

// EscapeFormat escapes '#' as "##", for arguments tmux expands as formats:
// a directory or command containing "#S" or "#{session_name}" then reaches
// tmux's consumer verbatim.
func EscapeFormat(s string) string { return strings.ReplaceAll(s, "#", "##") }

// EscapeArg protects an argument from tmux's command parser, which treats a
// trailing ';' as a command separator and "\;" as a literal ';'.
func EscapeArg(s string) string {
	if strings.HasSuffix(s, ";") {
		return s[:len(s)-1] + `\;`
	}
	return s
}
