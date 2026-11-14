package lock

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcquireCreatesParents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", ".lock", "config")
	release, err := Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	release()
	release() // idempotent
}

func TestTryAcquire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway")
	release, err := TryAcquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := TryAcquire(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("second TryAcquire: got %v, want ErrLocked", err)
	}
	release()
	release2, err := TryAcquire(path)
	if err != nil {
		t.Fatalf("TryAcquire after release: %v", err)
	}
	release2()
}

func TestAcquireTimesOut(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway")
	release, err := Acquire(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = Acquire(ctx, path)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("timeout took %v", d)
	}
}

// TestAcquireSerialises runs several holders against one lock file and checks
// that no two are ever inside the critical section at the same time, and that
// a waiter gets the lock once the holder releases it.
func TestAcquireSerialises(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	const workers = 8
	var inside, maxInside, done atomic.Int32
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			release, err := Acquire(ctx, path)
			if err != nil {
				t.Error(err)
				return
			}
			n := inside.Add(1)
			for {
				m := maxInside.Load()
				if n <= m || maxInside.CompareAndSwap(m, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			inside.Add(-1)
			done.Add(1)
			release()
		})
	}
	wg.Wait()
	if got := maxInside.Load(); got != 1 {
		t.Fatalf("max holders at once = %d, want 1", got)
	}
	if got := done.Load(); got != workers {
		t.Fatalf("%d of %d workers got the lock", got, workers)
	}
}
