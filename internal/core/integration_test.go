package core_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/labsbit/rov/internal/core"
	"github.com/labsbit/rov/internal/lock"
	"github.com/labsbit/rov/internal/logs"
	"github.com/labsbit/rov/internal/ports"
	"github.com/labsbit/rov/internal/testenv"
)

func TestMain(m *testing.M) { testenv.Main(m) }

var bg = context.Background()

func listenArg(port int) string { return "--listen=127.0.0.1:" + strconv.Itoa(port) }

// yamlCmd quotes a shell command for a flow-style rov.yaml entry.
func yamlCmd(cmd string) string { return "'" + strings.ReplaceAll(cmd, "'", "''") + "'" }

func waitMet(t *testing.T, p *testenv.Project, name string, cond core.WaitCondition) core.WaitResult {
	t.Helper()
	res, err := p.Engine.Wait(bg, name, cond, 15*time.Second)
	if err != nil {
		t.Fatalf("wait %s: %v", name, err)
	}
	if res.Outcome != core.WaitMet {
		t.Fatalf("wait %s: outcome %v: %s\nlog:\n%s", name, res.Outcome, res.Message, p.Log(name))
	}
	return res
}

// spawn → status shows the port → wait returns → stop frees the port.
func TestSpawnWaitStop(t *testing.T) {
	p := testenv.NewProject(t, "processes: {}\n")
	port := testenv.FreePort(t)
	msg, err := p.Engine.Spawn(bg, core.SpawnOptions{Name: "mock", Cmd: testenv.Helper(listenArg(port)), Env: nil})
	if err != nil || msg != "started" {
		t.Fatalf("spawn: %q %v", msg, err)
	}
	res := waitMet(t, p, "mock", core.WaitCondition{})
	if !strings.Contains(res.Message, ":"+strconv.Itoa(port)) {
		t.Errorf("wait message %q", res.Message)
	}

	pr := p.Proc(t, "mock")
	if pr.Kind != core.KindAdhoc || pr.State != core.StateRunning || pr.PID == 0 {
		t.Fatalf("process = %+v", pr)
	}
	if got := pr.PortNumbers(); !slices.Equal(got, []int{port}) {
		t.Fatalf("ports = %v, want [%d]", got, port)
	}
	if pt := pr.Ports[0]; pt.Source != core.SourceProc || pt.PID == 0 || pt.Addr != "127.0.0.1" {
		t.Errorf("port = %+v", pt)
	}
	if pr.StartedAt.IsZero() || pr.Cwd == "" {
		t.Errorf("started %v cwd %q", pr.StartedAt, pr.Cwd)
	}

	if msg, err := p.Engine.Stop(bg, "mock"); err != nil || msg != "stopped" {
		t.Fatalf("stop: %q %v", msg, err)
	}
	if !testenv.PortFree(port) {
		t.Fatal("port still taken after stop")
	}
	pr = p.Proc(t, "mock")
	if pr.State != core.StateStopped || !slices.Equal(pr.LastPorts, []int{port}) || len(pr.Ports) != 0 {
		t.Errorf("after stop: state %s last %v ports %v", pr.State, pr.LastPorts, pr.Ports)
	}
	if msg, _ := p.Engine.Stop(bg, "mock"); msg != "not running" {
		t.Errorf("second stop: %q", msg)
	}
	// up restarts the stored definition.
	if msg, err := p.Engine.Up(bg, "mock"); err != nil || msg != "started" {
		t.Fatalf("up: %q %v", msg, err)
	}
	waitMet(t, p, "mock", core.WaitCondition{Port: port})
	if msg, _ := p.Engine.Up(bg, "mock"); msg != "already running" {
		t.Errorf("up again: %q", msg)
	}
}

