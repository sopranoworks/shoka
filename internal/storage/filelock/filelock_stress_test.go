package filelock

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStress_NoDeadlockNoLeak fires many goroutines at random paths and asserts
// the manager neither deadlocks nor leaks goroutines. Run under -race.
func TestStress_NoDeadlockNoLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	m := NewManager(Config{MaxLeaseDuration: time.Second, ReaperInterval: 10 * time.Millisecond})

	const goroutines = 400
	const opsEach = 25
	const distinctPaths = 32

	var ops atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < opsEach; i++ {
				path := fmt.Sprintf("file-%d.md", (seed*7+i*13)%distinctPaths)
				ctx := context.Background()
				// Occasionally use a short-lived ctx that may cancel mid-wait.
				if i%5 == 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(context.Background(), time.Millisecond)
					_ = m.WithLock(ctx, fmt.Sprintf("sess-%d", seed), path, func() error {
						ops.Add(1)
						return nil
					})
					cancel()
					continue
				}
				_ = m.WithLock(ctx, fmt.Sprintf("sess-%d", seed), path, func() error {
					ops.Add(1)
					return nil
				})
			}
		}(g)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("stress run did not complete; possible deadlock")
	}

	m.Stop()
	if got := ops.Load(); got == 0 {
		t.Fatal("no operations recorded")
	}

	// Allow background acquisition/cleanup goroutines to drain.
	var after int
	for i := 0; i < 50; i++ {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
		after = runtime.NumGoroutine()
		if after <= before+5 {
			break
		}
	}
	if after > before+10 {
		t.Fatalf("goroutine leak: before=%d after=%d", before, after)
	}
}
