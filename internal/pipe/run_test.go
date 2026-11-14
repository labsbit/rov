package pipe

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		return nil
	}
	if b[len(b)-1] != '\n' {
		t.Fatalf("%s: last line unterminated", path)
	}
	return strings.Split(string(b[:len(b)-1]), "\n")
}

func runAll(t *testing.T, opt Options, input string) {
	t.Helper()
	if err := Run(context.Background(), opt, strings.NewReader(input)); err != nil {
		t.Fatal(err)
	}
}

func TestRunWritesBothFiles(t *testing.T) {
	dir := t.TempDir()
	runAll(t, Options{Dir: dir, Name: "web"}, "\x1b[32mready\x1b[0m in 12 ms\r\n\x1b]0;vite\x07plain\nprompt> ")
	p := Paths(dir, "web")
	if got, want := readLines(t, p.Log), []string{"ready in 12 ms", "plain", "prompt> "}; !equal(got, want) {
		t.Errorf(".log = %q, want %q", got, want)
	}
	if got, want := readLines(t, p.ANSI), []string{"\x1b[32mready\x1b[0m in 12 ms", "plain", "prompt> "}; !equal(got, want) {
		t.Errorf(".ansi = %q, want %q", got, want)
	}
}

func TestRunAppends(t *testing.T) {
	dir := t.TempDir()
	runAll(t, Options{Dir: dir, Name: "a"}, "one\n")
	runAll(t, Options{Dir: dir, Name: "a"}, "two\n")
	if got := readLines(t, Paths(dir, "a").Log); !equal(got, []string{"one", "two"}) {
		t.Errorf("got %q", got)
	}
}

func TestRunInvalidName(t *testing.T) {
	for _, name := range []string{"", ".lock", "../x", "a/b", "a\x00"} {
		if err := Run(context.Background(), Options{Dir: t.TempDir(), Name: name}, strings.NewReader("")); err == nil {
			t.Errorf("%q: want error", name)
		}
	}
}

// waitFor polls until cond holds or fails the test after 2 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunPartialFlush(t *testing.T) {
	dir := t.TempDir()
	p := Paths(dir, "rov")
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{Dir: dir, Name: "rov", FlushAfter: 20 * time.Millisecond}, pr)
	}()

	io.WriteString(pw, "line\n")
	waitFor(t, "complete line", func() bool { return len(readLines(t, p.Log)) == 1 })

	start := time.Now()
	io.WriteString(pw, "Continue? [y/N] ")
	waitFor(t, "flushed prompt", func() bool { return len(readLines(t, p.Log)) == 2 })
	if d := time.Since(start); d < 15*time.Millisecond {
		t.Errorf("flushed after %v, before FlushAfter", d)
	}
	if got := readLines(t, p.ANSI); len(got) != 2 || got[1] != "Continue? [y/N] " {
		t.Errorf(".ansi = %q", got)
	}

	io.WriteString(pw, "y\r\nbye")
	pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	want := []string{"line", "Continue? [y/N] ", "y", "bye"}
	if got := readLines(t, p.Log); !equal(got, want) {
		t.Errorf(".log = %q, want %q", got, want)
	}
	if got := readLines(t, p.ANSI); !equal(got, want) {
		t.Errorf(".ansi = %q, want %q", got, want)
	}
}

