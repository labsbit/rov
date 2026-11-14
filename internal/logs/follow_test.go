package logs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/labsbit/rov/internal/pipe"
)

func init() { pollInterval = 5 * time.Millisecond }

// follow starts Follow in the background; stop cancels it and checks it
// ended with context.Canceled.
func follow(t *testing.T, q Query) (lines <-chan string, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan string, 1000)
	done := make(chan error, 1)
	go func() { done <- Follow(ctx, q, out) }()
	return out, func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Follow returned %v", err)
		}
	}
}

// expect reads exactly want from lines, then checks nothing else arrives
// for a few poll intervals.
func expect(t *testing.T, lines <-chan string, want ...string) {
	t.Helper()
	var got []string
	timeout := time.After(2 * time.Second)
	for len(got) < len(want) {
		select {
		case l := <-lines:
			got = append(got, l)
		case <-timeout:
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	select {
	case l := <-lines:
		got = append(got, l)
	case <-time.After(30 * time.Millisecond):
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestFollow(t *testing.T) {
	dir := t.TempDir()
	p := pipe.Paths(dir, "web")
	write(t, p.Log1, "r1\nr2\n")
	write(t, p.Log, "a\nb\npart")
	lines, stop := follow(t, Query{Dir: dir, Name: "web", N: 3})
	defer stop()
	expect(t, lines, "r2", "a", "b")

	appendTo(t, p.Log, "ial\nc\n")
	expect(t, lines, "partial", "c")

	// Rotation: a last line in the old file, then a new file.
	appendTo(t, p.Log, "d\n")
	if err := os.Rename(p.Log, p.Log1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // the path is briefly missing
	write(t, p.Log, "e\n")
	expect(t, lines, "d", "e")

	// Restart: PrepareRun moves the run to prev and starts an empty file.
	appendTo(t, p.Log, "f\n")
	if err := pipe.PrepareRun(dir, "web"); err != nil {
		t.Fatal(err)
	}
	appendTo(t, p.Log, "new 1\n")
	expect(t, lines, "f", "new 1")

	// Truncated in place.
	write(t, p.Log, "")
	time.Sleep(20 * time.Millisecond)
	appendTo(t, p.Log, "t\n")
	expect(t, lines, "t")
}

func TestFollowWaitsForFile(t *testing.T) {
	dir := t.TempDir()
	lines, stop := follow(t, Query{Dir: dir, Name: "late", N: 10})
	defer stop()
	time.Sleep(20 * time.Millisecond)
	write(t, pipe.Paths(dir, "late").Log, "hello\n")
	expect(t, lines, "hello")
}

func TestFollowGrepANSI(t *testing.T) {
	dir := t.TempDir()
	p := pipe.Paths(dir, "g")
	write(t, p.ANSI, "\x1b[31merr\x1b[0;1mor\x1b[0m 1\nok\n")
	write(t, p.Log, "error 1\nok\n")
	lines, stop := follow(t, Query{Dir: dir, Name: "g", N: 5, ANSI: true, Grep: regexp.MustCompile(`error`)})
	defer stop()
	expect(t, lines, "\x1b[31merr\x1b[0;1mor\x1b[0m 1")
	appendTo(t, p.ANSI, "fine\n\x1b[33merror 2\x1b[0m\n")
	expect(t, lines, "\x1b[33merror 2\x1b[0m")
}

// FollowNumbered reports how many lines come before the initial tail,
// across the rotated file, and with a filter only the matching ones.
func TestFollowNumbered(t *testing.T) {
	dir := t.TempDir()
	p := pipe.Paths(dir, "n")
	write(t, p.Log1, "a1\nb2\na3\n")
	write(t, p.Log, "a4\nb5\na6\npartial")
	for _, tc := range []struct {
		q    Query
		want int
		tail []string
	}{
		{Query{Dir: dir, Name: "n", N: 2}, 4, []string{"b5", "a6"}},
		{Query{Dir: dir, Name: "n", N: 100}, 0, []string{"a1", "b2", "a3", "a4", "b5", "a6"}},
		{Query{Dir: dir, Name: "n", N: 1, Grep: regexp.MustCompile(`^a`)}, 3, []string{"a6"}},
		{Query{Dir: dir, Name: "n"}, 6, nil},
		{Query{Dir: dir, Name: "missing", N: 5}, 0, nil},
	} {
		ctx, cancel := context.WithCancel(context.Background())
		out := make(chan string, 100)
		got := -1
		done := make(chan error, 1)
		go func() { done <- FollowNumbered(ctx, tc.q, out, func(n int) { got = n }) }()
		expect(t, out, tc.tail...)
		cancel()
		<-done
		if got != tc.want {
			t.Errorf("%+v: first = %d, want %d", tc.q, got, tc.want)
		}
	}
}

// TestFollowWriterRotation follows a real pipe writer through many
// rotations (up to two per poll) and a restart, and checks every line
// arrives once, in order.
func TestFollowWriterRotation(t *testing.T) {
	dir := t.TempDir()
	if err := pipe.PrepareRun(dir, "w"); err != nil {
		t.Fatal(err)
	}
	// N: the follower may only start once the first lines are written (and
	// rotated); its initial tail then delivers them.
	lines, stop := follow(t, Query{Dir: dir, Name: "w", N: 1000})
	defer stop()

	// Each .ansi line is 18 bytes: a 64-byte cap rotates every 3 lines, so
	// a batch of 5 rotates at most twice.
	run := func(from, to int) {
		pr, pw := io.Pipe()
		done := make(chan error, 1)
		go func() { done <- pipe.Run(context.Background(), pipe.Options{Dir: dir, Name: "w", Max: 64}, pr) }()
		for i := from; i <= to; i += 5 {
			for j := i; j < i+5; j++ {
				fmt.Fprintf(pw, "\x1b[32mline %d\x1b[0m\r\n", j)
			}
			expect(t, lines, seq(i, i+4, "line %d")...)
		}
		pw.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	run(10, 59)
	if err := pipe.PrepareRun(dir, "w"); err != nil {
		t.Fatal(err)
	}
	run(60, 99)
}
