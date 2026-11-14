package ports

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// commLen is the kernel's TASK_COMM_LEN minus the terminating NUL: a comm of
// exactly this length may have been truncated.
const commLen = 15

// tcpListen is the st value of a listening socket in /proc/net/tcp{,6}.
const tcpListen = "0A"

// ScanAt scans a /proc-like tree rooted at procRoot: <root>/net/tcp,
// <root>/net/tcp6, <root>/<pid>/stat, <root>/<pid>/fd/* and, for listener
// holders with possibly truncated names, <root>/<pid>/cmdline and exe.
//
// Missing tcp/tcp6 tables (e.g. IPv6 disabled), unreadable fd directories
// (other users' processes) and processes that exit mid-scan are normal and
// not errors. Malformed lines are skipped. An error is returned only when the
// process table itself can't be listed.
func ScanAt(procRoot string) (*Snapshot, error) {
	listening := map[uint64]netip.AddrPort{}
	for _, name := range []string{"tcp", "tcp6"} {
		// A missing or unreadable table only means fewer listeners.
		_ = readTCP(filepath.Join(procRoot, "net", name), listening)
	}

	d, err := os.Open(procRoot)
	if err != nil {
		return nil, fmt.Errorf("ports: %w", err)
	}
	names, err := d.Readdirnames(-1)
	d.Close()
	if err != nil {
		return nil, fmt.Errorf("ports: list %s: %w", procRoot, err)
	}

	s := &Snapshot{procs: make(map[int]procInfo, len(names)), boot: readBootTime(procRoot)}
	holders := map[uint64][]int{}
	buf := make([]byte, 0, 1024)
	for _, name := range names {
		pid, err := strconv.Atoi(name)
		if err != nil || pid <= 0 {
			continue
		}
		dir := filepath.Join(procRoot, name)
		buf, err = readInto(filepath.Join(dir, "stat"), buf)
		if err != nil {
			continue // vanished, or not a process
		}
		p, ok := parseStat(buf)
		if !ok {
			continue
		}
		s.procs[pid] = p
		if len(listening) > 0 {
			socketInodes(filepath.Join(dir, "fd"), listening, func(ino uint64) {
				holders[ino] = append(holders[ino], pid)
			})
		}
	}

	comms := map[int]string{}
	commOf := func(pid int) string {
		if c, ok := comms[pid]; ok {
			return c
		}
		c := s.procs[pid].comm
		if len(c) == commLen {
			c = fullComm(filepath.Join(procRoot, strconv.Itoa(pid)), c)
		}
		comms[pid] = c
		return c
	}
	ls := make([]Listener, 0, len(listening))
	for ino, ap := range listening {
		l := Listener{Port: int(ap.Port()), Addr: ap.Addr().String(), Inode: ino}
		pids := holders[ino]
		if len(pids) == 0 {
			ls = append(ls, l)
			continue
		}
		for _, pid := range pids {
			l.PID, l.Comm = pid, commOf(pid)
			ls = append(ls, l)
		}
	}
	s.listeners = sortDedupe(ls)
	return s, nil
}

// readInto reads the whole (small) file at path into buf[:0], growing it as
// needed. /proc files report size 0, so os.ReadFile's size hint is useless.
func readInto(path string, buf []byte) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return buf[:0], err
	}
	defer f.Close()
	buf = buf[:0]
	for {
		if len(buf) == cap(buf) {
			buf = append(buf, 0)[:len(buf)]
		}
		n, err := f.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return buf, err
		}
	}
}

// parseStat parses /proc/<pid>/stat. The comm field is enclosed in
// parentheses and may itself contain spaces and parentheses, so the fixed
// fields are read after the last ')'.
func parseStat(b []byte) (procInfo, bool) {
	open := bytes.IndexByte(b, '(')
	end := bytes.LastIndexByte(b, ')')
	if open < 0 || end < open {
		return procInfo{}, false
	}
	// state ppid pgrp session … starttime (field 22, index 19 here) …
	f := bytes.Fields(b[end+1:])
	if len(f) < 4 || len(f[0]) != 1 {
		return procInfo{}, false
	}
	ppid, err1 := strconv.Atoi(string(f[1]))
	pgrp, err3 := strconv.Atoi(string(f[2]))
	sid, err2 := strconv.Atoi(string(f[3]))
	if err1 != nil || err2 != nil || err3 != nil {
		return procInfo{}, false
	}
	p := procInfo{ppid: ppid, pgrp: pgrp, sid: sid, state: f[0][0], comm: string(b[open+1 : end])}
	if len(f) > 19 {
		p.start, _ = strconv.ParseUint(string(f[19]), 10, 64)
	}
	return p, true
}

// ticksPerSecond is USER_HZ, the unit of starttime in /proc/<pid>/stat: 100
// on every Linux architecture.
const ticksPerSecond = 100

// readBootTime returns the boot time, zero if unknown: now minus
// <procRoot>/uptime (to 10 ms), else <procRoot>/stat's btime, which is
// truncated to whole seconds.
func readBootTime(procRoot string) time.Time {
	if b, err := os.ReadFile(filepath.Join(procRoot, "uptime")); err == nil {
		if f := strings.Fields(string(b)); len(f) > 0 {
			if up, err := strconv.ParseFloat(f[0], 64); err == nil && up > 0 {
				return time.Now().Add(-time.Duration(up * float64(time.Second)))
			}
		}
	}
	b, err := os.ReadFile(filepath.Join(procRoot, "stat"))
	if err != nil {
		return time.Time{}
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "btime "); ok {
			if secs, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil && secs > 0 {
				return time.Unix(secs, 0)
			}
		}
	}
	return time.Time{}
}

