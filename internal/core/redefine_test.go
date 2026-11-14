package core_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/labsbit/rov/internal/core"
	"github.com/labsbit/rov/internal/testenv"
)

// Redefine changes the stored definition without restarting; the next
// restart runs it.
func TestRedefine(t *testing.T) {
	p := testenv.NewProject(t, "processes:\n  web: { cmd: 'true' }\n")
	if err := os.Mkdir(filepath.Join(p.Dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	port := testenv.FreePort(t)
	if _, err := p.Engine.Spawn(bg, core.SpawnOptions{Name: "srv", Cmd: testenv.Helper(listenArg(port))}); err != nil {
		t.Fatal(err)
	}
	before := waitMet(t, p, "srv", core.WaitCondition{}).Process

	newCmd := testenv.Helper(listenArg(port), "--color")
	msg, err := p.Engine.Redefine(bg, core.SpawnOptions{Name: "srv", Cmd: newCmd, Dir: "sub", Env: envOf("A", "1")})
	if err != nil || msg != "definition saved; restart to apply" {
		t.Fatalf("redefine: %q %v", msg, err)
	}
	pr := p.Proc(t, "srv")
	if pr.PID != before.PID || !pr.Running() {
		t.Fatalf("redefine restarted the process: pid %d → %d, state %s", before.PID, pr.PID, pr.State)
	}
	if pr.Cmd != newCmd || pr.Cwd != "sub" || !slices.Equal(pr.EnvKeys, []string{"A"}) || pr.Kind != core.KindAdhoc {
		t.Errorf("stored definition: cmd %q cwd %q env %v kind %s", pr.Cmd, pr.Cwd, pr.EnvKeys, pr.Kind)
	}
	// The running process still runs the old definition: changed, like an
	// edited rov.yaml entry.
	if !pr.Changed || pr.RunningCmd != before.Cmd {
		t.Errorf("after redefine: changed %v running_cmd %q, want true %q", pr.Changed, pr.RunningCmd, before.Cmd)
	}
	if pr.RunID == "" || pr.RunID != before.RunID {
		t.Errorf("redefine changed the run id: %q → %q", before.RunID, pr.RunID)
	}
	// A second redefine keeps what runs; redefining back clears changed.
	if _, err := p.Engine.Redefine(bg, core.SpawnOptions{Name: "srv", Cmd: newCmd + " ", Dir: "sub"}); err != nil {
		t.Fatal(err)
	}
	if pr := p.Proc(t, "srv"); !pr.Changed || pr.RunningCmd != before.Cmd {
		t.Errorf("after a second redefine: changed %v running_cmd %q", pr.Changed, pr.RunningCmd)
	}
	if _, err := p.Engine.Redefine(bg, core.SpawnOptions{Name: "srv", Cmd: before.Cmd}); err != nil {
		t.Fatal(err)
	}
	if pr := p.Proc(t, "srv"); pr.Changed {
		t.Errorf("redefined back to what runs, still changed (running %q)", pr.RunningCmd)
	}
	if _, err := p.Engine.Redefine(bg, core.SpawnOptions{Name: "srv", Cmd: newCmd, Dir: "sub", Env: envOf("A", "1")}); err != nil {
		t.Fatal(err)
	}

	if _, err := p.Engine.Restart(bg, "srv"); err != nil {
		t.Fatal(err)
	}
	pr = waitMet(t, p, "srv", core.WaitCondition{}).Process
	if pr.PID == before.PID || !strings.Contains(pr.Cmd, "--color") {
		t.Errorf("after restart: pid %d cmd %q", pr.PID, pr.Cmd)
	}
	if pr.Changed || pr.RunID == before.RunID {
		t.Errorf("after restart: changed %v, run id %q (before %q)", pr.Changed, pr.RunID, before.RunID)
	}

	for name, o := range map[string]core.SpawnOptions{
		"unknown":    {Name: "nope", Cmd: "true"},
		"config":     {Name: "web", Cmd: "true"},
		"no command": {Name: "srv", Cmd: " "},
		"bad cwd":    {Name: "srv", Cmd: "true", Dir: "missing"},
		"bad env":    {Name: "srv", Cmd: "true", Env: envOf("1X", "y")},
	} {
		if _, err := p.Engine.Redefine(bg, o); err == nil {
			t.Errorf("%s: redefine accepted %+v", name, o)
		}
	}
}

// Every start gets a new run id, also several within one second, and the
// id stays when the run ends.
func TestRunIDPerStart(t *testing.T) {
	p := testenv.NewProject(t, "processes:\n  web: { cmd: 'sleep 100' }\n")
	if _, err := p.Engine.Up(bg, "web"); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i := range 3 {
		pr := p.Proc(t, "web")
		if pr.RunID == "" || seen[pr.RunID] {
			t.Fatalf("start %d: run id %q (seen %v)", i, pr.RunID, seen)
		}
		seen[pr.RunID] = true
		if _, err := p.Engine.Restart(bg, "web"); err != nil {
			t.Fatal(err)
		}
	}
	id := p.Proc(t, "web").RunID
	if _, err := p.Engine.Stop(bg, "web"); err != nil {
		t.Fatal(err)
	}
	if pr := p.Proc(t, "web"); pr.Running() || pr.RunID != id {
		t.Errorf("after stop: state %s run id %q, want the run's %q", pr.State, pr.RunID, id)
	}
	if _, err := p.Engine.Down(bg, "web"); err != nil {
		t.Fatal(err)
	}
	if pr := p.Proc(t, "web"); pr.RunID != "" {
		t.Errorf("without a session: run id %q", pr.RunID)
	}
}

// While a restart kills the old run, a status read in between says
// stopped, never dead: the signal is rov's own.
func TestRestartKilledRunReadsStopped(t *testing.T) {
	p := testenv.NewProject(t, "processes:\n  web: { cmd: 'sleep 100' }\n")
	if _, err := p.Engine.Up(bg, "web"); err != nil {
		t.Fatal(err)
	}
	var seen []core.State
	core.SetKill(p.Engine, func(pid int, sig syscall.Signal) error {
		err := syscall.Kill(pid, sig)
		if sig != syscall.SIGTERM || len(seen) > 0 {
			return err
		}
		// the pane dies; look at it before the respawn
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			st, serr := p.Engine.Status(bg)
			if serr == nil {
				if pr := st.Get("web"); pr != nil && !pr.Running() {
					seen = append(seen, pr.State)
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		return err
	})
	defer core.SetKill(p.Engine, nil)
	if _, err := p.Engine.Restart(bg, "web"); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != core.StateStopped {
		t.Errorf("states while the old run died: %v, want [stopped]", seen)
	}
	if pr := p.Proc(t, "web"); !pr.Running() {
		t.Errorf("after restart: %s", pr.State)
	}
}
