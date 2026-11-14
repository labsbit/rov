//go:build linux

package ports

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"
)

// TestScanRealProc checks Scan against the live /proc: a socket this test
// opens must show up, held by this process.
func TestScanRealProc(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			addr := "127.0.0.1:0"
			wantAddr := "127.0.0.1"
			if network == "tcp6" {
				addr, wantAddr = "[::1]:0", "::1"
			}
			ln, err := net.Listen(network, addr)
			if err != nil {
				t.Skipf("no %s loopback: %v", network, err)
			}
			defer ln.Close()
			port := ln.Addr().(*net.TCPAddr).Port

			s, err := Scan()
			if err != nil {
				t.Fatal(err)
			}
			me := os.Getpid()
			if !s.Alive(me) {
				t.Fatalf("Alive(%d) = false", me)
			}
			idx := slices.IndexFunc(s.Listeners(), func(l Listener) bool { return l.Port == port })
			if idx < 0 {
				t.Fatalf("port %d not among %d listeners", port, len(s.Listeners()))
			}
			if l := s.Listeners()[idx]; l.PID != me || l.Addr != wantAddr || l.Comm == "" {
				t.Errorf("listener = %+v, want pid %d addr %s", l, me, wantAddr)
			}
			if !slices.ContainsFunc(s.PortsOf(me), func(l Listener) bool { return l.Port == port }) {
				t.Errorf("PortsOf(self) lacks %d: %+v", port, s.PortsOf(me))
			}
		})
	}
}

// TestScanRealSetsidGrandchild builds a pane-like tree for real
// (sh → sh → setsid helper): a listener in a setsid grandchild must belong to the
// tree of the outer shell.
func TestScanRealSetsidGrandchild(t *testing.T) {
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("setsid not installed")
	}
	// The test binary itself is the listener, so no interpreter is needed.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // free it for the child

	// A name longer than the kernel's 15-byte comm, to check its completion.
	const longName = "rov-ports-long-listener"
	bin := filepath.Join(t.TempDir(), longName)
	if err := os.Symlink(os.Args[0], bin); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("setsid", "--wait", "sh", "-c", `sh -c "setsid $0 -test.run=TestHelperListen" & wait`, bin)
	cmd.Env = append(os.Environ(), "PORTS_HELPER_LISTEN=127.0.0.1:"+strconv.Itoa(port))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Kill the whole tree: the helper is in its own session.
		if s, err := Scan(); err == nil {
			for _, pid := range s.Tree(cmd.Process.Pid) {
				if p, err := os.FindProcess(pid); err == nil {
					p.Kill()
				}
			}
		}
		cmd.Process.Kill()
		cmd.Wait()
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		s, err := Scan()
		if err != nil {
			t.Fatal(err)
		}
		ports := s.PortsOf(cmd.Process.Pid)
		if i := slices.IndexFunc(ports, func(l Listener) bool { return l.Port == port }); i >= 0 {
			if ports[i].PID == cmd.Process.Pid {
				t.Errorf("port held by the root itself, want a descendant: %+v", ports[i])
			}
			if ports[i].Comm != longName {
				t.Errorf("comm = %q, want %q", ports[i].Comm, longName)
			}
			if len(s.Tree(cmd.Process.Pid)) < 2 {
				t.Errorf("tree too small: %v", s.Tree(cmd.Process.Pid))
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("port %d never appeared in tree %v", port, s.Tree(cmd.Process.Pid))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestHelperListen is not a real test: it is the listening process for
// TestScanRealSetsidGrandchild.
func TestHelperListen(t *testing.T) {
	addr := os.Getenv("PORTS_HELPER_LISTEN")
	if addr == "" {
		t.Skip("helper process only")
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		os.Exit(2)
	}
	defer ln.Close()
	time.Sleep(10 * time.Second)
	os.Exit(0)
}

func BenchmarkScan(b *testing.B) {
	// A few listeners of our own so the fd walk has work to match.
	for range 3 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			b.Fatal(err)
		}
		defer ln.Close()
	}
	var procs, listeners int
	for b.Loop() {
		s, err := Scan()
		if err != nil {
			b.Fatal(err)
		}
		procs, listeners = len(s.procs), len(s.listeners)
	}
	b.ReportMetric(float64(procs), "procs")
	b.ReportMetric(float64(listeners), "listeners")
}

// TestStartedAtReal checks the start time of this test process against the
// time it has been running, and that Proc/ReadProc agree.
func TestStartedAtReal(t *testing.T) {
	s, err := Scan()
	if err != nil {
		t.Fatal(err)
	}
	me := os.Getpid()
	at, ok := s.StartedAt(me)
	if !ok {
		t.Fatal("StartedAt(self) unknown")
	}
	if d := time.Since(at); d < 0 || d > 10*time.Minute {
		t.Errorf("started %v ago", d)
	}
	sp, ok1 := s.Proc(me)
	rp, ok2 := ReadProc(me)
	if !ok1 || !ok2 || sp.Start != rp.Start || sp.Start == 0 || rp.PPID != os.Getppid() || rp.Pgrp == 0 {
		t.Errorf("Proc %+v %v, ReadProc %+v %v", sp, ok1, rp, ok2)
	}
	if _, ok := ReadProc(1 << 30); ok {
		t.Error("ReadProc of a missing pid succeeded")
	}
}
