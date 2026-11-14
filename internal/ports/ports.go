// Package ports detects listening TCP sockets and maps them to the processes
// that hold them.
//
// One [Scan] reads the whole process table and the kernel's socket tables
// once; the resulting [Snapshot] then answers questions for any number of
// process trees without touching the system again. A Snapshot is immutable
// and safe for concurrent use.
//
// On Linux the data comes from /proc. Other systems return
// [ErrUnsupported] from Scan for now (macOS via lsof is planned behind the
// same API).
package ports

import (
	"errors"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// ErrUnsupported is returned by [Scan] on systems without an implementation.
var ErrUnsupported = errors.New("ports: listener detection is not supported on this OS")

// Listener is one listening TCP socket as seen from one process holding it.
type Listener struct {
	// Port is the local TCP port.
	Port int `json:"port"`
	// Addr is the local address in canonical text form: "0.0.0.0",
	// "127.0.0.1", "::", "::1", … IPv4-mapped IPv6 addresses are rendered as
	// their IPv4 address.
	Addr string `json:"addr"`
	// Inode is the socket inode; several Listeners share one when a socket is
	// inherited across fork.
	Inode uint64 `json:"inode"`
	// PID is the holding process, or 0 when no visible process holds the
	// socket (typically another user's process, whose fds we can't read).
	PID int `json:"pid"`
	// Comm is the holder's command name, "" when PID is 0. Names the kernel
	// truncated to 15 bytes are completed from the executable name when it
	// extends them.
	Comm string `json:"comm"`
}

// procInfo is what a Snapshot keeps per process.
type procInfo struct {
	ppid  int
	pgrp  int
	sid   int
	state byte
	comm  string
	start uint64 // clock ticks since boot; 0 if unknown
}

// Snapshot is the result of one scan.
type Snapshot struct {
	procs     map[int]procInfo
	listeners []Listener // sorted and deduplicated
	boot      time.Time  // zero if unknown
}

// Proc returns pid as the snapshot saw it.
func (s *Snapshot) Proc(pid int) (Proc, bool) {
	if s == nil {
		return Proc{}, false
	}
	p, ok := s.procs[pid]
	if !ok {
		return Proc{}, false
	}
	return Proc{PID: pid, PPID: p.ppid, Pgrp: p.pgrp, Session: p.sid, State: p.state, Comm: p.comm, Start: p.start}, true
}

// StartedAt returns when pid started (to 10 ms), if the snapshot knows it.
func (s *Snapshot) StartedAt(pid int) (time.Time, bool) {
	if s == nil || s.boot.IsZero() {
		return time.Time{}, false
	}
	p, ok := s.procs[pid]
	if !ok || p.start == 0 {
		return time.Time{}, false
	}
	return s.boot.Add(time.Duration(p.start) * (time.Second / ticksPerSecond)), true
}

// Listeners returns every listening socket, sorted by port, then address,
// then pid. A socket held by several processes (shared across fork) appears
// once per holder; identical port+addr+pid rows (e.g. SO_REUSEPORT sockets in
// one process) appear once. A socket no visible process holds has PID 0.
//
// The returned slice is a copy.
func (s *Snapshot) Listeners() []Listener {
	if s == nil {
		return nil
	}
	return slices.Clone(s.listeners)
}

// Alive reports whether pid exists in the snapshot and is not a zombie.
func (s *Snapshot) Alive(pid int) bool {
	if s == nil {
		return false
	}
	p, ok := s.procs[pid]
	return ok && p.state != 'Z' && p.state != 'X' && p.state != 'x'
}

// Comm returns the command name of pid as the kernel reports it (at most 15
// bytes), or "" if pid is not in the snapshot.
func (s *Snapshot) Comm(pid int) string {
	if s == nil {
		return ""
	}
	return s.procs[pid].comm
}

// Tree returns the process tree of rootPID, sorted: rootPID itself (if it
// exists), every process whose session id is rootPID, and all
// ppid-descendants of any of those. Following ppid from the session members
// catches children that called setsid. A rootPID <= 0 yields nil.
func (s *Snapshot) Tree(rootPID int) []int {
	if s == nil || rootPID <= 0 {
		return nil
	}
	children := make(map[int][]int, len(s.procs))
	var queue []int
	for pid, p := range s.procs {
		children[p.ppid] = append(children[p.ppid], pid)
		if p.sid == rootPID || pid == rootPID {
			queue = append(queue, pid)
		}
	}
	seen := make(map[int]bool, len(queue))
	for _, pid := range queue {
		seen[pid] = true
	}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, c := range children[pid] {
			if !seen[c] {
				seen[c] = true
				queue = append(queue, c)
			}
		}
	}
	out := make([]int, 0, len(seen))
	for pid := range seen {
		out = append(out, pid)
	}
	slices.Sort(out)
	return out
}

// PortsOf returns the listeners held by the tree of rootPID (see
// [Snapshot.Tree]), one per port+addr, sorted by port then addr. When several
// processes of the tree hold the same port+addr, the lowest pid is reported.
func (s *Snapshot) PortsOf(rootPID int) []Listener {
	tree := s.Tree(rootPID)
	if len(tree) == 0 {
		return nil
	}
	in := make(map[int]bool, len(tree))
	for _, pid := range tree {
		in[pid] = true
	}
	var out []Listener
	for _, l := range s.listeners { // sorted by port, addr, pid
		if !in[l.PID] {
			continue
		}
		if n := len(out); n > 0 && out[n-1].Port == l.Port && out[n-1].Addr == l.Addr {
			continue
		}
		out = append(out, l)
	}
	return out
}

// sortDedupe sorts ls by port, addr, pid, inode and drops rows repeating an
// earlier port+addr+pid.
func sortDedupe(ls []Listener) []Listener {
	slices.SortFunc(ls, func(a, b Listener) int {
		if a.Port != b.Port {
			return a.Port - b.Port
		}
		if c := compareAddr(a.Addr, b.Addr); c != 0 {
			return c
		}
		if a.PID != b.PID {
			return a.PID - b.PID
		}
		switch {
		case a.Inode < b.Inode:
			return -1
		case a.Inode > b.Inode:
			return 1
		}
		return 0
	})
	return slices.CompactFunc(ls, func(a, b Listener) bool {
		return a.Port == b.Port && a.Addr == b.Addr && a.PID == b.PID
	})
}

// compareAddr orders addresses numerically, IPv4 before IPv6; unparsable
// strings sort last, lexically.
func compareAddr(a, b string) int {
	pa, ea := netip.ParseAddr(a)
	pb, eb := netip.ParseAddr(b)
	switch {
	case ea == nil && eb == nil:
		return pa.Compare(pb)
	case ea == nil:
		return -1
	case eb == nil:
		return 1
	}
	return strings.Compare(a, b)
}
