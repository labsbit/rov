package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/labsbit/rov/internal/lock"
)

// ErrNotRunning is wrapped by errors for actions that need a running
// process (send, wait); the CLI exits 3 on it.
var ErrNotRunning = errors.New("not running")

// BusyError says another rov held the process lock for longer than the
// engine's LockTimeout.
type BusyError struct {
	Name string
	// Holder describes the other holder, e.g. "rov (pid 4242) restarting
	// gateway", when it could be read.
	Holder string
}

func (e *BusyError) Error() string {
	who := e.Holder
	if who == "" {
		who = "another rov is working on " + e.Name
	}
	return "busy: " + who + "; try again in a moment"
}

// locked runs fn holding the flock of process name (<LockDir>/proc/<name>,
// apart from the rov.yaml lock <LockDir>/config and other files there), so
// rov processes (CLI calls, the TUI) act on one process one at a time. verb
// ("restarting") is written into the lock file for the busy message another
// caller may show.
func (e *Engine) locked(ctx context.Context, name, verb string, fn func() (string, error)) (string, error) {
	path := e.LockPath(name)
	lctx, cancel := context.WithTimeout(ctx, e.LockTimeout)
	defer cancel()
	release, err := lock.Acquire(lctx, path)
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return "", &BusyError{Name: name, Holder: holder(path)}
		}
		return "", err
	}
	defer release()
	// The lock is on the open file, not its content: rewriting it is safe.
	_ = os.WriteFile(path, fmt.Appendf(nil, "rov (pid %d) %s %s\n", os.Getpid(), verb, name), 0o644)
	return fn()
}

func holder(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