// restart while a grandchild holds the port (the `go run` case), and while a
// setsid'd child holds it (which a process-group kill misses).
func TestRestartTreeHoldingPort(t *testing.T) {
	for _, mode := range []string{"--child", "--setsid"} {
		t.Run(mode, func(t *testing.T) {
			port := testenv.FreePort(t)
			args := []string{"--child", listenArg(port)}
			if mode == "--setsid" {
				args = append(args, "--setsid")
			}
			p := testenv.NewProject(t, "processes:\n  srv: { cmd: "+yamlCmd(testenv.Helper(args...))+" }\n")
			if _, err := p.Engine.Up(bg, "srv"); err != nil {
				t.Fatal(err)
			}
			waitMet(t, p, "srv", core.WaitCondition{Port: port})
			first := p.Proc(t, "srv")
			holder := first.Ports[0].PID
			if holder == first.PID {
				t.Fatalf("port held by the pane process itself; want a descendant")
			}

			if msg, err := p.Engine.Restart(bg, "srv"); err != nil || msg != "restarted" {
				t.Fatalf("restart: %q %v", msg, err)
			}
			res := waitMet(t, p, "srv", core.WaitCondition{Port: port})
			if res.Process.Ports[0].PID == holder {
				t.Fatalf("port still held by the old pid %d", holder)
			}
			// The new server got the port: nothing crashed with "address in use".
			time.Sleep(300 * time.Millisecond)
			if pr := p.Proc(t, "srv"); pr.State != core.StateRunning {
				t.Fatalf("after restart: %s\n%s", pr.StateText(), p.Log("srv"))
			}
			if err := syscallKill0(holder); err == nil {
				t.Errorf("old holder %d still alive", holder)
			}
		})
	}
}

func syscallKill0(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscallSignal0)
}

// crash → crashed(N) and a last-ports warning → restart → logs --prev shows
// the crash.
func TestCrashConflictPrev(t *testing.T) {
	port := testenv.FreePort(t)
	p := testenv.NewProject(t, "processes:\n  worker: { cmd: "+yamlCmd(testenv.Helper(listenArg(port), "--crash=7", "--after=1500ms"))+" }\n")
	if _, err := p.Engine.Up(bg, "worker"); err != nil {
		t.Fatal(err)
	}
	waitMet(t, p, "worker", core.WaitCondition{Port: port}) // records last ports
	res, err := p.Engine.Wait(bg, "worker", core.WaitCondition{Exit: true}, 10*time.Second)
	if err != nil || res.Outcome != core.WaitEnded {
		t.Fatalf("wait --exit on a crash: %+v %v", res, err)
	}
	pr := p.Proc(t, "worker")
	if pr.State != core.StateCrashed || pr.ExitCode == nil || *pr.ExitCode != 7 || pr.StateText() != "crashed(7)" {
		t.Fatalf("state = %s %v", pr.StateText(), pr.ExitCode)
	}
	if !slices.Equal(pr.LastPorts, []int{port}) {
		t.Fatalf("last ports = %v", pr.LastPorts)
	}

	// Someone else takes the port: status explains it.
	ln, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	st := p.Status(t)
	want := fmt.Sprintf("worker: :%d is held by pid %d (", port, os.Getpid())
	if !slices.ContainsFunc(st.Warnings, func(w string) bool { return strings.HasPrefix(w, want) && strings.HasSuffix(w, "not managed by rov)") }) {
		t.Errorf("warnings = %q, want one starting %q", st.Warnings, want)
	}
	ln.Close()

	if _, err := p.Engine.Restart(bg, "worker"); err != nil {
		t.Fatal(err)
	}
	waitMet(t, p, "worker", core.WaitCondition{Port: port})
	prev, _, err := logs.Tail(logs.Query{Dir: p.Engine.LogDir(), Name: "worker", Run: logs.Prev})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(prev, "crashing with 7") {
		t.Errorf("prev run = %q", prev)
	}
	cur, _, _ := logs.Tail(logs.Query{Dir: p.Engine.LogDir(), Name: "worker"})
	if slices.Contains(cur, "crashing with 7") {
		t.Errorf("current run holds the old crash: %q", cur)
	}
}

// Two restarts at once run one after the other (the process lock).
func TestConcurrentRestartsSerialise(t *testing.T) {
	port := testenv.FreePort(t)
	const delay = 600 * time.Millisecond
	p := testenv.NewProject(t, "processes:\n  slow: { cmd: "+yamlCmd(testenv.Helper(listenArg(port), "--term-delay="+delay.String()))+" }\n")
	if _, err := p.Engine.Up(bg, "slow"); err != nil {
		t.Fatal(err)
	}
	waitMet(t, p, "slow", core.WaitCondition{Port: port})

	start := time.Now()
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = p.Engine.Restart(bg, "slow")
			if errs[i] == nil {
				// Each restart must find a working process to stop next.
				_, errs[i] = p.Engine.Wait(bg, "slow", core.WaitCondition{Port: port}, 10*time.Second)
			}
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if el := time.Since(start); el < 2*delay {
		t.Errorf("two restarts took %s; serialised they need at least %s", el, 2*delay)
	}
	if pr := p.Proc(t, "slow"); pr.State != core.StateRunning || !slices.Equal(pr.PortNumbers(), []int{port}) {
		t.Errorf("final: %s ports %v\n%s", pr.StateText(), pr.PortNumbers(), p.Log("slow"))
	}
	if st := p.Status(t); len(st.Processes) != 1 {
		t.Errorf("processes: %v", st.Names())
	}
}

