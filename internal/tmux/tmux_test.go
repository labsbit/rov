package tmux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEscapeArg(t *testing.T) {
	for in, want := range map[string]string{
		"":        "",
		"a":       "a",
		";":       `\;`,
		"a;":      `a\;`,
		`a\;`:     `a\\;`,
		";;":      `;\;`,
		"a;b":     "a;b",
		"x \\; y": "x \\; y",
	} {
		if got := EscapeArg(in); got != want {
			t.Errorf("EscapeArg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	for _, s := range []string{"", "just gateway", `a$HOME\x%41`, "line1\nline2\ttab", "é日本", string(all), "100%", "%zz"} {
		enc := EncodeValue(s)
		for i := 0; i < len(enc); i++ {
			if c := enc[i]; c < 0x20 || c >= 0x7f || c == '$' || c == '\\' {
				t.Fatalf("EncodeValue(%q) = %q holds byte %#x", s, enc, c)
			}
		}
		if got := DecodeValue(enc); got != s {
			t.Errorf("DecodeValue(EncodeValue(%q)) = %q", s, got)
		}
	}
	if got := EncodeValue("just gateway"); got != "just gateway" {
		t.Errorf("plain text encoded: %q", got)
	}
	if got := DecodeValue("50%"); got != "50%" {
		t.Errorf("DecodeValue kept a lone %%? got %q", got)
	}
}

func TestCheckVersion(t *testing.T) {
	for v, ok := range map[string]bool{
		"tmux 3.4":         true,
		"tmux 3.2":         true,
		"tmux 3.3a":        true,
		"tmux next-3.5":    true,
		"tmux 4.0":         true,
		"tmux master":      true,
		"tmux openbsd-7.4": true,
		"tmux 3.1c":        false,
		"tmux 2.9":         false,
	} {
		if err := checkVersion(v); (err == nil) != ok {
			t.Errorf("checkVersion(%q) = %v, want ok=%v", v, err, ok)
		}
	}
}

func TestParseSessions(t *testing.T) {
	out := "web\t%1\t100\t0\t\t\t1700000000\t99\tadhoc\tnpm%20x\n" +
		"web\t%2\t101\t0\t\t\t1700000000\t99\tx\ty\n" + // second pane of web: skipped
		"api\t%3\t200\t1\t7\t\t1700000001\t99\t\t\n" +
		"bad\trow\n" +
		"gw\t%4\t300\t1\t\t15\t1700000002\t99\t\t\n"
	got := parseSessions(out, []string{"@k", "@c"})
	if len(got) != 3 {
		t.Fatalf("got %d sessions: %+v", len(got), got)
	}
	if s := got[0]; s.Name != "web" || s.PaneID != "%1" || s.PanePID != 100 || s.ServerPID != 99 || s.Dead || s.Option("@k") != "adhoc" || s.Option("@c") != "npm x" {
		t.Errorf("web = %+v", s)
	}
	if s := got[1]; !s.Dead || s.DeadStatus == nil || *s.DeadStatus != 7 {
		t.Errorf("api = %+v", s)
	}
	if s := got[2]; !s.Dead || s.DeadStatus != nil || s.DeadSignal != 15 || !s.Created.Equal(time.Unix(1700000002, 0)) {
		t.Errorf("gw = %+v", s)
	}
}

func TestZombieStatus(t *testing.T) {
	// A real zombie's stat: exit_code (field 52) 1280 is exit status 5.
	exited := "439965 (cli.test) Z 439925 439965 439965 0 -1 4228108 778 0 0 0 0 0 0 0 20 0 1 0 4448319 0 0 18446744073709551615 0 0 0 0 0 0 1002060288 0 2143420159 1 0 0 17 1 0 0 0 0 0 0 0 0 0 0 0 0 1280\n"
	if code, sig, ok := zombieStatus([]byte(exited), 439925); !ok || code != 5 || sig != 0 {
		t.Errorf("exited: %d %d %v", code, sig, ok)
	}
	if _, _, ok := zombieStatus([]byte(exited), 1); ok {
		t.Error("a zombie of another parent was decoded")
	}
	killed := strings.Replace(exited, " 1280\n", " 9\n", 1)
	if code, sig, ok := zombieStatus([]byte(killed), 0); !ok || code != 0 || sig != 9 {
		t.Errorf("killed: %d %d %v", code, sig, ok)
	}
	running := strings.Replace(exited, ") Z ", ") S ", 1)
	if _, _, ok := zombieStatus([]byte(running), 0); ok {
		t.Error("a live process was decoded")
	}
	weird := "12 (a) b) Z) Z 7" + strings.Repeat(" 0", 47) + " 768\n"
	if code, _, ok := zombieStatus([]byte(weird), 7); !ok || code != 3 {
		t.Errorf("comm with parens: %d %v", code, ok)
	}
}

// TestExitStatusOfQuickExits starts panes that exit quickly while other
// panes open: before Sessions reaped the zombies tmux had lost the SIGCHLD
// of (libutempter), some read as dead with no status.
func TestExitStatusOfQuickExits(t *testing.T) {
	c := newTestServer(t)
	ctx := context.Background()
	const n = 8
	for i := range n {
		cmd := fmt.Sprintf("sleep 0.0%d; exit %d", i, 10+i)
		if _, err := c.CreateSession(ctx, NewSession{Name: fmt.Sprintf("q%d", i), Command: cmd}, ServerOptions{RemainOnExit: true}); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		list, err := c.Sessions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		dead := 0
		for _, s := range list {
			if s.Dead {
				dead++
			}
		}
		if dead == n {
			for _, s := range list {
				var i int
				fmt.Sscanf(s.Name, "q%d", &i)
				if s.DeadStatus == nil || *s.DeadStatus != 10+i {
					t.Errorf("%s: status %v signal %d, want %d", s.Name, s.DeadStatus, s.DeadSignal, 10+i)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d panes died", dead, n)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// newTestServer returns a Client on a throwaway server, killed at cleanup.
func newTestServer(t *testing.T) *Client {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	b := make([]byte, 4)
	rand.Read(b)
	c := New("rov-test-" + hex.EncodeToString(b))
	t.Cleanup(func() {
		c.KillServer(context.Background())
		os.Remove(c.SocketPath())
	})
	return c
}

func TestRealServer(t *testing.T) {
	c := newTestServer(t)
	ctx := context.Background()

	list, err := c.Sessions(ctx, "@rov_cmd")
	if err != nil || list != nil {
		t.Fatalf("no server: Sessions = %v, %v; want nil, nil", list, err)
	}
	if err := c.KillSession(ctx, "nope"); err != nil {
		t.Fatalf("KillSession without server: %v", err)
	}

	pane, err := c.CreateSession(ctx, NewSession{Name: "one", Dir: t.TempDir(), Width: 220, Height: 50, Command: "exec sleep 30"},
		ServerOptions{RemainOnExit: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(pane, "%") {
		t.Fatalf("pane id %q", pane)
	}

	tricky := []string{"a;", `a\;`, ";", "-x", "$HOME x\\$y", "#{pane_id}", "tab\there\nnl", "\x01\x1f", "é日本", "{ a }", "50%"}
	for _, v := range tricky {
		if _, err := c.Run(ctx, SetOptionCmd(pane, "@rov_cmd", v)); err != nil {
			t.Fatalf("set %q: %v", v, err)
		}
		s, ok, err := c.Session(ctx, "one", "@rov_cmd", "@rov_unset")
		if err != nil || !ok {
			t.Fatalf("Session: %v %v", ok, err)
		}
		if got := s.Option("@rov_cmd"); got != v {
			t.Errorf("option round trip: set %q, got %q", v, got)
		}
		if s.Option("@rov_unset") != "" {
			t.Errorf("unset option = %q", s.Option("@rov_unset"))
		}
	}
	if _, err := c.Run(ctx, SetOptionCmd(pane, "@rov_cmd", "")); err != nil {
		t.Fatalf("unset: %v", err)
	}

	// A command that ends with ';' and env values that do too reach the pane
	// intact, and the exit status is reported.
	if _, err := c.Run(ctx, RespawnCmd(Respawn{Pane: pane, Env: []string{"V=x;"}, Command: `test "$V" = "x;" && exit 7;`})); err != nil {
		t.Fatal(err)
	}
	s := waitDead(t, c, "one")
	if s.DeadStatus == nil || *s.DeadStatus != 7 {
		t.Fatalf("dead status = %v, want 7 (session %+v)", s.DeadStatus, s)
	}
	if s.PanePID <= 0 || s.Created.IsZero() {
		t.Errorf("session = %+v", s)
	}

	// '#' in a start directory and a pipe command is not a format.
	hashDir := filepath.Join(t.TempDir(), "a#S#{session_name}")
	if err := os.Mkdir(hashDir, 0o755); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(hashDir, "out#S")
	if _, err := c.Run(ctx,
		RespawnCmd(Respawn{Pane: pane, Dir: hashDir, Command: `pwd; echo 'x#S'; sleep 0.3`}),
		PipePaneCmd(pane, "cat > "+shellQuote(out))); err != nil {
		t.Fatal(err)
	}
	waitDead(t, c, "one")
	if b, err := os.ReadFile(out); err != nil || string(b) != hashDir+"\r\nx#S\r\n" {
		t.Errorf("pipe output %q, %v; want the directory and x#S", b, err)
	}
	pane2, err := c.CreateSession(ctx, NewSession{Name: "two", Dir: hashDir, Command: "pwd > pwd.txt"}, ServerOptions{RemainOnExit: true})
	if err != nil {
		t.Fatal(err)
	}
	waitDead(t, c, "two")
	if b, _ := os.ReadFile(filepath.Join(hashDir, "pwd.txt")); strings.TrimSpace(string(b)) != hashDir {
		t.Errorf("new-session -c: pwd %q, want %q (pane %s)", b, hashDir, pane2)
	}

	// pipe-pane refuses a dead pane.
	if _, err := c.Run(ctx, PipePaneCmd(pane, "cat >/dev/null")); err == nil {
		t.Error("pipe-pane on a dead pane succeeded")
	}

	// A cancelled caller is not a tmux timeout.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := c.Run(cctx, Cmd{"list-sessions"}); !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "timed out") {
		t.Errorf("cancelled Run: %v", err)
	}

	if err := c.KillSession(ctx, "one"); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := c.Session(ctx, "one"); ok || err != nil {
		t.Fatalf("after kill: ok=%v err=%v", ok, err)
	}
	if _, err := c.Run(ctx, Cmd{"kill-session", "-t", "=nope"}); !errors.Is(err, ErrNoServer) && !errors.Is(err, ErrNoSession) {
		t.Errorf("kill missing session: %v", err)
	}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func waitDead(t *testing.T, c *Client, name string) Session {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s, ok, err := c.Session(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		if ok && s.Dead {
			return s
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never died: %+v", name, s)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestAttachArgs(t *testing.T) {
	c := New("rov-x")
	t.Setenv("TMUX", "")
	if got := strings.Join(c.AttachArgs("web"), " "); got != "-L rov-x attach-session -t =web" {
		t.Errorf("outside tmux: %s", got)
	}
	t.Setenv("TMUX", c.SocketPath()+",123,0")
	if got := strings.Join(c.AttachArgs("web"), " "); got != "-L rov-x switch-client -t =web" {
		t.Errorf("inside the same server: %s", got)
	}
	t.Setenv("TMUX", "/tmp/tmux-1/default,123,0")
	if got := strings.Join(c.AttachArgs("web"), " "); got != "-L rov-x attach-session -t =web" {
		t.Errorf("inside another server: %s", got)
	}
}
