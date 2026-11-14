package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/labsbit/rov/internal/ports"
	"github.com/labsbit/rov/internal/tmux"
)

// InsideTreeError is returned, before anything is signalled, by an action
// that would stop a process tree rov itself runs in: `rov restart web` typed
// into web's pane, or by an agent running there. The process-group kill
// would end this rov halfway through. The CLI re-runs the action from a
// detached rov instead (see cli's detach); the TUI shows the error.
type InsideTreeError struct {
	Name string
	// PID is the member of the tree that is rov or one of its ancestors.
	PID  int
	Comm string
}

func (e *InsideTreeError) Error() string {
	return fmt.Sprintf("rov runs inside the process tree of %s (pid %d, %s, is rov or its ancestor): stopping it would kill this rov midway; run the command from outside %s", e.Name, e.PID, e.Comm, e.Name)
}

// StopError says that processes of a tree survived SIGKILL: another user's
// (permission denied), or stuck in the kernel.
type StopError struct {
	Name      string
	Survivors []Survivor
}

// Survivor is a process stopTree could not stop.
type Survivor struct {
	PID  int
	Comm string
	// Denied is set when kill(2) failed with EPERM.
	Denied bool
}

func (e *StopError) Error() string {
	var parts []string
	for _, s := range e.Survivors {
		why := "still alive after SIGKILL"
		if s.Denied {
			why = "permission denied"
		}
		parts = append(parts, fmt.Sprintf("pid %d (%s, %s)", s.PID, s.Comm, why))
	}
	return "could not stop " + strings.Join(parts, ", ")
}

// stopTree stops the process tree of a session's pane and returns how many
// processes it signalled (0: nothing was alive).
//
// Killing the pane only kills the pane's own process. `just gateway` is
// really just → go run → a compiled binary, and that binary holds the port,
// so a naive respawn would leave the real server orphaned and the
// replacement would die with "address already in use". tmux makes each pane
// a session leader, so the pane pid is also the process group id and a
// negative signal reaches the whole group. Children that called setsid (or
// setpgid) left that group; they are found through the tree (session id plus
// ppid descendants, see ports.Snapshot.Tree) and signalled individually,
// together with any group they lead.
//
// A setsid'd child whose parent has exited is no longer in the tree (its
// parent is init now). It is still stopped when it listens on one of the
// process's last ports (@rov_last_ports), started at or after the process
// did (@rov_started), and belongs to no other session of the server: an
// "orphan" (see orphans). Status warns about such a holder too.
//
// Everything gets SIGTERM, then SIGKILL after StopTimeout. A dead pane may
// still have such leftovers; they are stopped the same way. Before the
// signals the tree's ports are stored as @rov_last_ports.
//
// Pids are reused: a process is only signalled while it is still the one
// the scan saw (same start time, /proc/<pid>/stat field 22), and a group
// only while one of the members the scan saw is. If anything survives
// SIGKILL the result is a *StopError. If rov itself (or an ancestor) is in
// the tree, nothing is signalled and the result is an *InsideTreeError.
func (e *Engine) stopTree(ctx context.Context, s *tmux.Session) (int, error) {
	snap, _ := ports.Scan()
	var pids []int
	switch {
	case snap != nil:
		pids = sessionTree(snap, s)
		pids = append(pids, e.orphans(ctx, snap, s, pids)...)
	case !s.Dead && s.PanePID > 1:
		pids = []int{s.PanePID}
	}
	pids = slices.DeleteFunc(pids, func(pid int) bool { return pid <= 1 })
	if len(pids) == 0 {
		return 0, nil
	}
	if self, comm := selfInTree(snap, pids); self > 0 {
		return 0, &InsideTreeError{Name: s.Name, PID: self, Comm: comm}
	}
	if snap != nil {
		if ps := uniquePorts(portsOf(snap.Listeners(), pidSet(pids))); len(ps) > 0 {
			_, _ = e.Tmux.Run(ctx, tmux.SetOptionCmd(s.PaneID, OptLastPorts, encodePorts(ps)))
		}
	}

	// What the scan saw of each pid: signals go only to the same process.
	seen := map[int]ports.Proc{}
	for _, pid := range pids {
		if p, ok := snap.Proc(pid); ok {
			seen[pid] = p
		}
	}
	same := func(pid int) bool {
		want, known := seen[pid]
		if !known {
			return procAlive(pid) // no snapshot (not Linux): trust the pid
		}
		return stillSame(want)
	}
	// The pane's group, and every group a tree member leads. Its id stays
	// reserved while any member lives, so it can't name someone else's
	// group while a member we saw is still the same process.
	groups := map[int][]int{}
	if s.PanePID > 1 {
		groups[s.PanePID] = nil
	}
	for _, pid := range pids {
		pgid := 0
		if p, ok := seen[pid]; ok {
			pgid = p.Pgrp
		} else if g, err := syscall.Getpgid(pid); err == nil {
			pgid = g
		}
		if pgid > 1 && (pgid == pid || pgid == s.PanePID) {
			groups[pgid] = append(groups[pgid], pid)
		}
	}
	groupLive := func(g int) bool {
		if len(seen) == 0 {
			return groupAlive(g)
		}
		for _, m := range groups[g] {
			if same(m) {
				return true
			}
		}
		return false
	}
	denied := map[int]bool{}
	kill := e.kill
	if kill == nil {
		kill = syscall.Kill
	}
	signal := func(sig syscall.Signal) {
		for g := range groups {
			if groupLive(g) {
				_ = kill(-g, sig)
			}
		}
		for _, pid := range pids {
			if same(pid) {
				if err := kill(pid, sig); errors.Is(err, syscall.EPERM) {
					denied[pid] = true
				}
			}
		}
	}
	alive := func() bool {
		for _, pid := range pids {
			if same(pid) {
				return true
			}
		}
		for g := range groups {
			if groupLive(g) {
				return true
			}
		}
		return false
	}

	signal(syscall.SIGTERM)
	if waitUntil(ctx, e.StopTimeout, func() bool { return !alive() }) {
		return len(pids), nil
	}
	if ctx.Err() != nil {
		return len(pids), ctx.Err()
	}
	signal(syscall.SIGKILL)
	if waitUntil(ctx, 2*time.Second, func() bool { return !alive() }) {
		return len(pids), nil
	}
	if ctx.Err() != nil {
		return len(pids), ctx.Err()
	}
	serr := &StopError{Name: s.Name}
	for _, pid := range pids {
		if same(pid) {
			comm := ""
			if p, ok := ports.ReadProc(pid); ok {
				comm = p.Comm
			}
			serr.Survivors = append(serr.Survivors, Survivor{PID: pid, Comm: comm, Denied: denied[pid]})
		}
	}
	if len(serr.Survivors) == 0 {
		return len(pids), nil // only zombies of a group remain
	}
	return len(pids), serr
}

