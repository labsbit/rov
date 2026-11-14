package ports

import (
	"bufio"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// fixtureProc copies testdata/proc into a temp dir and creates the fd
// symlinks listed in testdata/fds. It reports whether unreadable fd
// directories really are unreadable (false when running as root).
func fixtureProc(t *testing.T) (root string, permsEnforced bool) {
	t.Helper()
	root = filepath.Join(t.TempDir(), "proc")
	if err := os.CopyFS(root, os.DirFS("testdata/proc")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open("testdata/fds")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var locked []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		dir := filepath.Join(root, fields[0], "fd")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if fields[1] == "-" {
			locked = append(locked, dir)
			continue
		}
		if err := os.Symlink(fields[2], filepath.Join(dir, fields[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	// The locked dir gets a socket link too, to prove it isn't read.
	for _, dir := range locked {
		if err := os.Symlink("socket:[1006]", filepath.Join(dir, "5")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(dir, 0o755) })
	}
	_, err = os.ReadDir(locked[0])
	return root, err != nil
}

func TestScanAtFixture(t *testing.T) {
	root, permsEnforced := fixtureProc(t)
	s, err := ScanAt(root)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("Listeners", func(t *testing.T) {
		want := []Listener{
			{Port: 3001, Addr: "127.0.0.1", Inode: 1005, PID: 300, Comm: "node"}, // v4-mapped
			{Port: 3001, Addr: "127.0.0.1", Inode: 1005, PID: 301, Comm: "node"}, // shared across fork
			{Port: 3773, Addr: "127.0.0.1", Inode: 1012, PID: 502, Comm: "node-MainThread"},
			{Port: 5432, Addr: "0.0.0.0", Inode: 1006}, // other user's process
			{Port: 8080, Addr: "0.0.0.0", Inode: 1001, PID: 203, Comm: "api-gateway"},
			{Port: 8080, Addr: "::", Inode: 1002, PID: 203, Comm: "api-gateway"},
			{Port: 8081, Addr: "0.0.0.0", Inode: 1009, PID: 203, Comm: "api-gateway"}, // SO_REUSEPORT pair, once
			{Port: 9101, Addr: "127.0.0.1", Inode: 1003, PID: 210, Comm: "weird) (name"},
			{Port: 9102, Addr: "::1", Inode: 1004, PID: 211, Comm: "helper"},
			{Port: 9200, Addr: "0.0.0.0", Inode: 1007, PID: 500, Comm: "worker-search-rust"}, // completed from cmdline
			{Port: 41538, Addr: "fd7a:115c:a1e0::f931:3273", Inode: 1011},
		}
		if !permsEnforced {
			// As root the "other user" fd dir is readable after all.
			want[3].PID, want[3].Comm = 400, "postgres"
		}
		if got := s.Listeners(); !reflect.DeepEqual(got, want) {
			t.Errorf("Listeners() =\n%s\nwant\n%s", fmtListeners(got), fmtListeners(want))
		}
	})

	t.Run("Tree", func(t *testing.T) {
		tests := []struct {
			name string
			root int
			want []int
		}{
			{"pane with setsid child and grandchildren", 200, []int{200, 201, 202, 203, 210, 211}},
			{"forked server", 300, []int{300, 301}},
			{"zombie child still listed", 500, []int{500, 501}},
			{"setsid child alone", 210, []int{210, 211}},
			{"leaf", 203, []int{203}},
			{"tmux server covers all panes", 100, []int{100, 200, 201, 202, 203, 210, 211, 300, 301, 500, 501, 502}},
			{"unknown pid", 99999, nil},
			{"pid 0 never matches kernel threads", 0, nil},
			{"negative", -1, nil},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := s.Tree(tt.root); !slices.Equal(got, tt.want) {
					t.Errorf("Tree(%d) = %v, want %v", tt.root, got, tt.want)
				}
			})
		}
	})

	t.Run("PortsOf", func(t *testing.T) {
		tests := []struct {
			root int
			want []string // port/addr/pid
		}{
			{200, []string{"8080/0.0.0.0/203", "8080/::/203", "8081/0.0.0.0/203", "9101/127.0.0.1/210", "9102/::1/211"}},
			{300, []string{"3001/127.0.0.1/300"}},
			{201, []string{"8080/0.0.0.0/203", "8080/::/203", "8081/0.0.0.0/203"}},
			{400, nil}, // its socket is in an unreadable fd dir
			{99999, nil},
		}
		for _, tt := range tests {
			var got []string
			for _, l := range s.PortsOf(tt.root) {
				got = append(got, fmtL(l))
			}
			if tt.root == 400 && !permsEnforced {
				continue // as root, the fd dir is readable
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("PortsOf(%d) = %v, want %v", tt.root, got, tt.want)
			}
		}
	})

	t.Run("Alive", func(t *testing.T) {
		for pid, want := range map[int]bool{203: true, 1: true, 501: false, 600: false, 700: false, 99999: false} {
			if got := s.Alive(pid); got != want {
				t.Errorf("Alive(%d) = %v, want %v", pid, got, want)
			}
		}
	})

	t.Run("Comm", func(t *testing.T) {
		for pid, want := range map[int]string{100: "tmux: server", 210: "weird) (name", 500: "worker-search-r", 4242: ""} {
			if got := s.Comm(pid); got != want {
				t.Errorf("Comm(%d) = %q, want %q", pid, got, want)
			}
		}
	})
}

func TestScanAtMissing(t *testing.T) {
	if _, err := ScanAt(filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Fatal("want error for a missing proc root")
	}
	// A root without net tables is fine: processes, no listeners.
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "42"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "42", "stat"), []byte("42 (x) S 1 42 42 0"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := ScanAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Alive(42) || len(s.Listeners()) != 0 {
		t.Errorf("Alive(42)=%v Listeners=%v", s.Alive(42), s.Listeners())
	}
}

func TestNilSnapshot(t *testing.T) {
	var s *Snapshot
	if s.Listeners() != nil || s.Tree(1) != nil || s.PortsOf(1) != nil || s.Alive(1) || s.Comm(1) != "" {
		t.Error("nil Snapshot must answer empty")
	}
}

func TestParseHexAddrPort(t *testing.T) {
	tests := []struct {
		in   string
		want string // "" = invalid
	}{
		{"0100007F:1F90", "127.0.0.1:8080"},
		{"00000000:0016", "0.0.0.0:22"},
		{"3500007F:0035", "127.0.0.53:53"},
		{"0101A8C0:FFFF", "192.168.1.1:65535"},
		{"00000000000000000000000000000000:0016", "[::]:22"},
		{"00000000000000000000000001000000:1F90", "[::1]:8080"},
		{"0000000000000000FFFF00000100007F:0BB9", "127.0.0.1:3001"}, // v4-mapped
		{"5C117AFD0000E0A100000000733231F9:A242", "[fd7a:115c:a1e0::f931:3273]:41538"},
		{"000080FE00000000FF4B5A02F8A85AFE:0016", "[fe80::25a:4bff:fe5a:a8f8]:22"},
		{"0100007F", ""},
		{"0100007F:", ""},
		{"0100007F:1F9", ""},
		{"0100007F:XYZW", ""},
		{"0100007G:1F90", ""},
		{"01007F:1F90", ""},
		{"", ""},
	}
	for _, tt := range tests {
		ap, ok := parseHexAddrPort(tt.in)
		got := ""
		if ok {
			got = ap.String()
		}
		if got != tt.want {
			t.Errorf("parseHexAddrPort(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseStat(t *testing.T) {
	tests := []struct {
		in   string
		want procInfo
		ok   bool
	}{
		{"1 (systemd) S 0 1 1 0 -1", procInfo{ppid: 0, pgrp: 1, sid: 1, state: 'S', comm: "systemd"}, true},
		{"7 (a b) c)) R 3 7 5 0", procInfo{ppid: 3, pgrp: 7, sid: 5, state: 'R', comm: "a b) c)"}, true},
		{"8 () Z 1 8 8", procInfo{ppid: 1, pgrp: 8, sid: 8, state: 'Z', comm: ""}, true},
		{"9 (x) S 1 9", procInfo{}, false}, // too few fields
		{"10 (x) S 1 10 10 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 4448319 0", procInfo{ppid: 1, pgrp: 10, sid: 10, state: 'S', comm: "x", start: 4448319}, true},
		{"9 (x) S one 9 9", procInfo{}, false}, // bad ppid
		{"9 (x) SS 1 9 9", procInfo{}, false},  // bad state
		{"9 x S 1 9 9", procInfo{}, false},     // no parens
		{"9 )x( S 1 9 9", procInfo{}, false},   // reversed
		{"", procInfo{}, false},
	}
	for _, tt := range tests {
		got, ok := parseStat([]byte(tt.in))
		if ok != tt.ok || got != tt.want {
			t.Errorf("parseStat(%q) = %+v, %v; want %+v, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestParseTCPLine(t *testing.T) {
	tests := []struct {
		in    string
		inode uint64
		addr  string
	}{
		{"   0: 0100007F:18EB 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 10419 1 0000000000000000 100 0 0 10 0", 10419, "127.0.0.1:6379"},
		{"   1: 0100007F:18EB 0100007F:9C40 01 00000000:00000000 00:00000000 00000000     0        0 10420 1", 0, ""}, // established
		{"   2: 0100007F:18EB 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 0 1", 0, ""},     // inode 0
		{"   3: 0100007F:18EB 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 -5 1", 0, ""},    // bad inode
		{"  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode", 0, ""},
		{"", 0, ""},
	}
	for _, tt := range tests {
		ino, ap, ok := parseTCPLine(tt.in)
		if tt.inode == 0 {
			if ok {
				t.Errorf("parseTCPLine(%q) = %d %v, want rejected", tt.in, ino, ap)
			}
			continue
		}
		if !ok || ino != tt.inode || ap != netip.MustParseAddrPort(tt.addr) {
			t.Errorf("parseTCPLine(%q) = %d %v %v, want %d %s", tt.in, ino, ap, ok, tt.inode, tt.addr)
		}
	}
}

func TestSortDedupeAddrOrder(t *testing.T) {
	in := []Listener{
		{Port: 80, Addr: "::", PID: 1},
		{Port: 80, Addr: "127.0.0.1", PID: 1},
		{Port: 80, Addr: "10.0.0.1", PID: 1},
		{Port: 80, Addr: "0.0.0.0", PID: 2},
		{Port: 80, Addr: "0.0.0.0", PID: 1, Inode: 9},
		{Port: 80, Addr: "0.0.0.0", PID: 1, Inode: 3},
		{Port: 22, Addr: "::1", PID: 0},
	}
	var got []string
	for _, l := range sortDedupe(in) {
		got = append(got, fmtL(l))
	}
	want := []string{"22/::1/0", "80/0.0.0.0/1", "80/0.0.0.0/2", "80/10.0.0.1/1", "80/127.0.0.1/1", "80/::/1"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func fmtL(l Listener) string {
	return fmt.Sprintf("%d/%s/%d", l.Port, l.Addr, l.PID)
}

func fmtListeners(ls []Listener) string {
	var b strings.Builder
	for _, l := range ls {
		fmt.Fprintf(&b, "  %+v\n", l)
	}
	return b.String()
}

// BenchmarkScanAtSynthetic scans a generated tree of 500 processes with 40
// fds each (20 000 readlinks), on a regular filesystem.
func BenchmarkScanAtSynthetic(b *testing.B) {
	root := b.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "net"), 0o755); err != nil {
		b.Fatal(err)
	}
	var tcp strings.Builder
	tcp.WriteString("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n")
	for i := range 50 {
		fmt.Fprintf(&tcp, "%4d: 0100007F:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000 0 %d 1\n", i, 20000+i, 5000+i)
	}
	if err := os.WriteFile(filepath.Join(root, "net", "tcp"), []byte(tcp.String()), 0o644); err != nil {
		b.Fatal(err)
	}
	for pid := 1; pid <= 500; pid++ {
		dir := filepath.Join(root, strconv.Itoa(pid))
		if err := os.MkdirAll(filepath.Join(dir, "fd"), 0o755); err != nil {
			b.Fatal(err)
		}
		stat := fmt.Sprintf("%d (proc-%d) S %d %d %d 0 -1 0 0 0 0 0 0 0\n", pid, pid, pid/2, pid, pid)
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
			b.Fatal(err)
		}
		for fd := range 40 {
			target := fmt.Sprintf("socket:[%d]", 100000+pid*40+fd)
			if pid%10 == 0 && fd == 3 {
				target = fmt.Sprintf("socket:[%d]", 5000+pid/10)
			}
			if err := os.Symlink(target, filepath.Join(dir, "fd", strconv.Itoa(fd))); err != nil {
				b.Fatal(err)
			}
		}
	}
	for b.Loop() {
		if _, err := ScanAt(root); err != nil {
			b.Fatal(err)
		}
	}
}
