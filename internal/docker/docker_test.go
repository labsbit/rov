package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParsePorts(t *testing.T) {
	tests := []struct {
		in   string
		want []Port
	}{
		{"", nil},
		{"127.0.0.1:6379->6379/tcp", []Port{{6379, "127.0.0.1", "tcp"}}},
		{"9000/tcp, 127.0.0.1:8123->8123/tcp, 9009/tcp", []Port{{8123, "127.0.0.1", "tcp"}}},
		{"0.0.0.0:8123->8123/tcp, [::]:8123->8123/tcp", []Port{{8123, "0.0.0.0", "tcp"}, {8123, "::", "tcp"}}},
		{":::8123->8123/tcp", []Port{{8123, "::", "tcp"}}},
		{"[::1]:8124->8123/tcp", []Port{{8124, "::1", "tcp"}}},
		{"0.0.0.0:5000-5002->5000-5002/tcp", []Port{{5000, "0.0.0.0", "tcp"}, {5001, "0.0.0.0", "tcp"}, {5002, "0.0.0.0", "tcp"}}},
		{"0.0.0.0:53->53/udp", []Port{{53, "0.0.0.0", "udp"}}},
		{"127.0.0.1:80->80", []Port{{80, "127.0.0.1", "tcp"}}},
		{":80->80/tcp", []Port{{80, "0.0.0.0", "tcp"}}},
		{"0.0.0.0:80->80/tcp, 0.0.0.0:80->80/tcp", []Port{{80, "0.0.0.0", "tcp"}}},
		// malformed
		{"0.0.0.0:x->80/tcp", nil},
		{"0.0.0.0:0->80/tcp", nil},
		{"0.0.0.0:70000->80/tcp", nil},
		{"0.0.0.0:5002-5000->1/tcp", nil},
		{"0.0.0.0:1-65535->1/tcp", nil}, // absurd range
		{"80->80/tcp", nil},
		{"->", nil},
		{" , ,", nil},
	}
	for _, tt := range tests {
		if got := ParsePorts(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParsePorts(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestUniquePortsAndTCP(t *testing.T) {
	ps := ParsePorts("0.0.0.0:8123->8123/tcp, [::]:8123->8123/tcp, 127.0.0.1:6379->6379/tcp, 0.0.0.0:53->53/udp")
	if got, want := UniquePorts(ps), []int{53, 6379, 8123}; !slices.Equal(got, want) {
		t.Errorf("UniquePorts = %v, want %v", got, want)
	}
	if got, want := UniquePorts(TCP(ps)), []int{6379, 8123}; !slices.Equal(got, want) {
		t.Errorf("UniquePorts(TCP) = %v, want %v", got, want)
	}
	if got := UniquePorts(nil); len(got) != 0 {
		t.Errorf("UniquePorts(nil) = %v", got)
	}
}

func TestParseLabels(t *testing.T) {
	tests := []struct {
		in   string
		want map[string]string
	}{
		{"", map[string]string{}},
		{"a=b,c=d", map[string]string{"a": "b", "c": "d"}},
		{"a=,b=x=y", map[string]string{"a": "", "b": "x=y"}},
		{"f=/a.yaml,/b.yaml,g=1", map[string]string{"f": "/a.yaml,/b.yaml", "g": "1"}},
		{"junk,a=1", map[string]string{"a": "1"}},
		{"=x,a=1", map[string]string{"a": "1"}},
	}
	for _, tt := range tests {
		if got := ParseLabels(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParseLabels(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

// TestParsePSReal parses docker ps output captured on a dev box (docker
// 29.6.1), in List's format and in plain {{json .}}; both must agree.
func TestParsePSReal(t *testing.T) {
	want := []Container{
		{
			ID: "7e47b0509f50a90d19a348af0f420892482ef7f50f358e9f9c36aaa1021ba485", Name: "sag-redis", Image: "redis:7-alpine",
			Project: "search-api-gateway-local", Service: "redis", WorkingDir: "/home/dev/myapp",
			State: "running", Health: "healthy", Status: "Up 11 hours (healthy)",
			Ports: []Port{{6379, "127.0.0.1", "tcp"}},
		},
		{
			ID: "d51f3bb26bd45bc0e17460a3d8db966e78848fb318a8a893c0734190302ba7e5", Name: "sag-clickhouse", Image: "clickhouse/clickhouse-server:24.8-alpine",
			Project: "search-api-gateway-local", Service: "clickhouse", WorkingDir: "/home/dev/myapp",
			State: "running", Health: "healthy", Status: "Up 11 hours (healthy)",
			Ports: []Port{{8123, "127.0.0.1", "tcp"}},
		},
	}
	for _, file := range []string{"ps-real.jsonl", "ps-plain.jsonl"} {
		t.Run(file, func(t *testing.T) {
			b, err := os.ReadFile(filepath.Join("testdata", file))
			if err != nil {
				t.Fatal(err)
			}
			got, err := ParsePS(b)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("ParsePS =\n%s\nwant\n%s", dump(got), dump(want))
			}
		})
	}
}

func TestParsePSMalformed(t *testing.T) {
	for _, in := range []string{"not json", `{"c":`, `{"c":"string"}`, `[1,2]`} {
		if _, err := ParsePS([]byte(in)); err == nil {
			t.Errorf("ParsePS(%q): want error", in)
		}
	}
	if cs, err := ParsePS([]byte("\n  \n")); err != nil || len(cs) != 0 {
		t.Errorf("blank input: %v %v", cs, err)
	}
	// Unknown fields and missing ones are fine.
	cs, err := ParsePS([]byte(`{"c":{"Names":"x","Future":42}}`))
	if err != nil || len(cs) != 1 || cs[0].Name != "x" {
		t.Errorf("sparse line: %v %v", cs, err)
	}
}

// syntheticWant is what List returns for testdata/ps-synthetic.jsonl and the
// root /home/dev/myapp.
var syntheticWant = []struct {
	name, project, service, wd, state, health string
	ports                                     []Port
}{
	{"old-api-1", "old", "api", "/home/dev/myapp", "running", "healthy", []Port{{7000, "::", "tcp"}, {7000, "0.0.0.0", "tcp"}}},
	{"old-gone-1", "old", "gone", "/home/dev/myapp", "created", "", nil},
	{"old-paused-1", "old", "paused", "/home/dev/myapp", "paused", "", nil},
	{"plain-a-1", "plain", "a", "/home/dev/myapp/infra", "running", "healthy", []Port{{9999, "127.0.0.1", "tcp"}}},
	{"proj-cache-1", "proj", "cache", "/home/dev/myapp/apps/web", "running", "unhealthy", []Port{{6380, "127.0.0.1", "tcp"}}},
	{"proj-db-1", "proj", "db", "/home/dev/myapp/apps/web", "running", "starting", []Port{{5432, "0.0.0.0", "tcp"}, {5432, "::", "tcp"}}},
	{"proj-web-1", "proj", "web", "/home/dev/myapp/apps/web", "exited", "", nil},
}

func checkSynthetic(t *testing.T, got []Container) {
	t.Helper()
	if len(got) != len(syntheticWant) {
		t.Fatalf("got %d containers, want %d:\n%s", len(got), len(syntheticWant), dump(got))
	}
	for i, w := range syntheticWant {
		g := got[i]
		if g.Name != w.name || g.Project != w.project || g.Service != w.service || g.WorkingDir != w.wd ||
			g.State != w.state || g.Health != w.health || !reflect.DeepEqual(g.Ports, w.ports) {
			t.Errorf("container %d = %+v\nwant %+v", i, g, w)
		}
	}
}

func TestRootMatcher(t *testing.T) {
	m := newRootMatcher("/home/dev/myapp/./")
	tests := map[string]bool{
		"/home/dev/myapp":          true,
		"/home/dev/myapp/":         true,
		"/home/dev/myapp/apps/web": true,
		"/home/dev/myapp/../x":     false,
		"/home/dev/myapp-old":      false,
		"/home/dev":                false,
		"relative/dir":             false,
		"":                         false,
	}
	for dir, want := range tests {
		if got := m.match(dir); got != want {
			t.Errorf("match(%q) = %v, want %v", dir, got, want)
		}
	}
	if !Within("/x", "/") || !Within("/", "/") || Within("/ab", "/a") {
		t.Error("Within edge cases")
	}
}

func TestRootMatcherSymlinks(t *testing.T) {
	tmp := t.TempDir()
	real := filepath.Join(tmp, "real")
	link := filepath.Join(tmp, "link")
	if err := os.MkdirAll(filepath.Join(real, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// Root given through a symlink, label is the real path.
	if !newRootMatcher(link).match(filepath.Join(real, "sub")) {
		t.Error("root via symlink should match the real label")
	}
	// Root is the real path, label went through the symlink.
	if !newRootMatcher(real).match(filepath.Join(link, "sub")) {
		t.Error("label via symlink should match the real root")
	}
	if newRootMatcher(filepath.Join(real, "sub")).match(link) {
		t.Error("a parent must not match")
	}
}

func TestSummarize(t *testing.T) {
	c := func(state, health string) Container { return Container{State: state, Health: health} }
	tests := []struct {
		name           string
		cs             []Container
		up             bool
		running, total int
	}{
		{"none", nil, false, 0, 0},
		{"all healthy", []Container{c("running", "healthy"), c("running", "healthy")}, true, 2, 2},
		{"no health check counts as fine", []Container{c("running", ""), c("running", "healthy")}, true, 2, 2},
		{"one exited", []Container{c("running", "healthy"), c("exited", "")}, false, 1, 2},
		{"starting", []Container{c("running", "starting")}, false, 1, 1},
		{"unhealthy", []Container{c("running", "unhealthy"), c("running", "")}, false, 2, 2},
		{"paused", []Container{c("paused", "")}, false, 0, 1},
	}
	for _, tt := range tests {
		up, r, n := Summarize(tt.cs)
		if up != tt.up || r != tt.running || n != tt.total {
			t.Errorf("%s: Summarize = %v %d/%d, want %v %d/%d", tt.name, up, r, n, tt.up, tt.running, tt.total)
		}
	}
}

func TestProjects(t *testing.T) {
	cs := []Container{{Project: "b"}, {Project: "a"}, {Project: ""}, {Project: "b"}}
	if got := Projects(cs); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("Projects = %v", got)
	}
	if got := Projects(nil); len(got) != 0 {
		t.Errorf("Projects(nil) = %v", got)
	}
}

// fakeDocker writes a shell script standing in for the docker CLI. It
// records each invocation (cwd and args) in a log file and behaves per mode:
//
//	ok    ps prints psFile; version, compose, start, stop succeed; logs
//	      prints to stdout and stderr
//	down  every command fails like a docker CLI without a daemon
//	hang  every command sleeps
//	fail  every command fails with an ordinary daemon error
func fakeDocker(t *testing.T, mode, psFile string) (c *Client, logFile string) {
	t.Helper()
	dir := t.TempDir()
	logFile = filepath.Join(dir, "calls.log")
	if psFile != "" {
		abs, err := filepath.Abs(psFile)
		if err != nil {
			t.Fatal(err)
		}
		psFile = abs
	}
	script := fmt.Sprintf(`#!/bin/sh
echo "$(pwd) | $* | COMPOSE_FILE=${COMPOSE_FILE-unset}" >> %q
case %q in
down) echo "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?" >&2; exit 1 ;;
hang) exec sleep 5 ;;
fail) echo "Error response from daemon: No such container: $2" >&2; exit 1 ;;
esac
case "$1" in
ps) cat %q ;;
version) echo 29.6.1 ;;
logs) echo "out line 1"; echo "err line" >&2; echo "out line 2" ;;
esac
exit 0
`, logFile, mode, psFile)
	bin := filepath.Join(dir, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Client{Bin: bin, Timeout: 500 * time.Millisecond, ComposeTimeout: time.Second}, logFile
}

func calls(t *testing.T, logFile string) []string {
	t.Helper()
	b, err := os.ReadFile(logFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func TestListFake(t *testing.T) {
	ctx := context.Background()
	c, logFile := fakeDocker(t, "ok", "testdata/ps-synthetic.jsonl")
	got, err := c.List(ctx, "/home/dev/myapp")
	if err != nil {
		t.Fatal(err)
	}
	checkSynthetic(t, got)
	cl := calls(t, logFile)
	if len(cl) != 1 || !strings.Contains(cl[0], "ps --all --no-trunc --filter label=com.docker.compose.project.working_dir --format "+psFormat) {
		t.Errorf("calls = %q", cl)
	}
	if !c.Available(ctx) {
		t.Error("Available after a successful List should be true")
	}
	if len(calls(t, logFile)) != 1 {
		t.Error("Available should have used the cached positive answer")
	}

	// A root with no containers.
	got, err = c.List(ctx, "/nowhere")
	if err != nil || len(got) != 0 {
		t.Errorf("List(/nowhere) = %v, %v", got, err)
	}
}

func TestUnavailable(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		c    func(t *testing.T) (*Client, string)
	}{
		{"no binary", func(t *testing.T) (*Client, string) {
			return &Client{Bin: filepath.Join(t.TempDir(), "no-docker")}, ""
		}},
		{"daemon down", func(t *testing.T) (*Client, string) { return fakeDocker(t, "down", "") }},
		{"daemon hangs", func(t *testing.T) (*Client, string) { return fakeDocker(t, "hang", "") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, logFile := tt.c(t)
			start := time.Now()
			_, err := c.List(ctx, "/")
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("List err = %v, want ErrUnavailable", err)
			}
			if d := time.Since(start); d > 3*time.Second {
				t.Errorf("List took %s", d)
			}
			// Cached: no second exec.
			n := len(calls(t, logFile))
			if c.Available(ctx) {
				t.Error("Available = true")
			}
			if _, err := c.List(ctx, "/"); !errors.Is(err, ErrUnavailable) {
				t.Errorf("second List err = %v", err)
			}
			if _, err := c.Logs(ctx, "x", 10, false); !errors.Is(err, ErrUnavailable) {
				t.Errorf("Logs err = %v", err)
			}
			if logFile != "" && len(calls(t, logFile)) != n {
				t.Errorf("negative answer not cached: %q", calls(t, logFile))
			}
		})
	}
}

func TestAvailableFresh(t *testing.T) {
	c, _ := fakeDocker(t, "ok", "testdata/ps-real.jsonl")
	if !c.Available(context.Background()) {
		t.Error("Available = false with a working fake")
	}
	c, _ = fakeDocker(t, "down", "")
	if c.Available(context.Background()) {
		t.Error("Available = true with the daemon down")
	}
}

func TestActionsFake(t *testing.T) {
	ctx := context.Background()
	t.Setenv("COMPOSE_FILE", "/somewhere/else.yaml")
	c, logFile := fakeDocker(t, "ok", "")
	for _, f := range []func() error{
		func() error { return c.ComposeStop(ctx, "search-api-gateway-local") },
		func() error { return c.ComposeDown(ctx, "search-api-gateway-local") },
		func() error { return c.Start(ctx, "sag-redis") },
		func() error { return c.Stop(ctx, "sag-redis") },
	} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	got := calls(t, logFile)
	want := []string{
		"/ | compose -p search-api-gateway-local stop | COMPOSE_FILE=unset",
		"/ | compose -p search-api-gateway-local down | COMPOSE_FILE=unset",
		"start sag-redis",
		"stop sag-redis",
	}
	for i, w := range want {
		if i >= len(got) || !strings.Contains(got[i], w) {
			t.Errorf("call %d = %q, want it to contain %q", i, at(got, i), w)
		}
	}
	for _, g := range got {
		if strings.Contains(g, " -v") || strings.Contains(g, "--volumes") {
			t.Errorf("volumes must never be removed: %q", g)
		}
	}

	if err := c.ComposeDown(ctx, ""); err == nil {
		t.Error("empty project must be refused")
	}
	if len(calls(t, logFile)) != len(want) {
		t.Error("empty project must not run docker")
	}
}

func TestActionErrors(t *testing.T) {
	ctx := context.Background()
	c, _ := fakeDocker(t, "fail", "")
	err := c.Stop(ctx, "ghost")
	if err == nil || errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "No such container: ghost") {
		t.Errorf("Stop err = %v, want docker's stderr, not ErrUnavailable", err)
	}

	c, _ = fakeDocker(t, "hang", "")
	c.ComposeTimeout = 200 * time.Millisecond
	err = c.ComposeStop(ctx, "p")
	if err == nil || errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("slow compose err = %v, want a deadline error that is not ErrUnavailable", err)
	}
}

func TestLogsFake(t *testing.T) {
	ctx := context.Background()
	c, logFile := fakeDocker(t, "ok", "")
	r, err := c.Logs(ctx, "sag-redis", 50, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
	if got := string(b); !strings.Contains(got, "out line 1") || !strings.Contains(got, "err line") || !strings.Contains(got, "out line 2") {
		t.Errorf("merged output = %q", got)
	}
	r, err = c.Logs(ctx, "sag-redis", -1, true)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(r)
	r.Close()
	got := calls(t, logFile)
	if !strings.Contains(at(got, 0), "| logs --tail 50 -- sag-redis |") || !strings.Contains(at(got, 1), "| logs --tail all --follow -- sag-redis |") {
		t.Errorf("calls = %q", got)
	}

	// A follow on a hanging docker ends on Close without an error.
	c, _ = fakeDocker(t, "hang", "")
	r, err = c.Logs(ctx, "x", 1, true)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close of a following stream = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not stop docker logs -f")
	}

	// Failing docker logs: Close reports it.
	c, _ = fakeDocker(t, "fail", "")
	r, err = c.Logs(ctx, "ghost", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(r)
	if err := r.Close(); err == nil || !strings.Contains(string(b), "No such container") {
		t.Errorf("Close = %v, output %q", err, b)
	}
}

func at(s []string, i int) string {
	if i < len(s) {
		return s[i]
	}
	return "<none>"
}

func dump(cs []Container) string {
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "  %+v\n", c)
	}
	return b.String()
}

// TestCallerDeadlineNotCached: the caller's ctx running out while docker
// answers slowly is the caller's problem, not proof that docker is down; it
// must not be cached as unavailable. Only rov's own query timeout is.
func TestCallerDeadlineNotCached(t *testing.T) {
	c, logFile := fakeDocker(t, "hang", "")
	c.Timeout = 3 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := c.List(ctx, "/")
	if err == nil || errors.Is(err, ErrUnavailable) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("List err = %v, want the caller's deadline, not ErrUnavailable", err)
	}
	if err := c.cachedUnavailable(); err != nil {
		t.Fatalf("the caller's deadline was cached as %v", err)
	}

	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	if c.Available(cancelled) {
		t.Error("Available = true on a cancelled ctx")
	}
	if _, cached := c.cached(); cached {
		t.Error("Available on a cancelled ctx cached its answer")
	}

	// The next call asks docker again, and rov's own timeout is cached.
	c.Timeout = 200 * time.Millisecond
	n := len(calls(t, logFile))
	if _, err := c.List(context.Background(), "/"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("List err = %v, want ErrUnavailable after rov's own timeout", err)
	}
	if len(calls(t, logFile)) != n+1 {
		t.Errorf("List didn't run docker again: %q", calls(t, logFile))
	}
	if err := c.cachedUnavailable(); err == nil {
		t.Error("rov's own timeout was not cached")
	}
}

// TestLogsTimeout: docker logs without follow is bounded by ComposeTimeout.
func TestLogsTimeout(t *testing.T) {
	c, _ := fakeDocker(t, "hang", "")
	c.ComposeTimeout = 300 * time.Millisecond
	start := time.Now()
	r, err := c.Logs(context.Background(), "x", 10, false)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(r)
	err = r.Close()
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("Logs without follow took %s", d)
	}
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "300ms") {
		t.Errorf("Close = %v, want a timeout error", err)
	}
	if c.cachedUnavailable() != nil {
		t.Error("a slow docker logs must not mark docker unavailable")
	}
}

func TestParseStartedAt(t *testing.T) {
	out := "43f71e02 /rovtest-kv-1 2026-11-07T21:24:51.542237555Z\n" +
		"aa11 /never 0001-01-01T00:00:00Z\n" +
		"garbage\n"
	m := ParseStartedAt([]byte(out))
	want := time.Date(2026, 11, 7, 21, 24, 51, 542237555, time.UTC)
	if !m["43f71e02"].Equal(want) || !m["rovtest-kv-1"].Equal(want) {
		t.Errorf("started = %v", m)
	}
	if _, ok := m["never"]; ok || len(m) != 2 {
		t.Errorf("a never-started container or garbage got in: %v", m)
	}
}
