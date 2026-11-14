//go:build dockertest

package docker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The integration test needs this image locally; it never pulls.
const testImage = "redis:7-alpine"

const testCompose = `services:
  kv:
    image: ` + testImage + `
    pull_policy: never
    ports: ["127.0.0.1::6379"]
    volumes: [kvdata:/data]
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 1s
      timeout: 2s
      retries: 20
volumes:
  kvdata: {}
`

// TestComposeLifecycle runs a throwaway one-service compose project:
// discovery, ports, health, compose stop, container start/stop, logs and
// compose down. Run with: go test -tags dockertest ./internal/docker/
func TestComposeLifecycle(t *testing.T) {
	ctx := context.Background()
	c := Default()
	if !c.Available(ctx) {
		t.Skip("docker not available")
	}
	if err := exec.Command("docker", "image", "inspect", testImage).Run(); err != nil {
		t.Skipf("image %s not present locally", testImage)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(testCompose), 0o644); err != nil {
		t.Fatal(err)
	}
	var rnd [4]byte
	rand.Read(rnd[:])
	project := "rovtest-" + hex.EncodeToString(rnd[:])
	compose := func(args ...string) error {
		cmd := exec.Command("docker", append([]string{"compose", "-p", project, "-f", "compose.yaml"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Logf("docker compose %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return err
	}
	t.Cleanup(func() { compose("down", "-v", "--remove-orphans") }) // test cleanup only
	if err := compose("up", "-d", "--wait"); err != nil {
		t.Fatal("compose up failed")
	}

	list := func() []Container {
		t.Helper()
		cs, err := c.List(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		return cs
	}

	// Discovery.
	cs := list()
	if len(cs) != 1 {
		t.Fatalf("List = %s", dump(cs))
	}
	ct := cs[0]
	if ct.Project != project || ct.Service != "kv" || ct.State != "running" || ct.Health != "healthy" {
		t.Errorf("container = %+v", ct)
	}
	if up, r, n := Summarize(cs); !up || r != 1 || n != 1 {
		t.Errorf("Summarize = %v %d/%d", up, r, n)
	}
	if len(ct.Ports) != 1 || ct.Ports[0].Addr != "127.0.0.1" || ct.Ports[0].Proto != "tcp" {
		t.Fatalf("ports = %+v", ct.Ports)
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(ct.Ports[0].Port)), 2*time.Second)
	if err != nil {
		t.Errorf("published port not reachable: %v", err)
	} else {
		conn.Close()
	}

	// A sibling root doesn't see it.
	if cs, err := c.List(ctx, filepath.Join(dir, "sub")); err != nil || len(cs) != 0 {
		t.Errorf("List(sub) = %v, %v", cs, err)
	}

	// Logs.
	r, err := c.Logs(ctx, ct.Name, 20, false)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	if err := r.Close(); err != nil || !strings.Contains(string(b), "Ready to accept connections") {
		t.Errorf("Logs: %v\n%s", err, b)
	}

	// compose stop keeps the container.
	if err := c.ComposeStop(ctx, project); err != nil {
		t.Fatal(err)
	}
	cs = list()
	if len(cs) != 1 || cs[0].State != "exited" {
		t.Fatalf("after ComposeStop: %s", dump(cs))
	}
	if up, r, n := Summarize(cs); up || r != 0 || n != 1 {
		t.Errorf("Summarize after stop = %v %d/%d", up, r, n)
	}
	if len(cs[0].Ports) != 0 {
		t.Errorf("a stopped container publishes nothing: %+v", cs[0].Ports)
	}

	// Single container start → healthy again; stop.
	if err := c.Start(ctx, ct.Name); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		cs = list()
		if len(cs) == 1 && cs[0].State == "running" && cs[0].Health == "healthy" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never healthy after Start: %s", dump(cs))
		}
		time.Sleep(250 * time.Millisecond)
	}
	if err := c.Stop(ctx, ct.Name); err != nil {
		t.Fatal(err)
	}
	if cs = list(); len(cs) != 1 || cs[0].State != "exited" {
		t.Errorf("after Stop: %s", dump(cs))
	}

	// compose down removes the container, keeps the volume.
	if err := c.ComposeDown(ctx, project); err != nil {
		t.Fatal(err)
	}
	if cs = list(); len(cs) != 0 {
		t.Errorf("after ComposeDown: %s", dump(cs))
	}
	out, err := exec.Command("docker", "volume", "ls", "-q", "--filter", "label=com.docker.compose.project="+project).Output()
	if err != nil || !strings.Contains(string(out), project+"_kvdata") {
		t.Errorf("ComposeDown must keep volumes: %v %q", err, out)
	}
}