func TestRunContextCancelFlushes(t *testing.T) {
	dir := t.TempDir()
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, Options{Dir: dir, Name: "x", FlushAfter: time.Hour}, pr) }()
	io.WriteString(pw, "partial")
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The write may or may not have been read before cancel; whatever was
	// read is flushed, and the files agree.
	l, a := readLines(t, Paths(dir, "x").Log), readLines(t, Paths(dir, "x").ANSI)
	if !equal(l, a) || len(l) > 1 {
		t.Errorf(".log %q, .ansi %q", l, a)
	}
}

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	p := Paths(dir, "r")
	const max = 300
	var in strings.Builder
	var want []string
	for i := range 200 {
		fmt.Fprintf(&in, "\x1b[3%dmline %d\x1b[0m\r\n", i%8, i)
		want = append(want, fmt.Sprintf("line %d", i))
	}
	runAll(t, Options{Dir: dir, Name: "r", Max: max}, in.String())

	log1, log := readLines(t, p.Log1), readLines(t, p.Log)
	ansi1, ansi := readLines(t, p.ANSI1), readLines(t, p.ANSI)
	if len(log1) == 0 || len(log) == 0 {
		t.Fatalf("expected rotation: .log.1 %d lines, .log %d lines", len(log1), len(log))
	}
	if len(log1) != len(ansi1) || len(log) != len(ansi) {
		t.Fatalf("misaligned: .log.1 %d / .ansi.1 %d, .log %d / .ansi %d", len(log1), len(ansi1), len(log), len(ansi))
	}
	for i := range log {
		if stripSGR(ansi[i]) != log[i] {
			t.Errorf(".ansi line %d %q != .log %q", i, ansi[i], log[i])
		}
	}
	for i := range log1 {
		if stripSGR(ansi1[i]) != log1[i] {
			t.Errorf(".ansi.1 line %d %q != .log.1 %q", i, ansi1[i], log1[i])
		}
	}
	got := append(log1, log...)
	if !equal(got, want[len(want)-len(got):]) {
		t.Errorf(".log.1+.log is not the tail of the input: %q", got)
	}
	for _, f := range []string{p.Log, p.ANSI, p.Log1, p.ANSI1} {
		if fi, err := os.Stat(f); err != nil || fi.Size() > max {
			t.Errorf("%s: %v, size over cap", filepath.Base(f), err)
		}
	}
}

func TestRotationLineOverCap(t *testing.T) {
	dir := t.TempDir()
	p := Paths(dir, "r")
	runAll(t, Options{Dir: dir, Name: "r", Max: 10}, "a\n0123456789abcdef\nb\n")
	if got := readLines(t, p.Log1); !equal(got, []string{"0123456789abcdef"}) {
		t.Errorf(".log.1 = %q", got)
	}
	if got := readLines(t, p.Log); !equal(got, []string{"b"}) {
		t.Errorf(".log = %q", got)
	}
	if l, a := readLines(t, p.Log1), readLines(t, p.ANSI1); len(l) != len(a) {
		t.Errorf("misaligned .1: %d vs %d", len(l), len(a))
	}
}

func writeFile(t *testing.T, path, s string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareRun(t *testing.T) {
	t.Run("moves current to prev", func(t *testing.T) {
		dir := t.TempDir()
		p := Paths(dir, "api")
		writeFile(t, p.Log, "cur\n")
		writeFile(t, p.ANSI, "\x1b[31mcur\x1b[0m\n")
		writeFile(t, p.Log1, "old\n")
		writeFile(t, p.ANSI1, "old\n")
		writeFile(t, p.PrevLog, "older\n")
		writeFile(t, p.PrevANSI, "older\n")
		if err := PrepareRun(dir, "api"); err != nil {
			t.Fatal(err)
		}
		if got := readLines(t, p.PrevLog); !equal(got, []string{"cur"}) {
			t.Errorf("prev.log = %q", got)
		}
		if got := readLines(t, p.PrevANSI); !equal(got, []string{"\x1b[31mcur\x1b[0m"}) {
			t.Errorf("prev.ansi = %q", got)
		}
		for _, f := range []string{p.Log1, p.ANSI1} {
			if exists(f) {
				t.Errorf("%s still exists", f)
			}
		}
		for _, f := range []string{p.Log, p.ANSI} {
			if fi, err := os.Stat(f); err != nil || fi.Size() != 0 {
				t.Errorf("%s: want empty file, got %v", f, err)
			}
		}
	})
	t.Run("only plain current", func(t *testing.T) {
		dir := t.TempDir()
		p := Paths(dir, "api")
		writeFile(t, p.Log, "cur\n") // written by the bash script: no .ansi
		writeFile(t, p.PrevANSI, "stale\n")
		if err := PrepareRun(dir, "api"); err != nil {
			t.Fatal(err)
		}
		if exists(p.PrevANSI) {
			t.Error("stale prev.ansi kept next to a newer prev.log")
		}
		if got := readLines(t, p.PrevLog); !equal(got, []string{"cur"}) {
			t.Errorf("prev.log = %q", got)
		}
	})
	t.Run("nothing to move keeps prev", func(t *testing.T) {
		dir := t.TempDir()
		p := Paths(dir, "api")
		writeFile(t, p.PrevLog, "crash\n")
		writeFile(t, p.PrevANSI, "crash\n")
		if err := PrepareRun(dir, "api"); err != nil {
			t.Fatal(err)
		}
		if got := readLines(t, p.PrevLog); !equal(got, []string{"crash"}) {
			t.Errorf("prev.log = %q", got)
		}
	})
	t.Run("creates dir", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "a", ".logs")
		if err := PrepareRun(dir, "x"); err != nil {
			t.Fatal(err)
		}
		if !exists(Paths(dir, "x").Log) {
			t.Error("no current log")
		}
	})
	t.Run("invalid name", func(t *testing.T) {
		if err := PrepareRun(t.TempDir(), "../x"); err == nil {
			t.Error("want error")
		}
	})
}