// stillSame reports whether the process the scan saw as want is still
// alive under its pid: not a zombie, and the same start time (a reused pid
// has a later one).
func stillSame(want ports.Proc) bool {
	now, ok := ports.ReadProc(want.PID)
	return ok && !now.Zombie() && now.Start == want.Start
}

// selfInTree returns the first of rov and its ancestors (by ppid, through
// the snapshot, else /proc) that is in pids, with its comm; 0 if none.
func selfInTree(snap *ports.Snapshot, pids []int) (int, string) {
	in := pidSet(pids)
	pid := os.Getpid()
	for range 64 { // a ppid chain is short; this bounds a malformed one
		if pid <= 1 {
			return 0, ""
		}
		p, ok := snap.Proc(pid)
		if !ok {
			if p, ok = ports.ReadProc(pid); !ok {
				return 0, ""
			}
		}
		if in[pid] {
			return pid, p.Comm
		}
		pid = p.PPID
	}
	return 0, ""
}

// orphans returns the pids that stopTree stops besides the tree: processes
// listening on a last port of s (@rov_last_ports) that started at or after
// s's process did (@rov_started), are neither in tree nor in the tree of any
// other session, and aren't rov. They are what is left of a setsid'd server
// whose parent exited (see stopTree). Without a known start time nothing
// qualifies.
func (e *Engine) orphans(ctx context.Context, snap *ports.Snapshot, s *tmux.Session, tree []int) []int {
	last := decodePorts(s.Option(OptLastPorts))
	started := startedAt(*s)
	if len(last) == 0 || started.IsZero() || s.Option(OptStarted) == "" {
		return nil
	}
	cands := orphanCandidates(snap, last, started, pidSet(tree))
	if len(cands) == 0 {
		return nil
	}
	others := map[int]bool{os.Getpid(): true}
	sessions, err := e.Sessions(ctx)
	if err != nil {
		return nil // can't tell whose they are: leave them
	}
	for i := range sessions {
		if sessions[i].Name == s.Name {
			continue
		}
		for _, pid := range sessionTree(snap, &sessions[i]) {
			others[pid] = true
		}
	}
	return slices.DeleteFunc(cands, func(pid int) bool { return others[pid] })
}

// orphanCandidates returns the holders of ports that started at or after
// started and are not in exclude, sorted and unique.
func orphanCandidates(snap *ports.Snapshot, last []int, started time.Time, exclude map[int]bool) []int {
	want := map[int]bool{}
	for _, p := range last {
		want[p] = true
	}
	var out []int
	for _, l := range snap.Listeners() {
		if !want[l.Port] || l.PID <= 1 || exclude[l.PID] || !snap.Alive(l.PID) {
			continue
		}
		if t, ok := snap.StartedAt(l.PID); ok && !t.Before(started) {
			out = append(out, l.PID)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// waitDead waits until tmux has noticed that the pane of name exited (or the
// session is gone). tmux notices asynchronously; respawning before it does
// loses the remain-on-exit bookkeeping.
func (e *Engine) waitDead(ctx context.Context, name string) {
	waitUntil(ctx, e.StopTimeout, func() bool {
		s, ok, err := e.Tmux.Session(ctx, name)
		return err != nil || !ok || s.Dead
	})
}

// waitUntil polls cond (quickly at first, then every 100 ms) until it is true
// or timeout passes, and reports whether it became true.
func waitUntil(ctx context.Context, timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	wait := 10 * time.Millisecond
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return false
		case <-t.C:
		}
		wait = min(wait*2, 100*time.Millisecond)
	}
}

// procAlive reports whether pid exists and isn't a zombie. A zombie is dead
// for our purposes: its parent (or init) reaps it.
func procAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return !errors.Is(err, os.ErrNotExist) || !procfs()
	}
	// The state follows the last ')' (the command name may hold one).
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 || i+2 >= len(b) {
		return true
	}
	return b[i+2] != 'Z' && b[i+2] != 'X'
}

// groupAlive reports whether process group g has a member that isn't a
// zombie.
func groupAlive(g int) bool {
	if err := syscall.Kill(-g, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	if !procfs() {
		return true
	}
	// kill(-g, 0) also counts zombies; look for a live member.
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true
	}
	for _, ent := range entries {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		if pgid, err := syscall.Getpgid(pid); err == nil && pgid == g && procAlive(pid) {
			return true
		}
	}
	return false
}

func procfs() bool {
	_, err := os.Stat("/proc/self/stat")
	return err == nil
}