// An exited ad-hoc process stays listed with its log until rm.
func TestExitedAdhocSurvivesUntilRm(t *testing.T) {
	p := testenv.NewProject(t, "processes: {}\n")
	if _, err := p.Engine.Spawn(bg, core.SpawnOptions{Name: "once", Cmd: testenv.Helper("--crash=0")}); err != nil {
		t.Fatal(err)
	}
	waitMet(t, p, "once", core.WaitCondition{Exit: true})
	time.Sleep(200 * time.Millisecond)
	pr := p.Proc(t, "once")
	if pr.State != core.StateExited || pr.Kind != core.KindAdhoc {
		t.Fatalf("state %s kind %s", pr.State, pr.Kind)
	}
	if !strings.Contains(p.Log("once"), "crashing with 0") {
		t.Errorf("log = %q", p.Log("once"))
	}
	// spawning the same name again replaces the exited one.
	if msg, err := p.Engine.Spawn(bg, core.SpawnOptions{Name: "once", Cmd: testenv.Helper("--crash=0")}); err != nil || msg != "replaced" {
		t.Fatalf("respawn: %q %v", msg, err)
	}
	waitMet(t, p, "once", core.WaitCondition{Exit: true})

	if msg, err := p.Engine.Rm(bg, "once", false); err != nil || msg != "removed" {
		t.Fatalf("rm: %q %v", msg, err)
	}
	if p.Status(t).Get("once") != nil {
		t.Fatal("still listed after rm")
	}
	if p.Log("once") == "" {
		t.Error("rm without --purge deleted the log")
	}
	if msg, err := p.Engine.Rm(bg, "once", true); err != nil || msg != "logs deleted" {
		t.Errorf("rm --purge of logs only: %q %v", msg, err)
	}
	if _, err := os.Stat(p.Engine.LogPaths("once").Log); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("log survived --purge: %v", err)
	}
}

func TestSpawnRules(t *testing.T) {
	port := testenv.FreePort(t)
	p := testenv.NewProject(t, "processes:\n  web: { cmd: 'true' }\n")
	if _, err := p.Engine.Spawn(bg, core.SpawnOptions{Name: "web", Cmd: "true"}); err == nil || !strings.Contains(err.Error(), "configured in rov.yaml") {
		t.Errorf("spawn of a config name: %v", err)
	}
	if _, err := p.Engine.Spawn(bg, core.SpawnOptions{Name: "Bad.Name", Cmd: "true"}); err == nil {
		t.Error("invalid name accepted")
	}
	if _, err := p.Engine.Spawn(bg, core.SpawnOptions{Name: "x", Cmd: "true", Dir: "nope"}); err == nil {
		t.Error("missing cwd accepted")
	}
	if _, err := p.Engine.Rm(bg, "web", false); err == nil {
		t.Error("rm of a config process accepted")
	}
	if _, err := p.Engine.Spawn(bg, core.SpawnOptions{Name: "srv", Cmd: testenv.Helper(listenArg(port)), Env: envOf("PORT", "1")}); err != nil {
		t.Fatal(err)
	}
	waitMet(t, p, "srv", core.WaitCondition{})
	_, err := p.Engine.Spawn(bg, core.SpawnOptions{Name: "srv", Cmd: "true"})
	if !errors.Is(err, core.ErrAlreadyRunning) {
		t.Errorf("spawn on a running name: %v", err)
	}
	if msg, err := p.Engine.Spawn(bg, core.SpawnOptions{Name: "srv", Cmd: testenv.Helper(listenArg(port), "--color"), Replace: true}); err != nil || msg != "replaced" {
		t.Fatalf("--replace: %q %v", msg, err)
	}
	pr := waitMet(t, p, "srv", core.WaitCondition{}).Process
	if !strings.Contains(pr.Cmd, "--color") || len(pr.EnvKeys) != 0 {
		t.Errorf("replaced definition: cmd %q env %v", pr.Cmd, pr.EnvKeys)
	}
}

