package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Logs runs `docker logs --tail N [-f] <container>` and returns its output,
// the container's stdout and stderr merged in one stream in the order docker
// writes them. A negative tail means all lines.
//
// Without follow the stream ends when docker has printed everything, and
// docker is stopped if it hasn't within the client's ComposeTimeout (Close
// then reports it); with follow it lasts until ctx is done or the reader is
// closed. Close stops docker and returns its failure, if any (e.g. "No such
// container", whose message also appears in the stream).
func (c *Client) Logs(ctx context.Context, container string, tail int, follow bool) (io.ReadCloser, error) {
	return c.LogsSince(ctx, container, tail, time.Time{}, follow)
}

// LogsSince is Logs from a point in time on (`--since`); the zero time is
// the start of the log. The TUI uses it to pick up a container's log again
// after its stream ended, without repeating lines.
func (c *Client) LogsSince(ctx context.Context, container string, tail int, since time.Time, follow bool) (io.ReadCloser, error) {
	if err := c.cachedUnavailable(); err != nil {
		return nil, err
	}
	bin, err := exec.LookPath(c.bin())
	if err != nil {
		err = fmt.Errorf("%w: %v", ErrUnavailable, err)
		c.remember(false, err)
		return nil, err
	}
	n := "all"
	if tail >= 0 {
		n = strconv.Itoa(tail)
	}
	args := []string{"logs", "--tail", n}
	if !since.IsZero() {
		args = append(args, "--since", since.UTC().Format(time.RFC3339Nano))
	}
	if follow {
		args = append(args, "--follow")
	}
	args = append(args, "--", container)

	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	var cancel context.CancelFunc
	timeout := c.composeTimeout()
	if follow {
		ctx, cancel = context.WithCancel(ctx)
	} else {
		ctx, cancel = context.WithTimeoutCause(ctx, timeout, errTimeout)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = composeEnv(os.Environ())
	// One pipe for both: docker writes each chunk to the matching fd, and the
	// kernel keeps their order.
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		cancel()
		pr.Close()
		pw.Close()
		return nil, fmt.Errorf("docker logs: %w", err)
	}
	pw.Close() // the child holds its copy; EOF arrives when it exits
	return &logStream{r: pr, cmd: cmd, ctx: ctx, cancel: cancel, timeout: timeout}, nil
}

// logStream is the reader Logs returns.
type logStream struct {
	r       *os.File
	cmd     *exec.Cmd
	ctx     context.Context
	cancel  context.CancelFunc
	timeout time.Duration // of a stream without follow
	eof     atomic.Bool   // docker closed its end: it has exited or is exiting
	once    sync.Once
	err     error
}

func (l *logStream) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	if err == io.EOF {
		l.eof.Store(true)
	}
	return n, err
}

// Close stops docker if it's still running, reaps it and returns its error.
// A docker that ran to the end reports its exit status (non-zero is an
// error); one stopped early — by Close, by ctx, or by SIGPIPE after the
// reader went away — is not an error.
func (l *logStream) Close() error {
	l.once.Do(func() {
		defer l.cancel()
		finished := l.eof.Load()
		l.r.Close()
		if !finished {
			l.cancel()
		}
		err := l.cmd.Wait()
		if !finished {
			var ee *exec.ExitError
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
				errors.As(err, &ee) && !ee.Exited() {
				err = nil
			}
		}
		switch {
		case ownTimeout(l.ctx):
			l.err = fmt.Errorf("docker logs: %w: not done after %s", context.DeadlineExceeded, l.timeout)
		case err != nil:
			l.err = fmt.Errorf("docker logs: %w", err)
		}
	})
	return l.err
}
