package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labsbit/rov/internal/core"
	"github.com/labsbit/rov/internal/testenv"
)

func TestParseArgs(t *testing.T) {
	defs := []flagDef{{names: []string{"-n", "--lines"}, value: true}, {names: []string{"-f"}}, {names: []string{"--env"}, value: true}}
	a, err := parseCmdArgs([]string{"gateway", "-n", "200", "-f", "--env=A=1", "--env", "B=2", "--", "x", "-n", "--y"}, defs...)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(a.pos, []string{"gateway"}) || a.str("-n") != "200" || !a.has("-f") || !slices.Equal(a.all("--env"), []string{"A=1", "B=2"}) {
		t.Errorf("parsed %+v", a)
	}
	if !a.dashdash || !slices.Equal(a.rest, []string{"x", "-n", "--y"}) {
		t.Errorf("rest = %q", a.rest)
	}
	// Without a command line, what follows -- is positional, not dropped.
	if b, err := parseArgs([]string{"-f", "--", "web", "-n"}, defs...); err != nil || !slices.Equal(b.pos, []string{"web", "-n"}) || b.has("-n") {
		t.Errorf("parseArgs after --: %+v %v", b, err)
	}
	if n, _ := a.int("--lines", 0); n != 0 {
		t.Errorf("int of an alias key: %d (keys are the first name)", n)
	}
	for _, bad := range [][]string{{"--nope"}, {"-n"}, {"-f=1"}} {
		if _, err := parseArgs(bad, defs...); err == nil {
			t.Errorf("parseArgs(%q) succeeded", bad)
		}
	}
	if d, err := parseDuration("90"); err != nil || d != 90*time.Second {
		t.Errorf("parseDuration(90) = %v %v", d, err)
	}
	if d, err := parseDuration("2m"); err != nil || d != 2*time.Minute {
		t.Errorf("parseDuration(2m) = %v %v", d, err)
	}
}

func TestPipeEndToEndWithBrokenConfig(t *testing.T) {
	root := t.TempDir()
	// Broken YAML: __pipe falls back to the defaults and still writes.
	os.WriteFile(filepath.Join(root, "rov.yaml"), []byte("log_dir: [oops\n"), 0o644)
	var stderr bytes.Buffer
	code := runPipe([]string{root, "web"}, strings.NewReader("\x1b[32mok\x1b[0m line\r\nprogress 10%\rprogress 100%\n"), &stderr)
	if code != 0 {
		t.Fatalf("code %d: %s", code, stderr.String())
	}
	plain, _ := os.ReadFile(filepath.Join(root, ".logs", "web.log"))
	ansi, _ := os.ReadFile(filepath.Join(root, ".logs", "web.ansi"))
	if string(plain) != "ok line\nprogress 100%\n" {
		t.Errorf(".log = %q", plain)
	}
	if !strings.Contains(string(ansi), "\x1b[32mok\x1b[0m line") {
		t.Errorf(".ansi = %q", ansi)
	}
	// Bad arguments: still drains, never fails.
	if code := runPipe(nil, strings.NewReader(strings.Repeat("x", 1<<20)), &stderr); code != 0 {
		t.Errorf("bad args: code %d", code)
	}
	if code := runPipe([]string{root, "../escape"}, strings.NewReader("data\n"), &stderr); code != 0 {
		t.Errorf("bad name: code %d", code)
	}
}

// run executes one CLI command line against p and returns the exit code
// and output.
func run(t *testing.T, p *testenv.Project, argv ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	a := &App{
		Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""),
		Getwd: func() (string, error) { return p.Dir, nil },
		Open:  func(string) (*core.Engine, error) { return p.Engine, nil },
		Loc:   time.UTC,
	}
	code := a.Run(argv)
	return code, out.String(), errb.String()
}