// __pipe end to end: colour kept in .ansi, stripped in .log, and env
// FORCE_COLOR set.
func TestPipeColour(t *testing.T) {
	p := testenv.NewProject(t, "processes:\n  c: { cmd: "+yamlCmd(testenv.Helper("--color", "--crash=0")+`; echo "fc=$FORCE_COLOR cf=$CLICOLOR_FORCE"`)+" }\n")
	if _, err := p.Engine.Up(bg, "c"); err != nil {
		t.Fatal(err)
	}
	waitMet(t, p, "c", core.WaitCondition{Exit: true})
	testenv.Eventually(t, 3*time.Second, "env line in the log", func() bool { return strings.Contains(p.Log("c"), "fc=1 cf=1") })
	plain := p.Log("c")
	ansi, _ := os.ReadFile(p.Engine.LogPaths("c").ANSI)
	if strings.Contains(plain, "\x1b") || !strings.Contains(plain, "red line\nbold green plain tail\n") {
		t.Errorf(".log = %q", plain)
	}
	if !strings.Contains(string(ansi), "\x1b[31mred line\x1b[0m") || !strings.Contains(string(ansi), "bold green") {
		t.Errorf(".ansi = %q", ansi)
	}
	if strings.Count(plain, "\n") != strings.Count(string(ansi), "\n") {
		t.Errorf("files not line-aligned:\n%q\n%q", plain, ansi)
	}
}

// Rotation at a tiny log_max: both files stay within the cap, the newest
// lines are in .log and the older ones in .log.1.
func TestRotation(t *testing.T) {
	p := testenv.NewProject(t, "log_max: 4KiB\nprocesses:\n  spam: { cmd: "+yamlCmd(testenv.Helper("--spam=500", "--crash=0"))+" }\n")
	if _, err := p.Engine.Up(bg, "spam"); err != nil {
		t.Fatal(err)
	}
	waitMet(t, p, "spam", core.WaitCondition{Exit: true})
	testenv.Eventually(t, 3*time.Second, "the last line", func() bool { return strings.Contains(p.Log("spam"), "crashing with 0") })
	paths := p.Engine.LogPaths("spam")
	for _, f := range []string{paths.Log, paths.Log1, paths.ANSI, paths.ANSI1} {
		fi, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > 4096 {
			t.Errorf("%s is %d bytes, over the 4KiB cap", f, fi.Size())
		}
	}
	lines, _, err := logs.Tail(logs.Query{Dir: p.Engine.LogDir(), Name: "spam", N: 0})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(lines, "line 000500 "+strings.Repeat("x", 40)) {
		t.Errorf("last spam line missing from %d lines", len(lines))
	}
	if slices.Contains(lines, "line 000001 "+strings.Repeat("x", 40)) {
		t.Error("first line survived two rotations")
	}
}

func TestWaitOutcomes(t *testing.T) {
	p := testenv.NewProject(t, "processes:\n"+
		"  slow: { cmd: "+yamlCmd(testenv.Helper("--after=300ms", "--crash=0"))+" }\n"+
		"  boom: { cmd: "+yamlCmd(testenv.Helper("--crash=3", "--after=200ms"))+" }\n"+
		"  idle: { cmd: "+yamlCmd(testenv.Helper())+" }\n")
	for _, n := range []string{"slow", "boom", "idle"} {
		if _, err := p.Engine.Up(bg, n); err != nil {
			t.Fatal(err)
		}
	}
	// --log sees a line of the current run.
	waitMet(t, p, "idle", core.WaitCondition{Log: regexp.MustCompile(`helper \d+ up`)})
	// default: exit 0 counts.
	if res := waitMet(t, p, "slow", core.WaitCondition{}); res.Message != "exited 0" {
		t.Errorf("slow: %q", res.Message)
	}
	// a crash ends the wait (exit code 3 in the CLI).
	res, err := p.Engine.Wait(bg, "boom", core.WaitCondition{}, 10*time.Second)
	if err != nil || res.Outcome != core.WaitEnded || !strings.Contains(res.Message, "crashed(3)") {
		t.Errorf("boom: %+v %v", res, err)
	}
	// nothing listens: timeout.
	res, err = p.Engine.Wait(bg, "idle", core.WaitCondition{}, 500*time.Millisecond)
	if err != nil || res.Outcome != core.WaitTimedOut || !strings.Contains(res.Message, "no ports yet") {
		t.Errorf("idle: %+v %v", res, err)
	}
	if _, err := p.Engine.Wait(bg, "nope", core.WaitCondition{}, time.Second); err == nil {
		t.Error("wait on an unknown name succeeded")
	}
	if _, err := p.Engine.Wait(bg, "idle", core.WaitCondition{Healthy: true}, time.Second); err == nil {
		t.Error("--healthy on a process without docker accepted")
	}
}