// TestOldWriterAfterPrepareRun is the restart race: the old writer still
// runs while PrepareRun moves its files and a new writer starts.
func TestOldWriterAfterPrepareRun(t *testing.T) {
	dir := t.TempDir()
	p := Paths(dir, "rov")
	const max = 200
	oldR, oldW := io.Pipe()
	oldDone := make(chan error, 1)
	go func() { oldDone <- Run(context.Background(), Options{Dir: dir, Name: "rov", Max: max}, oldR) }()
	io.WriteString(oldW, "old 1\n")
	waitFor(t, "old line", func() bool { return len(readLines(t, p.Log)) == 1 })

	if err := PrepareRun(dir, "rov"); err != nil {
		t.Fatal(err)
	}
	newR, newW := io.Pipe()
	newDone := make(chan error, 1)
	go func() { newDone <- Run(context.Background(), Options{Dir: dir, Name: "rov", Max: max}, newR) }()

	// The dying old process prints well past the cap.
	for i := range 50 {
		fmt.Fprintf(oldW, "old tail %d\n", i)
	}
	io.WriteString(newW, "new 1\n")
	oldW.Close()
	newW.Close()
	if err := <-oldDone; err != nil {
		t.Fatal(err)
	}
	if err := <-newDone; err != nil {
		t.Fatal(err)
	}

	if got := readLines(t, p.Log); !equal(got, []string{"new 1"}) {
		t.Errorf("new run .log = %q", got)
	}
	if got := readLines(t, p.ANSI); !equal(got, []string{"new 1"}) {
		t.Errorf("new run .ansi = %q", got)
	}
	if exists(p.Log1) || exists(p.ANSI1) {
		t.Error("old writer rotated the new run")
	}
	prev, prevA := readLines(t, p.PrevLog), readLines(t, p.PrevANSI)
	if len(prev) < 2 || prev[0] != "old 1" || prev[1] != "old tail 0" || !equal(prev, prevA) {
		t.Errorf("prev = %q / %q", prev, prevA)
	}
	if fi, _ := os.Stat(p.PrevLog); fi.Size() > max {
		t.Errorf("detached writer grew prev past the cap: %d", fi.Size())
	}
}

// TestRotateRacesPrepareRun: PrepareRun landing between a writer's check
// that it still owns the files and its renames must not let the old writer
// rotate the new run's files and write into them.
func TestRotateRacesPrepareRun(t *testing.T) {
	dir := t.TempDir()
	p := Paths(dir, "rov")
	w, err := openPair(dir, "rov", 20)
	if err != nil {
		t.Fatal(err)
	}
	prepared := make(chan error, 1)
	testHookRotate = func() {
		testHookRotate = nil
		go func() { prepared <- PrepareRun(dir, "rov") }()
		// Give PrepareRun the time to run in the window, if nothing stops it.
		select {
		case err := <-prepared:
			prepared <- err
		case <-time.After(300 * time.Millisecond):
		}
	}
	defer func() { testHookRotate = nil }()

	w.line([]byte("old 1 ......."), []byte("old 1 ......."))
	w.line([]byte("old 2 ......."), []byte("old 2 .......")) // rotates
	select {
	case err := <-prepared:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no rotation, or PrepareRun never finished")
	}
	w.line([]byte("old 3"), []byte("old 3"))
	if err := w.close(); err != nil {
		t.Fatal(err)
	}

	for _, f := range []string{p.Log, p.ANSI} {
		if got := readLines(t, f); len(got) != 0 {
			t.Errorf("the new run's %s holds the old writer's %q", filepath.Base(f), got)
		}
	}
	if got := readLines(t, p.PrevLog); !equal(got, []string{"old 2 .......", "old 3"}) {
		t.Errorf("prev.log = %q", got)
	}
}
