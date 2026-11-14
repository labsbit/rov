package tmux

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"syscall"
	"time"
)

// The SIGCHLD tmux loses (see the package doc): tmux closes a pane's pty on
// EOF and records the exit status only when it reaps the process on SIGCHLD.
// A tmux linked with libutempter runs a helper (fork + waitpid) for every
// pane it opens or closes, with SIGCHLD set to SIG_DFL meanwhile. The pty EOF
// of an exiting process arrives before its SIGCHLD (the kernel closes the
// files first), so tmux is often running that helper — for the very pane that
// just closed — when the SIGCHLD comes; with SIG_DFL the kernel discards it.
// The process stays an unreaped zombie until some later child of the server
// exits, and the pane shows pane_dead=1 with an empty pane_dead_status.
//
// Sending the server a SIGCHLD makes it run its reaper (waitpid(-1) until no
// child is left to reap), which records the status exactly as if the signal
// hadn't been lost. If it still isn't recorded, the zombie's exit status is
// read from /proc/<pid>/stat (field 52).

// unreapedServer returns the server pid if a pane of list is dead with
// neither an exit status nor a signal, else 0.
func unreapedServer(list []Session) int {
	for _, s := range list {
		if s.Dead && s.DeadStatus == nil && s.DeadSignal == 0 && s.ServerPID > 1 {
			return s.ServerPID
		}
	}
	return 0
}

// reapZombies nudges the server to reap its exited children and lists the
// sessions again (up to ~100 ms), then fills in what is still missing from
// the zombies in /proc. On any error it returns what it has.
func (c *Client) reapZombies(ctx context.Context, list []Session, options []string) []Session {
	server := unreapedServer(list)
	if err := syscall.Kill(server, syscall.SIGCHLD); err == nil {
		for _, d := range []time.Duration{2 * time.Millisecond, 10 * time.Millisecond, 30 * time.Millisecond, 60 * time.Millisecond} {
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return fillFromZombies(list)
			case <-t.C:
			}
			again, err := c.listSessions(ctx, options)
			if err != nil {
				break
			}
			list = again
			if unreapedServer(list) == 0 {
				return list
			}
		}
	}
	return fillFromZombies(list)
}

// fillFromZombies sets the status of dead panes without one from their
// zombie process, when it is still there and a child of the server.
func fillFromZombies(list []Session) []Session {
	for i := range list {
		s := &list[i]
		if !s.Dead || s.DeadStatus != nil || s.DeadSignal != 0 || s.PanePID <= 1 {
			continue
		}
		b, err := os.ReadFile("/proc/" + strconv.Itoa(s.PanePID) + "/stat")
		if err != nil {
			continue
		}
		code, sig, ok := zombieStatus(b, s.ServerPID)
		switch {
		case !ok:
		case sig > 0:
			s.DeadSignal = sig
		default:
			s.DeadStatus = &code
		}
	}
	return list
}

// zombieStatus decodes the wait status of a zombie from its /proc/<pid>/stat:
// the exit code, or the signal that killed it. ok is false unless the
// process is a zombie whose parent is ppid (any parent if ppid <= 0).
func zombieStatus(stat []byte, ppid int) (code, sig int, ok bool) {
	end := bytes.LastIndexByte(stat, ')')
	if end < 0 {
		return 0, 0, false
	}
	// Fields after the comm start at field 3 (state); exit_code is field 52.
	f := bytes.Fields(stat[end+1:])
	if len(f) < 50 || string(f[0]) != "Z" {
		return 0, 0, false
	}
	if p, err := strconv.Atoi(string(f[1])); err != nil || ppid > 0 && p != ppid {
		return 0, 0, false
	}
	v, err := strconv.Atoi(string(f[52-3]))
	if err != nil {
		return 0, 0, false
	}
	if s := v & 0x7f; s != 0 {
		return 0, s, true // WIFSIGNALED: WTERMSIG
	}
	return (v >> 8) & 0xff, 0, true // WEXITSTATUS
}
