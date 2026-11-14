//go:build dockertest

package core_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/labsbit/rov/internal/core"
	"github.com/labsbit/rov/internal/testenv"
)

// The test needs this image locally; it never pulls.
const dockerTestImage = "redis:7-alpine"

// TestDockerOwnership runs a throwaway one-service compose project owned by
// a process: discovery and ownership, default wait, a wait right after a
// restart (old containers still up), --healthy, stop (compose stop),
// single-container start, down (compose down).
// Run with: go test -tags dockertest ./internal/core/
func TestDockerOwnership(t *testing.T) {
	if err := exec.Command("docker", "image", "inspect", dockerTestImage).Run(); err != nil {
		t.Skipf("docker or image %s not available", dockerTestImage)
	}
	var rnd [4]byte
	rand.Read(rnd[:])
	project := "rovtest-" + hex.EncodeToString(rnd[:])
	p := testenv.NewDockerProject(t, "processes:\n"+
		"  kv-infra:\n    cmd: sleep 1.5 && docker compose -p "+project+" up --wait --force-recreate\n    docker: "+project+"\n")
	compose := `services:
  kv:
    image: ` + dockerTestImage + `
    pull_policy: never
    ports: ["127.0.0.1::6379"]
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 1s
      retries: 20
`
	if err := os.WriteFile(filepath.Join(p.Dir, "compose.yaml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd := exec.Command("docker", "compose", "-p", project, "down", "-v", "--remove-orphans")
		cmd.Dir = p.Dir
		_ = cmd.Run()
	})
	ctx := context.Background()

	if _, err := p.Engine.Up(ctx, "kv-infra"); err != nil {
		t.Fatal(err)
	}
	res, err := p.Engine.Wait(ctx, "kv-infra", core.WaitCondition{}, 90*time.Second)
	if err != nil || res.Outcome != core.WaitMet {
		t.Fatalf("wait: %+v %v\n%s", res, err, p.Log("kv-infra"))
	}
	pr := p.Proc(t, "kv-infra")
	if pr.DockerState != "up" || len(pr.Containers) != 1 || pr.Containers[0].Service != "kv" || len(pr.PortNumbers()) != 1 {
		t.Fatalf("kv-infra: %+v", pr)
	}
	if res, err := p.Engine.Wait(ctx, "kv-infra/kv", core.WaitCondition{Healthy: true}, 30*time.Second); err != nil || res.Outcome != core.WaitMet {
		t.Fatalf("wait --healthy: %+v %v", res, err)
	}

	// Right after a restart the old container is still up, but it isn't
	// this run's: wait holds out until compose recreated it.
	// (Start times are whole seconds: begin the restart on a new second.)
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	restarted := time.Now().Truncate(time.Second)
	if _, err := p.Engine.Restart(ctx, "kv-infra"); err != nil {
		t.Fatal(err)
	}
	res, err = p.Engine.Wait(ctx, "kv-infra", core.WaitCondition{}, 90*time.Second)
	if err != nil || res.Outcome != core.WaitMet {
		t.Fatalf("wait after restart: %+v %v\n%s", res, err, p.Log("kv-infra"))
	}
	started, err := p.Engine.Docker.StartedAt(ctx, []string{pr.Containers[0].Name})
	if err != nil || started[pr.Containers[0].Name].Before(restarted) {
		t.Fatalf("wait after restart returned with the old container (started %v, restart at %v, %v): %s", started, restarted, err, res.Message)
	}
	if st := p.Status(t); len(st.Docker.Unowned) != 0 {
		t.Errorf("owned project listed as unowned: %+v", st.Docker.Unowned)
	}

	// Stop after the one-shot exited still reads stopped: compose stop ran.
	if res, err := p.Engine.Wait(ctx, "kv-infra", core.WaitCondition{Exit: true}, 60*time.Second); err != nil || res.Outcome != core.WaitMet {
		t.Fatalf("wait --exit: %+v %v", res, err)
	}
	msg, err := p.Engine.Stop(ctx, "kv-infra")
	if err != nil || !strings.Contains(msg, "containers stopped") {
		t.Fatalf("stop: %q %v", msg, err)
	}
	pr = p.Proc(t, "kv-infra")
	if pr.State != core.StateStopped {
		t.Errorf("after stop of the exited one-shot: state %s, want stopped", pr.State)
	}
	if len(pr.Containers) != 1 || pr.Containers[0].State == "running" || !strings.HasPrefix(pr.DockerState, "degraded 0/1") {
		t.Fatalf("after stop: %+v", pr)
	}
	if _, err := p.Engine.ContainerStart(ctx, "kv-infra/kv"); err != nil {
		t.Fatal(err)
	}
	if res, err := p.Engine.Wait(ctx, "kv-infra/kv", core.WaitCondition{}, 30*time.Second); err != nil || res.Outcome != core.WaitMet {
		t.Fatalf("wait after docker start: %+v %v", res, err)
	}

	msg, err = p.Engine.Down(ctx, "kv-infra")
	if err != nil || !strings.Contains(msg, "containers removed") {
		t.Fatalf("down: %q %v", msg, err)
	}
	if pr := p.Proc(t, "kv-infra"); len(pr.Containers) != 0 || pr.HasSession {
		t.Errorf("after down: %+v", pr)
	}
}
