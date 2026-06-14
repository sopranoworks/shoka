package walworker

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/storage/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openWAL(t *testing.T) *wal.Log {
	t.Helper()
	l, err := wal.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func appendWrite(t *testing.T, l *wal.Log, ns, project, path string) {
	t.Helper()
	_, err := l.Append(wal.Entry{Namespace: ns, Project: project, Path: path, Op: "write", Content: []byte(path)})
	require.NoError(t, err)
}

// orderRecorder records, per project, the order in which paths were committed.
type orderRecorder struct {
	mu    sync.Mutex
	order map[string][]string
}

func newOrderRecorder() *orderRecorder { return &orderRecorder{order: map[string][]string{}} }

func (r *orderRecorder) commit(_ context.Context, e wal.Entry) error {
	time.Sleep(5 * time.Millisecond) // make interleaving observable
	r.mu.Lock()
	r.order[e.Project] = append(r.order[e.Project], e.Path)
	r.mu.Unlock()
	return nil
}

func (r *orderRecorder) get(project string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := make([]string, len(r.order[project]))
	copy(cp, r.order[project])
	return cp
}

func fastConfig() Config {
	return Config{
		MinWorkers: 1, MaxWorkers: 8,
		IdleTimeout: 100 * time.Millisecond, ScanInterval: 20 * time.Millisecond,
		BackoffInitial: 5 * time.Millisecond, BackoffMax: 20 * time.Millisecond,
	}
}

func waitDrained(t *testing.T, l *wal.Log) {
	t.Helper()
	require.Eventually(t, func() bool { return l.PendingCount() == 0 }, 5*time.Second, 5*time.Millisecond, "WAL did not drain")
}

func TestPool_PreservesPerProjectOrder(t *testing.T) {
	l := openWAL(t)
	rec := newOrderRecorder()
	p := NewPool(l, rec.commit, nil, fastConfig())
	t.Cleanup(func() { _ = p.Shutdown(2 * time.Second) })

	// Append: p1-A, p2-X, p1-B, p1-C, p2-Y.
	appendWrite(t, l, "ns", "p1", "A")
	appendWrite(t, l, "ns", "p2", "X")
	appendWrite(t, l, "ns", "p1", "B")
	appendWrite(t, l, "ns", "p1", "C")
	appendWrite(t, l, "ns", "p2", "Y")
	p.Notify()

	waitDrained(t, l)

	assert.Equal(t, []string{"A", "B", "C"}, rec.get("p1"))
	assert.Equal(t, []string{"X", "Y"}, rec.get("p2"))
}

func TestPool_DifferentProjectsCommitInParallel(t *testing.T) {
	l := openWAL(t)
	const projects = 4
	const commitTime = 100 * time.Millisecond

	commit := func(_ context.Context, _ wal.Entry) error {
		time.Sleep(commitTime)
		return nil
	}
	p := NewPool(l, commit, nil, fastConfig())
	t.Cleanup(func() { _ = p.Shutdown(2 * time.Second) })

	for i := 0; i < projects; i++ {
		appendWrite(t, l, "ns", fmt.Sprintf("proj%d", i), "f.md")
	}
	start := time.Now()
	p.Notify()
	waitDrained(t, l)
	elapsed := time.Since(start)

	// Serial would be ~projects*commitTime. Parallel should be far less.
	assert.Less(t, elapsed, time.Duration(projects)*commitTime-50*time.Millisecond,
		"projects should commit concurrently (elapsed=%s)", elapsed)
}

func TestPool_GrowsAndShrinks(t *testing.T) {
	l := openWAL(t)
	cfg := fastConfig()
	cfg.MinWorkers = 1
	cfg.MaxWorkers = 4
	cfg.IdleTimeout = 120 * time.Millisecond

	commit := func(_ context.Context, _ wal.Entry) error {
		time.Sleep(200 * time.Millisecond)
		return nil
	}
	p := NewPool(l, commit, nil, cfg)
	t.Cleanup(func() { _ = p.Shutdown(2 * time.Second) })

	for i := 0; i < 4; i++ {
		appendWrite(t, l, "ns", fmt.Sprintf("proj%d", i), "f.md")
	}
	p.Notify()

	// Grows toward MaxWorkers while commits are slow.
	require.Eventually(t, func() bool { return p.Stats().ActiveWorkers >= 3 }, 2*time.Second, 5*time.Millisecond,
		"pool should grow under load")

	waitDrained(t, l)

	// Shrinks back to MinWorkers after IdleTimeout.
	require.Eventually(t, func() bool { return p.Stats().ActiveWorkers == cfg.MinWorkers }, 3*time.Second, 10*time.Millisecond,
		"pool should retire idle workers down to MinWorkers")
}

func TestPool_PersistentFailureKeepsEntry(t *testing.T) {
	l := openWAL(t)
	commit := func(_ context.Context, _ wal.Entry) error {
		return fmt.Errorf("always fails")
	}
	// High backstop so this exercises the pure retry path (an unclassified,
	// non-permanent failure keeps retrying and keeps the entry) without hitting the
	// MaxCommitAttempts quarantine backstop — that is covered separately.
	cfg := fastConfig()
	cfg.MaxCommitAttempts = 1_000_000
	p := NewPool(l, commit, nil, cfg)
	t.Cleanup(func() { _ = p.Shutdown(2 * time.Second) })

	appendWrite(t, l, "ns", "p", "f.md")
	p.Notify()

	require.Eventually(t, func() bool { return p.Stats().CommitsFailed >= 3 }, 2*time.Second, 5*time.Millisecond,
		"CommitsFailed should grow under persistent failure")
	assert.Equal(t, 1, l.PendingCount(), "the entry must stay in the WAL")
	assert.Equal(t, int64(0), p.Stats().CommitsTotal)
	assert.Equal(t, int64(0), p.Stats().QuarantinedTotal, "no quarantine below the backstop")
}

func TestPool_FailThenSucceedRemovesEntry(t *testing.T) {
	l := openWAL(t)
	var attempts atomic.Int64
	commit := func(_ context.Context, _ wal.Entry) error {
		if attempts.Add(1) <= 3 {
			return fmt.Errorf("transient")
		}
		return nil
	}
	p := NewPool(l, commit, nil, fastConfig())
	t.Cleanup(func() { _ = p.Shutdown(2 * time.Second) })

	appendWrite(t, l, "ns", "p", "f.md")
	p.Notify()

	waitDrained(t, l)
	assert.Equal(t, int64(1), p.Stats().CommitsTotal)
	assert.GreaterOrEqual(t, p.Stats().CommitsFailed, int64(3))
}

func TestPool_ShutdownWaitsForInFlight(t *testing.T) {
	l := openWAL(t)
	started := make(chan struct{})
	var startOnce sync.Once
	commit := func(_ context.Context, _ wal.Entry) error {
		startOnce.Do(func() { close(started) })
		time.Sleep(150 * time.Millisecond) // ignores ctx on purpose
		return nil
	}
	p := NewPool(l, commit, nil, fastConfig())

	appendWrite(t, l, "ns", "p", "f.md")
	p.Notify()
	<-started // a commit is in flight

	start := time.Now()
	require.NoError(t, p.Shutdown(2*time.Second))
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, 100*time.Millisecond, "Shutdown must wait for the in-flight commit")
}

func TestPool_NotifyTriggersImmediateDispatch(t *testing.T) {
	l := openWAL(t)
	cfg := fastConfig()
	cfg.ScanInterval = 5 * time.Second // only Notify should trigger promptly

	committed := make(chan time.Time, 1)
	commit := func(_ context.Context, _ wal.Entry) error {
		select {
		case committed <- time.Now():
		default:
		}
		return nil
	}
	p := NewPool(l, commit, nil, cfg)
	t.Cleanup(func() { _ = p.Shutdown(2 * time.Second) })

	appendWrite(t, l, "ns", "p", "f.md")
	t0 := time.Now()
	p.Notify()

	select {
	case ts := <-committed:
		assert.Less(t, ts.Sub(t0), 200*time.Millisecond, "Notify should dispatch well before the 5s scan interval")
	case <-time.After(time.Second):
		t.Fatal("commit did not happen after Notify")
	}
}