func TestCLIEndToEnd(t *testing.T) {
	port := testenv.FreePort(t)
	p := testenv.NewProject(t, "processes:\n"+
		"  web: { cmd: '"+testenv.Helper("--listen=127.0.0.1:"+strconv.Itoa(port))+"' }\n"+
		"  boom: { cmd: '"+testenv.Helper("--crash=5", "--after=300ms")+"' }\n")

	expect := func(wantCode int, argv ...string) (string, string) {
		t.Helper()
		code, out, errs := run(t, p, argv...)
		if code != wantCode {
			t.Fatalf("rov %s: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", strings.Join(argv, " "), code, wantCode, out, errs)
		}
		return out, errs
	}

	expect(ExitUsage, "bogus")
	expect(ExitUsage, "logs")
	expect(ExitUsage, "logs", "web", "--nope")
	expect(ExitFailed, "up", "web", "typo") // validated before anything runs
	if pr := p.Proc(t, "web"); pr.HasSession {
		t.Fatal("a typo in the second name started the first")
	}
	expect(ExitNotRunning, "send", "web", "hi")
	expect(ExitNotRunning, "wait", "web", "--timeout", "1s")

	out, _ := expect(ExitOK, "up", "web", "boom")
	if out != "web: started\nboom: started\n" {
		t.Errorf("up output %q", out)
	}
	out, _ = expect(ExitOK, "wait", "web")
	if out != "web: listening on :"+strconv.Itoa(port)+"\n" {
		t.Errorf("wait output %q", out)
	}
	out, _ = expect(ExitOK, "up", "web")
	if out != "web: already running\n" {
		t.Errorf("second up %q", out)
	}
	// A crash before the condition: exit 3 and the log tail.
	out, errs := expect(ExitNotRunning, "wait", "boom", "--port", "1")
	if !strings.Contains(errs, "boom: crashed(5) before port 1") || !strings.Contains(out, "crashing with 5") || !strings.Contains(out, "── last ") {
		t.Errorf("wait on a crash:\n%s\n%s", out, errs)
	}
	// Timeout: exit 1 and the log tail.
	out, errs = expect(ExitFailed, "wait", "web", "--log", "never printed", "--timeout", "400ms")
	if !strings.Contains(errs, "timed out after 400ms") || !strings.Contains(out, "listening on") {
		t.Errorf("wait timeout:\n%s\n%s", out, errs)
	}

	out, _ = expect(ExitOK, "status")
	if !strings.Contains(out, "web          running") || !strings.Contains(out, strconv.Itoa(port)) || !strings.Contains(out, "crashed(5)") {
		t.Errorf("status:\n%s", out)
	}
	out, _ = expect(ExitOK, "status", "--json")
	var st struct {
		Processes []struct {
			Name     string `json:"name"`
			State    string `json:"state"`
			ExitCode *int   `json:"exit_code"`
		} `json:"processes"`
	}
	if err := json.Unmarshal([]byte(out), &st); err != nil || len(st.Processes) != 2 || st.Processes[1].State != "crashed" || *st.Processes[1].ExitCode != 5 {
		t.Errorf("status --json: %v\n%s", err, out)
	}

	expect(ExitOK, "restart", "boom")
	out, _ = expect(ExitOK, "logs", "boom", "--prev", "--grep", "crash")
	if out != "crashing with 5\n" {
		t.Errorf("logs --prev --grep: %q", out)
	}
	out, _ = expect(ExitOK, "logs", "web", "-n", "1", "--json")
	if !strings.Contains(out, `"run": "current"`) || !strings.Contains(out, `"state": "running"`) {
		t.Errorf("logs --json: %s", out)
	}
	out, _ = expect(ExitOK, "show", "web", "-n", "2")
	if !strings.Contains(out, "state        running") || !strings.Contains(out, "── last 2 lines") {
		t.Errorf("show:\n%s", out)
	}
	out, _ = expect(ExitOK, "ports")
	if !strings.Contains(out, strconv.Itoa(port)) || !strings.Contains(out, " web") {
		t.Errorf("ports:\n%s", out)
	}

	// spawn: flags, --wait, -- handling.
	port2 := testenv.FreePort(t)
	out, _ = expect(ExitOK, "spawn", "mock", "--env", "A=1", "--wait", "--", testenv.Helper("--listen=127.0.0.1:"+strconv.Itoa(port2)))
	if !strings.Contains(out, "mock: started\nmock: listening on :"+strconv.Itoa(port2)) {
		t.Errorf("spawn --wait: %q", out)
	}
	expect(ExitUsage, "spawn", "mock2", "echo", "hi") // no --
	expect(ExitFailed, "spawn", "mock", "--", "true") // running, no --replace
	expect(ExitFailed, "spawn", "web", "--", "true")  // a config name
	spawnCwd(t, p)
	out, _ = expect(ExitOK, "list")
	if !strings.Contains(out, "ad-hoc\nmock ") {
		t.Errorf("list:\n%s", out)
	}
	expect(ExitOK, "rm", "mock", "--purge")

	// config edits through the shared writer.
	expect(ExitOK, "config", "add", "extra", "--after", "web", "--desc", "an extra", "--", "echo", "hi there")
	cfg, _ := os.ReadFile(filepath.Join(p.Dir, "rov.yaml"))
	if !strings.Contains(string(cfg), "extra:") || !strings.Contains(string(cfg), "echo 'hi there'") {
		t.Errorf("rov.yaml after add:\n%s", cfg)
	}
	expect(ExitOK, "config", "mv", "extra", "--first")
	expect(ExitOK, "config", "set", "extra", "--env", "K=v", "--desc", "")
	expect(ExitFailed, "config", "rm", "web") // running
	expect(ExitOK, "config", "rm", "web", "--stop")
	if got := p.Engine.Config().Names(); !slices.Equal(got, []string{"extra", "boom"}) {
		t.Errorf("config names = %v", got)
	}
	if p.Status(t).Get("web") != nil {
		t.Error("web lingers as ad-hoc after config rm")
	}
	// Names after -- are names, not dropped.
	if out, _ := expect(ExitOK, "stop", "--", "boom"); !strings.HasPrefix(out, "boom: ") {
		t.Errorf("stop -- boom: %q", out)
	}
	expect(ExitUsage, "show", "boom", "--", "extra")
	// down all goes bottom up: boom (last in rov.yaml) before extra.
	if out, _ := expect(ExitOK, "down", "all"); !strings.HasPrefix(out, "boom: ") || !strings.Contains(out, "\nextra: ") {
		t.Errorf("down all order:\n%s", out)
	}
}