// A changed rov.yaml entry is flagged until a restart; a name that leaves
// rov.yaml shows as ad-hoc with its stored command.
func TestChangedAndOrphaned(t *testing.T) {
	cmd := testenv.Helper()
	p := testenv.NewProject(t, "processes:\n  a: { cmd: "+yamlCmd(cmd)+" }\n")
	if _, err := p.Engine.Up(bg, "a"); err != nil {
		t.Fatal(err)
	}
	if p.Proc(t, "a").Changed {
		t.Fatal("changed right after start")
	}
	p.WriteConfig(t, "processes:\n  a:\n    cmd: "+yamlCmd(cmd)+"\n    env: { X: '1' }\n")
	pr := p.Proc(t, "a")
	if !pr.Changed || pr.RunningCmd != cmd {
		t.Fatalf("after env edit: changed %v running %q", pr.Changed, pr.RunningCmd)
	}
	if _, err := p.Engine.Restart(bg, "a"); err != nil {
		t.Fatal(err)
	}
	if pr := p.Proc(t, "a"); pr.Changed || !slices.Equal(pr.EnvKeys, []string{"X"}) {
		t.Fatalf("after restart: changed %v env %v", pr.Changed, pr.EnvKeys)
	}
	p.WriteConfig(t, "processes: {}\n")
	pr = p.Proc(t, "a")
	if pr.Kind != core.KindAdhoc || pr.Cmd != cmd || pr.State != core.StateRunning {
		t.Fatalf("orphaned: %+v", pr)
	}
	if msg, err := p.Engine.Down(bg, "a"); err != nil || msg != "removed" {
		t.Errorf("down of an ad-hoc process: %q %v", msg, err)
	}
}

func TestSendAndDown(t *testing.T) {
	p := testenv.NewProject(t, "processes:\n  e: { cmd: "+yamlCmd(testenv.Helper("--echo"))+" }\n")
	if err := p.Engine.Send(bg, "e", "hi"); !errors.Is(err, core.ErrNotRunning) {
		t.Errorf("send to an absent process: %v", err)
	}
	if _, err := p.Engine.Up(bg, "e"); err != nil {
		t.Fatal(err)
	}
	waitMet(t, p, "e", core.WaitCondition{Log: regexp.MustCompile(`up$`)})
	if err := p.Engine.Send(bg, "e", "hello; world"); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 3*time.Second, "echo", func() bool { return strings.Contains(p.Log("e"), "got: hello; world") })

	names, err := p.Engine.Resolve(bg, []string{"all"})
	if err != nil || !slices.Equal(names, []string{"e"}) {
		t.Errorf("resolve all: %v %v", names, err)
	}
	if _, err := p.Engine.Resolve(bg, []string{"e", "typo"}); err == nil {
		t.Error("resolve accepted a typo")
	}
	if msg, err := p.Engine.Down(bg, "e"); err != nil || msg != "session killed" {
		t.Fatalf("down: %q %v", msg, err)
	}
	if pr := p.Proc(t, "e"); pr.State != core.StateAbsent {
		t.Errorf("after down: %s", pr.State)
	}
	if msg, _ := p.Engine.Down(bg, "e"); msg != "no session" {
		t.Errorf("second down: %q", msg)
	}
}

