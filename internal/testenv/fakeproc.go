package testenv

import (
	"encoding/binary"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/labsbit/rov/internal/config"
	"github.com/labsbit/rov/internal/core"
	"github.com/labsbit/rov/internal/docker"
	"github.com/labsbit/rov/internal/ports"
	"github.com/labsbit/rov/internal/tmux"
)

// FakeProc builds a /proc tree for ports.ScanAt.
type FakeProc struct {
	t         testing.TB
	dir       string
	tcp, tcp6 []string
	inode     int
}

// NewFakeProc returns an empty fake /proc in a temp dir.
func NewFakeProc(t testing.TB) *FakeProc {
	dir := filepath.Join(t.TempDir(), "proc")
	if err := os.MkdirAll(filepath.Join(dir, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &FakeProc{t: t, dir: dir, inode: 5000}
}

// Proc adds a process.
func (f *FakeProc) Proc(pid, ppid, sid int, comm string) {
	d := filepath.Join(f.dir, strconv.Itoa(pid))
	if err := os.MkdirAll(filepath.Join(d, "fd"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	stat := fmt.Sprintf("%d (%s) S %d %d %d 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 1 1 1\n", pid, comm, ppid, pid, sid)
	if err := os.WriteFile(filepath.Join(d, "stat"), []byte(stat), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// Listen adds a listening socket on addr:port held by pid (0: by nobody
// visible).
func (f *FakeProc) Listen(pid int, addr string, port int) {
	f.inode++
	a := netip.MustParseAddr(addr)
	var hexAddr string
	raw := a.AsSlice()
	for i := 0; i < len(raw); i += 4 {
		var w [4]byte
		binary.BigEndian.PutUint32(w[:], binary.NativeEndian.Uint32(raw[i:i+4]))
		hexAddr += fmt.Sprintf("%02X%02X%02X%02X", w[0], w[1], w[2], w[3])
	}
	line := fmt.Sprintf("0: %s:%04X 00000000:0000 0A 00000000:00000000 00:00000000 00000000 1000 0 %d 1", hexAddr, port, f.inode)
	if a.Is4() {
		f.tcp = append(f.tcp, line)
	} else {
		f.tcp6 = append(f.tcp6, line)
	}
	if pid > 0 {
		link := filepath.Join(f.dir, strconv.Itoa(pid), "fd", strconv.Itoa(f.inode))
		if err := os.Symlink(fmt.Sprintf("socket:[%d]", f.inode), link); err != nil {
			f.t.Fatal(err)
		}
	}
}

// Snapshot scans the fake tree.
func (f *FakeProc) Snapshot() *ports.Snapshot {
	write := func(name string, lines []string) {
		s := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"
		for _, l := range lines {
			s += l + "\n"
		}
		if err := os.WriteFile(filepath.Join(f.dir, "net", name), []byte(s), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	write("tcp", f.tcp)
	write("tcp6", f.tcp6)
	snap, err := ports.ScanAt(f.dir)
	if err != nil {
		f.t.Fatal(err)
	}
	return snap
}

// SampleTime is the "now" of SampleInputs.
var SampleTime = time.Date(2026, 11, 7, 10, 14, 12, 0, time.UTC)

// SampleInputs is a project in every interesting state, for Compose and
// output tests:
//
//   - infra exited 0, owning two healthy containers (6379, 8123)
//   - gateway running 2h13m; its child holds 8080 on 0.0.0.0 and ::
//   - tracking running with two ports
//   - worker-rust crashed(101); its last port 9101 is now held by a foreign
//     pid 7712
//   - api running with a changed rov.yaml entry
//   - panel absent
//   - ad-hoc mock-stripe running 4m on 12111, ad-hoc old-mock stopped
//   - an unowned compose project "scratch" with one exited container
//   - a socket of another user on 5432
func SampleInputs(t testing.TB) core.Inputs {
	root := "/home/dev/myapp"
	cfg := &config.Config{Path: root + "/rov.yaml", LogDir: ".logs", LogMax: config.DefaultLogMax, Processes: []config.Process{
		{Name: "infra", Cmd: "just infra", Description: "Redis + ClickHouse (docker)", Docker: "search-api-gateway-local"},
		{Name: "gateway", Cmd: "just gateway", Description: "search gateway"},
		{Name: "tracking", Cmd: "just tracking"},
		{Name: "worker-rust", Cmd: "just worker-rust"},
		{Name: "api", Cmd: "just api", Env: []config.EnvVar{{Key: "PORT", Value: "3001"}}},
		{Name: "panel", Cmd: "just panel"},
	}}
	f := NewFakeProc(t)
	f.Proc(1, 0, 1, "systemd")
	f.Proc(41233, 1, 41233, "just")
	f.Proc(41260, 41233, 41233, "go")
	f.Proc(41301, 41260, 41233, "api-gateway")
	f.Listen(41301, "0.0.0.0", 8080)
	f.Listen(41301, "::", 8080)
	f.Proc(41390, 1, 41390, "just")
	f.Proc(41402, 41390, 41390, "tracking-servi")
	f.Listen(41402, "127.0.0.1", 8090)
	f.Listen(41402, "127.0.0.1", 8091)
	f.Proc(41502, 1, 41502, "just")
	f.Proc(41530, 41502, 41502, "bun")
	f.Listen(41530, "0.0.0.0", 3001)
	f.Proc(51002, 1, 51002, "node")
	f.Listen(51002, "127.0.0.1", 12111)
	f.Proc(7712, 1, 7712, "worker-search-r")
	f.Listen(7712, "0.0.0.0", 9101)
	f.Listen(0, "0.0.0.0", 5432)
	f.Listen(0, "127.0.0.1", 6379) // docker-proxy, root's
	f.Listen(0, "127.0.0.1", 8123)

	started := func(d time.Duration) string { return strconv.FormatInt(SampleTime.Add(-d).Unix(), 10) }
	exit := func(n int) *int { return &n }
	opts := func(kv ...string) map[string]string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	sessions := []tmux.Session{
		{Name: "infra", PaneID: "%1", PanePID: 41100, Dead: true, DeadStatus: exit(0),
			Options: opts(core.OptKind, "config", core.OptCmd, "just infra", core.OptCwd, ".", core.OptStarted, started(2*time.Hour+20*time.Minute), core.OptLastPorts, "6379,8123")},
		{Name: "gateway", PaneID: "%2", PanePID: 41233,
			Options: opts(core.OptKind, "config", core.OptCmd, "just gateway", core.OptCwd, ".", core.OptStarted, started(2*time.Hour+13*time.Minute), core.OptLastPorts, "8080")},
		{Name: "tracking", PaneID: "%3", PanePID: 41390,
			Options: opts(core.OptKind, "config", core.OptCmd, "just tracking", core.OptCwd, ".", core.OptStarted, started(2*time.Hour+13*time.Minute))},
		{Name: "worker-rust", PaneID: "%4", PanePID: 41450, Dead: true, DeadStatus: exit(101),
			Options: opts(core.OptKind, "config", core.OptCmd, "just worker-rust", core.OptCwd, ".", core.OptStarted, started(time.Hour), core.OptLastPorts, "9101")},
		{Name: "api", PaneID: "%5", PanePID: 41502,
			Options: opts(core.OptKind, "config", core.OptCmd, "just api", core.OptCwd, ".", core.OptStarted, started(2*time.Hour+10*time.Minute))},
		{Name: "old-mock", PaneID: "%7", PanePID: 50001, Dead: true,
			Options: opts(core.OptKind, "adhoc", core.OptCmd, "node old.js", core.OptCwd, "tools", core.OptStopped, "1", core.OptStarted, started(time.Hour), core.OptLastPorts, "12000")},
		{Name: "mock-stripe", PaneID: "%6", PanePID: 51002,
			Options: opts(core.OptKind, "adhoc", core.OptCmd, "node scripts/mock-stripe.js", core.OptCwd, ".", core.OptEnv, `["STRIPE_KEY=sk_test"]`, core.OptStarted, started(4*time.Minute))},
	}
	wd := root
	containers := []docker.Container{
		{Name: "sag-clickhouse", Project: "search-api-gateway-local", Service: "clickhouse", WorkingDir: wd, State: "running", Health: "healthy", Status: "Up 2 hours (healthy)",
			Ports: []docker.Port{{Port: 8123, Addr: "127.0.0.1", Proto: "tcp"}}},
		{Name: "sag-redis", Project: "search-api-gateway-local", Service: "redis", WorkingDir: wd, State: "running", Health: "healthy", Status: "Up 2 hours (healthy)",
			Ports: []docker.Port{{Port: 6379, Addr: "127.0.0.1", Proto: "tcp"}, {Port: 6379, Addr: "127.0.0.1", Proto: "udp"}}},
		{Name: "scratch-db-1", Project: "scratch", Service: "db", WorkingDir: wd + "/tools/scratch", State: "exited", Status: "Exited (0) 3 days ago"},
	}
	return core.Inputs{
		Root: root, Project: "myapp", Socket: "rov-myapp-3f9a1c2e",
		Config: cfg, LogDir: root + "/.logs",
		Sessions: sessions, Ports: f.Snapshot(), Containers: containers,
		Now: SampleTime,
	}
}
