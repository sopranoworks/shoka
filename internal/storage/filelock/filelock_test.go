package filelock

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// twoDifferentStripePaths returns two paths guaranteed to map to different
// stripes, so a "different paths run in parallel" assertion is deterministic.
func twoDifferentStripePaths() (string, string) {
	base := "p0"
	s0 := fnv32a(base) % numStripes
	for i := 1; ; i++ {
		cand := fmt.Sprintf("p%d", i)
		if fnv32a(cand)%numStripes != s0 {
			return base, cand
		}
	}
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m := NewManager(Config{MaxLeaseDuration: 5 * time.Minute, ReaperInterval: 50 * time.Millisecond})
	t.Cleanup(m.Stop)
	return m
}

func TestWithLock_SamePathSerialises(t *testing.T) {
	m := newTestManager(t)
	const path = "same.md"

	var inCritical atomic.Int32
	var maxConcurrent atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := m.WithLock(context.Background(), "s", path, func() error {
				n := inCritical.Add(1)
				for {
					old := maxConcurrent.Load()
					if n <= old || maxConcurrent.CompareAndSwap(old, n) {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
				inCritical.Add(-1)
				return nil
			})
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), maxConcurrent.Load(), "same path must never run two fns concurrently")
}

func TestWithLock_DifferentPathsRunInParallel(t *testing.T) {
	m := newTestManager(t)
	p1, p2 := twoDifferentStripePaths()

	const hold = 150 * time.Millisecond
	start := time.Now()
	var wg sync.WaitGroup
	for _, p := range []string{p1, p2} {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			_ = m.WithLock(context.Background(), "s", path, func() error {
				time.Sleep(hold)
				return nil
			})
		}(p)
	}
	wg.Wait()
	elapsed := time.Since(start)

	// If they serialised, elapsed would be ~2*hold. Parallel => ~hold.
	assert.Less(t, elapsed, 2*hold-20*time.Millisecond,
		"different paths should run concurrently (elapsed=%s)", elapsed)
}

func TestWithLock_PanicBecomesErrorAndReleasesLock(t *testing.T) {
	m := newTestManager(t)
	const path = "boom.md"

	err := m.WithLock(context.Background(), "s", path, func() error {
		panic("kaboom")
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "panic")

	// The lock must have been released: a subsequent WithLock proceeds promptly.
	done := make(chan struct{})
	go func() {
		_ = m.WithLock(context.Background(), "s", path, func() error { return nil })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("subsequent WithLock blocked; lock was not released after panic")
	}

	assert.Empty(t, m.ActiveLeases(), "lease record must be dropped after panic")
}

func TestWithLock_ContextCancelledWhileWaiting(t *testing.T) {
	m := newTestManager(t)
	const path = "contended.md"

	holderReady := make(chan struct{})
	releaseHolder := make(chan struct{})
	go func() {
		_ = m.WithLock(context.Background(), "holder", path, func() error {
			close(holderReady)
			<-releaseHolder
			return nil
		})
	}()
	<-holderReady // the path is now held

	ctx, cancel := context.WithCancel(context.Background())
	var fnRan atomic.Bool
	errCh := make(chan error, 1)
	go func() {
		errCh <- m.WithLock(ctx, "waiter", path, func() error {
			fnRan.Store(true)
			return nil
		})
	}()

	time.Sleep(30 * time.Millisecond) // let the waiter block on acquisition
	cancel()

	select {
	case err := <-errCh:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("cancelled waiter did not return")
	}
	assert.False(t, fnRan.Load(), "fn must not run when ctx is cancelled while waiting")

	close(releaseHolder) // let the holder finish
}

func TestReleaseAllForSession_RemovesLeaseRecords(t *testing.T) {
	m := newTestManager(t)

	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	for _, p := range []string{"a.md", "b.md"} {
		go func(path string) {
			_ = m.WithLock(context.Background(), "sess-1", path, func() error {
				ready <- struct{}{}
				<-release
				return nil
			})
		}(p)
	}
	<-ready
	<-ready
	require.Len(t, m.ActiveLeases(), 2)

	m.ReleaseAllForSession("sess-1")
	assert.Empty(t, m.ActiveLeases(), "records for the session must be removed")

	// Empty sessionID is exempt.
	m.ReleaseAllForSession("")
	close(release)
}

func TestReaper_ReleasesStaleLeaseRecords(t *testing.T) {
	m := NewManager(Config{MaxLeaseDuration: 200 * time.Millisecond, ReaperInterval: 20 * time.Millisecond})
	t.Cleanup(m.Stop)
	const path = "long.md"

	ready := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = m.WithLock(context.Background(), "s", path, func() error {
			close(ready)
			<-release
			return nil
		})
	}()
	<-ready
	require.Len(t, m.ActiveLeases(), 1)

	require.Eventually(t, func() bool {
		return len(m.ActiveLeases()) == 0
	}, time.Second, 10*time.Millisecond, "reaper should drop the stale lease record")
	assert.GreaterOrEqual(t, m.ForcedReleaseCount(), int64(1))

	close(release)
}

func TestActiveLeases_ReflectsInFlightWork(t *testing.T) {
	m := newTestManager(t)

	assert.Empty(t, m.ActiveLeases())

	ready := make(chan struct{})
	release := make(chan struct{})
	go func() {
		_ = m.WithLock(context.Background(), "sess-x", "doc.md", func() error {
			close(ready)
			<-release
			return nil
		})
	}()
	<-ready

	leases := m.ActiveLeases()
	require.Len(t, leases, 1)
	assert.Equal(t, "doc.md", leases[0].Path)
	assert.Equal(t, "sess-x", leases[0].SessionID)
	assert.False(t, leases[0].AcquiredAt.IsZero())

	close(release)
	require.Eventually(t, func() bool { return len(m.ActiveLeases()) == 0 }, time.Second, 5*time.Millisecond)
}