// A process that ignores SIGTERM is killed after StopTimeout.
func TestStopEscalatesToKill(t *testing.T) {
	port := testenv.FreePort(t)
	p := testenv.NewProject(t, "processes:\n  stubborn: { cmd: "+yamlCmd(testenv.Helper("--child", "--ignore-term", listenArg(port)))+" }\n")
	p.Engine.StopTimeout = 500 * time.Millisecond
	if _, err := p.Engine.Up(bg, "stubborn"); err != nil {
		t.Fatal(err)
	}
	waitMet(t, p, "stubborn", core.WaitCondition{Port: port})
	start := time.Now()
	if _, err := p.Engine.Stop(bg, "stubborn"); err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el < 500*time.Millisecond {
		t.Errorf("stop took %s: it didn't wait for SIGTERM", el)
	}
	if !testenv.PortFree(port) {
		t.Error("port still taken: SIGKILL didn't reach the child")
	}
}

// A process lock held elsewhere makes an action fail, after LockTimeout,
// with a busy message naming the holder; once released, the action runs.
func TestBusyLock(t *testing.T) {
	p := testenv.NewProject(t, "processes:\n  a: { cmd: 'sleep 30' }\n")
	p.Engine.LockTimeout = 300 * time.Millisecond
	release, err := lock.Acquire(bg, p.Engine.LockPath("a"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p.Engine.LockPath("a"), []byte("rov (pid 4242) restarting a\n"), 0o644)
	start := time.Now()
	_, err = p.Engine.Up(bg, "a")
	var busy *core.BusyError
	if !errors.As(err, &busy) || err.Error() != "busy: rov (pid 4242) restarting a; try again in a moment" {
		t.Fatalf("up while locked: %v", err)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Error("gave up before LockTimeout")
	}
	release()
	if msg, err := p.Engine.Up(bg, "a"); err != nil || msg != "started" {
		t.Fatalf("up after release: %q %v", msg, err)
	}
}

// A root and a cwd with '#' in them: tmux expands formats in the start
// directory and the pipe-pane command, so rov escapes them.
func TestHashInPaths(t *testing.T) {
	p := testenv.NewProjectNamed(t, "p#S#{session_name}", "processes:\n"+
		"  hash: { cmd: \"pwd && echo 'marker#S'\", cwd: 'w#{pane_id}' }\n")
	cwd := filepath.Join(p.Dir, "w#{pane_id}")
	if err := os.Mkdir(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Engine.Up(bg, "hash"); err != nil {
		t.Fatal(err)
	}
	waitMet(t, p, "hash", core.WaitCondition{Exit: true})
	testenv.Eventually(t, 3*time.Second, "the log", func() bool { return strings.Contains(p.Log("hash"), "marker#S") })
	if log := p.Log("hash"); !strings.Contains(log, cwd+"\n") {
		t.Errorf("log %q: want the cwd %s", log, cwd)
	}
	if !strings.HasPrefix(p.Engine.LogPaths("hash").Log, p.Dir+string(filepath.Separator)) {
		t.Errorf("log path %s outside %s", p.Engine.LogPaths("hash").Log, p.Dir)
	}
	// And again through respawn-pane.
	if _, err := p.Engine.Restart(bg, "hash"); err != nil {
		t.Fatal(err)
	}
	waitMet(t, p, "hash", core.WaitCondition{Exit: true})
	testenv.Eventually(t, 3*time.Second, "the log after restart", func() bool { return strings.Contains(p.Log("hash"), cwd+"\n") })
}

// A process named "config" doesn't share the rov.yaml lock.
func TestProcessLockApartFromConfigLock(t *testing.T) {
	p := testenv.NewProject(t, "processes:\n  config: { cmd: 'sleep 30' }\n")
	p.Engine.LockTimeout = 300 * time.Millisecond
	release, err := lock.Acquire(bg, filepath.Join(p.Engine.LockDir(), "config"))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if msg, err := p.Engine.Up(bg, "config"); err != nil || msg != "started" {
		t.Fatalf("up config while rov.yaml is locked: %q %v", msg, err)
	}
	if got, want := p.Engine.LockPath("config"), filepath.Join(p.Engine.LockDir(), "proc", "config"); got != want {
		t.Errorf("LockPath = %s, want %s", got, want)
	}
}

// What survives SIGKILL (here: kill(2) refused with EPERM, as for another
// user's process) makes stop fail and leaves the process running, not
// "stopped".
func TestStopReportsSurvivors(t *testing.T) {
	p := testenv.NewProject(t, "processes:\n  tough: { cmd: "+yamlCmd(testenv.Helper())+" }\n")
	p.Engine.StopTimeout = 200 * time.Millisecond
	if _, err := p.Engine.Up(bg, "tough"); err != nil {
		t.Fatal(err)
	}
	waitMet(t, p, "tough", core.WaitCondition{Log: regexp.MustCompile("up")})
	pid := p.Proc(t, "tough").PID
	core.SetKill(p.Engine, func(target int, sig syscall.Signal) error {
		if target == pid || target == -pid {
			return syscall.EPERM
		}
		return syscall.Kill(target, sig)
	})
	_, err := p.Engine.Stop(bg, "tough")
	var serr *core.StopError
	if !errors.As(err, &serr) || !strings.Contains(err.Error(), fmt.Sprintf("could not stop pid %d", pid)) || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("stop: %v", err)
	}
	if pr := p.Proc(t, "tough"); pr.State != core.StateRunning {
		t.Errorf("after a failed stop: %s, want running", pr.State)
	}
	core.SetKill(p.Engine, nil)
	if msg, err := p.Engine.Stop(bg, "tough"); err != nil || msg != "stopped" {
		t.Fatalf("stop: %q %v", msg, err)
	}
}

// rov never signals a tree it runs in, and never a pid that was reused.
func TestStopTreeGuards(t *testing.T) {
	snap, err := ports.Scan()
	if err != nil {
		t.Skip("no /proc:", err)
	}
	if pid, _ := core.SelfInTree(snap, []int{os.Getppid()}); pid != os.Getppid() {
		t.Errorf("parent in tree: SelfInTree = %d, want %d", pid, os.Getppid())
	}
	if pid, _ := core.SelfInTree(snap, []int{os.Getpid()}); pid != os.Getpid() {
		t.Errorf("self in tree: SelfInTree = %d", pid)
	}
	if pid, _ := core.SelfInTree(snap, []int{1 << 30}); pid != 0 {
		t.Errorf("unrelated tree: SelfInTree = %d", pid)
	}
	me, ok := ports.ReadProc(os.Getpid())
	if !ok || me.Start == 0 {
		t.Fatalf("ReadProc(self) = %+v %v", me, ok)
	}
	if !core.StillSame(me) {
		t.Error("StillSame(self) = false")
	}
	reused := me
	reused.Start--
	if core.StillSame(reused) {
		t.Error("a pid with another start time counts as the same process")
	}
}

// A setsid'd server whose parent exited is out of the tree; it still holds
// the last port. Status says so, and restart stops it.
func TestOrphanedSetsidChild(t *testing.T) {
	port := testenv.FreePort(t)
	p := testenv.NewProject(t, "processes:\n  wrap: { cmd: "+yamlCmd(testenv.Helper("--child", "--setsid", "--parent-exit=1s", listenArg(port)))+" }\n")
	if _, err := p.Engine.Up(bg, "wrap"); err != nil {
		t.Fatal(err)
	}
	waitMet(t, p, "wrap", core.WaitCondition{Port: port}) // records the port
	waitMet(t, p, "wrap", core.WaitCondition{Exit: true}) // the parent left
	if testenv.PortFree(port) {
		t.Fatal("the setsid'd child died with its parent")
	}
	st := p.Status(t)
	want := fmt.Sprintf("wrap: :%d is still held by pid", port)
	if !slices.ContainsFunc(st.Warnings, func(w string) bool {
		return strings.HasPrefix(w, want) && strings.Contains(w, "rov restart wrap stops it")
	}) {
		t.Errorf("warnings %q, want one starting %q", st.Warnings, want)
	}
	old := 0
	if snap, err := ports.Scan(); err == nil {
		for _, l := range snap.Listeners() {
			if l.Port == port {
				old = l.PID
			}
		}
	}
	if msg, err := p.Engine.Restart(bg, "wrap"); err != nil {
		t.Fatalf("restart: %q %v", msg, err)
	}
	// The new run gets the port: the old child was stopped.
	waitMet(t, p, "wrap", core.WaitCondition{Port: port})
	if old > 0 {
		if pr, ok := ports.ReadProc(old); ok && !pr.Zombie() {
			t.Errorf("old child %d still alive", old)
		}
	}
	if _, err := p.Engine.Down(bg, "wrap"); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, 5*time.Second, "the port to be free", func() bool { return testenv.PortFree(port) })
}