// `rov restart self` run inside self's own pane (an agent in the pane)
// can't stop the tree it runs in: it hands the restart to a detached rov,
// which does it once the first one has exited.
func TestRestartFromInsideItsPane(t *testing.T) {
	bin := testenv.RovBin(t)
	p := testenv.NewProjectOwnSocket(t, "processes:\n  self: { cmd: 'true' }\n")
	script := "if [ -e second ]; then echo second run; exec sleep 60; fi; touch second; echo first run; " +
		shellQuote(bin) + " -C " + shellQuote(p.Dir) + " restart self; echo rov exit $?; sleep 60"
	p.WriteConfig(t, "processes:\n  self: { cmd: "+yamlQuote(script)+" }\n")
	if code, out, errs := run(t, p, "up", "self"); code != 0 {
		t.Fatalf("up: %d %s %s", code, out, errs)
	}
	testenv.Eventually(t, 20*time.Second, "the second run", func() bool { return strings.Contains(p.Log("self"), "second run") })
	prev, _ := os.ReadFile(p.Engine.LogPaths("self").PrevLog)
	if !strings.Contains(string(prev), "self: this rov runs inside its process tree") || !strings.Contains(string(prev), "rov exit 0") {
		t.Errorf("first run's log:\n%s", prev)
	}
	detached, _ := os.ReadFile(filepath.Join(p.Engine.LockDir(), "detached", "self.log"))
	if !strings.Contains(string(detached), "self: restarted") {
		t.Errorf("detached rov's log:\n%s", detached)
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func yamlQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// spawnCwd: --cwd relative to the root or absolute inside it, stored
// relative; without it the caller's cwd, which must be inside the root.
func spawnCwd(t *testing.T, p *testenv.Project) {
	t.Helper()
	sub := filepath.Join(p.Dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	spawn := func(wd string, want int, args ...string) string {
		t.Helper()
		var out, errb bytes.Buffer
		a := &App{
			Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""),
			Getwd: func() (string, error) { return wd, nil },
			Open:  func(string) (*core.Engine, error) { return p.Engine, nil },
			Loc:   time.UTC,
		}
		if code := a.Run(append([]string{"spawn"}, args...)); code != want {
			t.Fatalf("spawn %q from %s: exit %d, want %d\n%s%s", args, wd, code, want, out.String(), errb.String())
		}
		return errb.String()
	}
	cwdOf := func(name string) string {
		t.Helper()
		pr := p.Proc(t, name)
		_, _ = p.Engine.Rm(context.Background(), name, true)
		return pr.Cwd
	}
	spawn(p.Dir, ExitOK, "c1", "--cwd", sub, "--", "sleep 30")
	if got := cwdOf("c1"); got != "sub" {
		t.Errorf("--cwd <absolute inside>: cwd %q, want sub", got)
	}
	spawn(outside, ExitOK, "c2", "--cwd", "sub", "--", "sleep 30")
	if got := cwdOf("c2"); got != "sub" {
		t.Errorf("--cwd sub from outside: cwd %q, want sub", got)
	}
	spawn(sub, ExitOK, "c3", "--", "sleep 30")
	if got := cwdOf("c3"); got != "sub" {
		t.Errorf("no --cwd from sub: cwd %q, want sub", got)
	}
	spawn(p.Dir, ExitOK, "c4", "--", "sleep 30")
	if got := cwdOf("c4"); got != "." {
		t.Errorf("no --cwd from the root: cwd %q, want .", got)
	}
	if e := spawn(outside, ExitFailed, "c5", "--", "sleep 30"); !strings.Contains(e, "is outside the project root") || !strings.Contains(e, "pass --cwd") {
		t.Errorf("no --cwd from outside: %q", e)
	}
	if e := spawn(p.Dir, ExitFailed, "c6", "--cwd", outside, "--", "sleep 30"); !strings.Contains(e, "outside the project root") {
		t.Errorf("--cwd <absolute outside>: %q", e)
	}
	spawn(p.Dir, ExitFailed, "c7", "--cwd", "../x", "--", "sleep 30")
	if pr := p.Status(t).Get("c5"); pr != nil {
		t.Errorf("a refused spawn left a process: %+v", pr)
	}
}