// Proc is one process as /proc/<pid>/stat describes it right now.
type Proc struct {
	PID, PPID, Pgrp, Session int
	State                    byte
	Comm                     string
	// Start is the start time in clock ticks since boot (0 if unknown): with
	// the pid it identifies a process, as pids are reused.
	Start uint64
}

// Zombie reports whether the process has exited and awaits its reaper.
func (p Proc) Zombie() bool { return p.State == 'Z' || p.State == 'X' || p.State == 'x' }

// ReadProc reads /proc/<pid>/stat. ok is false if the process doesn't exist
// (or /proc isn't there).
func ReadProc(pid int) (Proc, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return Proc{}, false
	}
	p, ok := parseStat(b)
	if !ok {
		return Proc{}, false
	}
	return Proc{PID: pid, PPID: p.ppid, Pgrp: p.pgrp, Session: p.sid, State: p.state, Comm: p.comm, Start: p.start}, true
}

// socketInodes calls fn for every fd in fdDir that links to a socket whose
// inode is in want. Unreadable directories and vanished fds are skipped.
func socketInodes(fdDir string, want map[uint64]netip.AddrPort, fn func(uint64)) {
	d, err := os.Open(fdDir)
	if err != nil {
		return
	}
	fds, _ := d.Readdirnames(-1)
	d.Close()
	for _, fd := range fds {
		target, err := os.Readlink(fdDir + "/" + fd)
		if err != nil {
			continue
		}
		rest, ok := strings.CutPrefix(target, "socket:[")
		if !ok {
			continue
		}
		rest, ok = strings.CutSuffix(rest, "]")
		if !ok {
			continue
		}
		ino, err := strconv.ParseUint(rest, 10, 64)
		if err != nil {
			continue
		}
		if _, ok := want[ino]; ok {
			fn(ino)
		}
	}
}

// fullComm returns the base name of the process's argv[0] or executable if it
// extends the (possibly truncated) comm, otherwise comm unchanged.
func fullComm(pidDir, comm string) string {
	if b, err := os.ReadFile(filepath.Join(pidDir, "cmdline")); err == nil {
		arg0, _, _ := bytes.Cut(b, []byte{0})
		if base := filepath.Base(string(arg0)); len(arg0) > 0 && len(base) > len(comm) && strings.HasPrefix(base, comm) {
			return base
		}
	}
	if exe, err := os.Readlink(filepath.Join(pidDir, "exe")); err == nil {
		exe = strings.TrimSuffix(exe, " (deleted)")
		if base := filepath.Base(exe); len(base) > len(comm) && strings.HasPrefix(base, comm) {
			return base
		}
	}
	return comm
}

// readTCP adds the listening sockets of one /proc/net/tcp{,6} table to out,
// keyed by inode.
func readTCP(path string, out map[uint64]netip.AddrPort) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() { // the header row fails to parse and is skipped
		ino, ap, ok := parseTCPLine(sc.Text())
		if ok {
			out[ino] = ap
		}
	}
	return sc.Err()
}

// parseTCPLine parses one row of /proc/net/tcp{,6}:
//
//	sl local_address rem_address st tx_queue:rx_queue tr:tm->when retrnsmt uid timeout inode …
//
// and reports it only for a listening socket with a non-zero inode.
func parseTCPLine(line string) (uint64, netip.AddrPort, bool) {
	f := strings.Fields(line)
	if len(f) < 10 || f[3] != tcpListen {
		return 0, netip.AddrPort{}, false
	}
	ap, ok := parseHexAddrPort(f[1])
	if !ok {
		return 0, netip.AddrPort{}, false
	}
	ino, err := strconv.ParseUint(f[9], 10, 64)
	if err != nil || ino == 0 {
		return 0, netip.AddrPort{}, false
	}
	return ino, ap, true
}

// parseHexAddrPort parses "0100007F:1F90" (IPv4) or a 32-digit IPv6 address
// and port. The kernel prints the address as 32-bit words in host byte
// order and the port in host order after ntohs. IPv4-mapped IPv6 addresses
// are unmapped to IPv4.
func parseHexAddrPort(s string) (netip.AddrPort, bool) {
	addrHex, portHex, ok := strings.Cut(s, ":")
	if !ok || len(portHex) != 4 {
		return netip.AddrPort{}, false
	}
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return netip.AddrPort{}, false
	}
	raw, err := hex.DecodeString(addrHex)
	if err != nil || (len(raw) != 4 && len(raw) != 16) {
		return netip.AddrPort{}, false
	}
	// Each 4-byte group was printed as a native-endian uint32: undo that.
	for i := 0; i < len(raw); i += 4 {
		binary.NativeEndian.PutUint32(raw[i:], binary.BigEndian.Uint32(raw[i:]))
	}
	var addr netip.Addr
	if len(raw) == 4 {
		addr = netip.AddrFrom4([4]byte(raw))
	} else {
		addr = netip.AddrFrom16([16]byte(raw)).Unmap()
	}
	return netip.AddrPortFrom(addr, uint16(port)), true
}
