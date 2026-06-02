package filelock

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestWithLocks_MutualExclusionOnSharedPath verifies that WithLocks excludes a
// concurrent WithLock on a path in its set: the single-path acquirer cannot enter
// until the multi-lock holder releases.
func TestWithLocks_MutualExclusionOnSharedPath(t *testing.T) {
	m := NewManager(Config{})
	defer m.Stop()

	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = m.WithLocks(context.Background(), "", []string{"a", "b", "c"}, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	entered := make(chan struct{})
	go func() {
		_ = m.WithLock(context.Background(), "", "b", func() error {
			close(entered)
			return nil
		})
	}()

	select {
	case <-entered:
		t.Fatal("WithLock(b) entered while WithLocks held b")
	case <-time.After(150 * time.Millisecond):
		// good: still blocked
	}
	close(release)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("WithLock(b) never acquired after WithLocks released")
	}
}

// TestWithLocks_DuplicatePathsNoDeadlock proves the de-duplication: passing the
// same path twice (which maps to one stripe) must not self-deadlock on the
// non-reentrant mutex.
func TestWithLocks_DuplicatePathsNoDeadlock(t *testing.T) {
	m := NewManager(Config{})
	defer m.Stop()
	done := make(chan struct{})
	go func() {
		_ = m.WithLocks(context.Background(), "", []string{"x", "x", "x"}, func() error { return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WithLocks with duplicate paths deadlocked")
	}
}

// TestWithLocks_ConcurrentOppositeOrderNoDeadlock runs many WithLocks calls whose
// path sets overlap in opposite orders; the canonical stripe ordering must keep
// them deadlock-free. Run under -race it also checks the critical sections do not
// interleave on a shared counter.
func TestWithLocks_ConcurrentOppositeOrderNoDeadlock(t *testing.T) {
	m := NewManager(Config{})
	defer m.Stop()

	var mu sync.Mutex
	counter := 0
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = m.WithLocks(context.Background(), "", []string{"p", "q"}, func() error {
				mu.Lock()
				counter++
				mu.Unlock()
				return nil
			})
		}()
		go func() {
			defer wg.Done()
			_ = m.WithLocks(context.Background(), "", []string{"q", "p"}, func() error {
				mu.Lock()
				counter++
				mu.Unlock()
				return nil
			})
		}()
	}
	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent opposite-order WithLocks deadlocked")
	}
	if counter != 100 {
		t.Errorf("counter = %d, want 100", counter)
	}
}

// TestWithLocks_CtxCancelReleasesAcquired verifies a cancelled context while
// waiting for one stripe releases the stripes already acquired (no leak).
func TestWithLocks_CtxCancelReleasesAcquired(t *testing.T) {
	m := NewManager(Config{})
	defer m.Stop()

	hold := make(chan struct{})
	go func() {
		_ = m.WithLock(context.Background(), "", "b", func() error {
			<-hold
			return nil
		})
	}()
	time.Sleep(50 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- m.WithLocks(ctx, "", []string{"a", "b"}, func() error { return nil })
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected ctx error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WithLocks did not return after ctx cancel")
	}
	close(hold)

	// 'a' must be free now (it was released on cancel): a fresh lock acquires fast.
	got := make(chan struct{})
	go func() {
		_ = m.WithLock(context.Background(), "", "a", func() error { close(got); return nil })
	}()
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("path 'a' was not released after cancelled WithLocks")
	}
}
