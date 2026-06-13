package walworker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shoka/mcp-server/internal/storage/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The D3 quarantine battery (B-38.2). Before this fix, process() looped forever on
// a permanently-uncommittable entry: it never returned, so its deferred doneCh
// never fired — the project's in-flight mark was held forever (head-of-line
// blocking every later entry) and the worker slot was consumed; MaxWorkers such
// entries saturated the pool and stalled ALL commit progress. The load-bearing fix
// is that process() RETURNS on quarantine (fires doneCh), not merely that it stops
// retrying. These tests pin that, plus the pool-not-saturated regression (the
// teeth), the deposit-before-Remove ordering, the N-attempt backstop, and that a
// transient failure within the backstop still commits.

// permanentCommitErr wraps ErrCommitPermanent the way storage.commitEntry does for a
// repo-absent project (multi-%w keeps the underlying cause in the chain).
func permanentCommitErr() error {
	return fmt.Errorf("open repo: %w: %w", errors.New("repository does not exist"), ErrCommitPermanent)
}

// quarantineRecorder is a test QuarantineFunc. It records the entries deposited; if
// failWith is set it instead reports a deposit failure (so the entry is kept).
type quarantineRecorder struct {
	mu       sync.Mutex
	deposits []wal.Entry
	failWith error
}

func (q *quarantineRecorder) fn(e wal.Entry) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.failWith != nil {
		return q.failWith
	}
	q.deposits = append(q.deposits, e)
	return nil
}

func (q *quarantineRecorder) deposited() []wal.Entry {
	q.mu.Lock()
	defer q.mu.Unlock()
	cp := make([]wal.Entry, len(q.deposits))
	copy(cp, q.deposits)
	return cp
}

// TestPool_PermanentFailureQuarantinesDepositsRemovesReturns is the core D3 case: a
// classified-permanent entry is deposited to lost+found (via the QuarantineFunc),
// removed from the WAL, counted, and — the load-bearing part — process() RETURNS so
// the project's in-flight mark clears and a LATER entry for the SAME project
// dispatches and commits. (Before the fix, the later entry would be head-of-line
// blocked forever.)
func TestPool_PermanentFailureQuarantinesDepositsRemovesReturns(t *testing.T) {
	l := openWAL(t)
	rec := &quarantineRecorder{}

	var committed sync.Map // path -> true
	commit := func(_ context.Context, e wal.Entry) error {
		if e.Path == "bad.md" {
			return permanentCommitErr()
		}
		committed.Store(e.Path, true)
		return nil
	}
	p := NewPool(l, commit, rec.fn, fastConfig())
	t.Cleanup(func() { _ = p.Shutdown(2 * time.Second) })

	// Same project: a permanently-uncommittable entry FIRST, then a healthy one.
	appendWrite(t, l, "ns", "p", "bad.md")
	appendWrite(t, l, "ns", "p", "good.md")
	p.Notify()

	waitDrained(t, l)

	// The bad entry was deposited to lost+found and removed; the later same-project
	// entry committed — proving doneCh fired and the in-flight mark cleared.
	deposited := rec.deposited()
	require.Len(t, deposited, 1, "the permanent entry must be deposited exactly once")
	assert.Equal(t, "bad.md", deposited[0].Path)
	assert.Equal(t, []byte("bad.md"), deposited[0].Content, "deposit carries the entry's WAL content")
	_, goodCommitted := committed.Load("good.md")
	assert.True(t, goodCommitted, "the later same-project entry must dispatch and commit (doneCh fired)")

	st := p.Stats()
	assert.Equal(t, int64(1), st.QuarantinedTotal)
	assert.Equal(t, int64(0), st.QuarantineFailed)
	assert.Equal(t, int64(1), st.CommitsTotal, "the healthy entry committed")
	assert.Equal(t, 0, l.PendingCount(), "both entries left the WAL")
	// The in-flight mark clears one step AFTER the WAL drains: process() removes the
	// entry, then RETURNS, then the deferred doneCh fires, then the dispatcher unmarks
	// the project. waitDrained only gates on the WAL being empty, so await the real
	// post-condition here rather than reading the snapshot a beat too early.
	assert.Eventually(t, func() bool { return p.Stats().InFlightProjects == 0 }, 2*time.Second, 5*time.Millisecond, "the in-flight mark cleared")
}

// TestPool_NotSaturatedByPermanentFailures is the availability-hole regression — the
// teeth. MaxWorkers permanently-failing projects must NOT stall healthy projects'
// commits. Before the fix, the failing projects' workers loop forever and the pool
// saturates so the healthy entries never drain (this test would time out). After the
// fix each failing entry quarantines and returns, so the pool drains fully.
func TestPool_NotSaturatedByPermanentFailures(t *testing.T) {
	l := openWAL(t)
	rec := &quarantineRecorder{}

	const badProjects = 8 // == default MaxWorkers: enough to saturate the pool
	const okProjects = 4

	var okCommitted sync.Map
	commit := func(_ context.Context, e wal.Entry) error {
		if strings.HasPrefix(e.Project, "bad") {
			return permanentCommitErr()
		}
		time.Sleep(2 * time.Millisecond)
		okCommitted.Store(e.Project, true)
		return nil
	}
	p := NewPool(l, commit, rec.fn, fastConfig())
	t.Cleanup(func() { _ = p.Shutdown(2 * time.Second) })

	for i := 0; i < badProjects; i++ {
		appendWrite(t, l, "ns", fmt.Sprintf("bad%d", i), "f.md")
	}
	for i := 0; i < okProjects; i++ {
		appendWrite(t, l, "ns", fmt.Sprintf("ok%d", i), "f.md")
	}
	p.Notify()

	// The WHOLE WAL drains — the healthy projects are not starved by the failing
	// ones. (Before the fix this never reaches zero: 8 workers loop forever.)
	waitDrained(t, l)

	st := p.Stats()
	assert.Equal(t, int64(badProjects), st.QuarantinedTotal, "every permanent entry quarantined")
	assert.Equal(t, int64(okProjects), st.CommitsTotal, "every healthy entry committed")
	for i := 0; i < okProjects; i++ {
		_, ok := okCommitted.Load(fmt.Sprintf("ok%d", i))
		assert.Truef(t, ok, "healthy project ok%d must have committed", i)
	}
}

// TestPool_DepositFailureKeepsEntry pins the deposit-before-Remove ordering: when the
// QuarantineFunc reports a deposit failure, the entry is NOT removed (its content is
// preserved). The worker still returns (no saturation), so a DIFFERENT project's
// entry commits normally.
func TestPool_DepositFailureKeepsEntry(t *testing.T) {
	l := openWAL(t)
	rec := &quarantineRecorder{failWith: errors.New("lost+found unwritable")}

	var okCommitted sync.Map
	commit := func(_ context.Context, e wal.Entry) error {
		if e.Project == "bad" {
			return permanentCommitErr()
		}
		okCommitted.Store(e.Project, true)
		return nil
	}
	p := NewPool(l, commit, rec.fn, fastConfig())
	t.Cleanup(func() { _ = p.Shutdown(2 * time.Second) })

	appendWrite(t, l, "ns", "bad", "f.md")
	appendWrite(t, l, "ns", "ok", "g.md")
	p.Notify()

	// The healthy entry commits despite the bad project's repeated deposit failures
	// (the worker is not stuck): proof the slot frees each round.
	require.Eventually(t, func() bool { _, ok := okCommitted.Load("ok"); return ok },
		2*time.Second, 5*time.Millisecond, "a different project's entry must still commit")
	require.Eventually(t, func() bool { return p.Stats().QuarantineFailed >= 1 },
		2*time.Second, 5*time.Millisecond, "the deposit failure must be surfaced")

	// The bad entry is NOT removed — its content is kept until it can be preserved.
	assert.Equal(t, int64(0), p.Stats().QuarantinedTotal, "nothing was successfully quarantined")
	assert.GreaterOrEqual(t, l.PendingCount(), 1, "the un-deposited entry must stay in the WAL")
	require.NoError(t, p.Shutdown(2*time.Second))
}

// TestPool_BackstopQuarantinesUnclassifiedStuckEntry pins the N-attempt backstop: an
// UNCLASSIFIED error (not ErrCommitPermanent) that keeps failing is quarantined after
// MaxCommitAttempts, so nothing loops indefinitely.
func TestPool_BackstopQuarantinesUnclassifiedStuckEntry(t *testing.T) {
	l := openWAL(t)
	rec := &quarantineRecorder{}

	var attempts atomic.Int64
	commit := func(_ context.Context, _ wal.Entry) error {
		attempts.Add(1)
		return errors.New("stuck, but not classified permanent") // never wraps ErrCommitPermanent
	}
	cfg := fastConfig()
	cfg.MaxCommitAttempts = 3
	p := NewPool(l, commit, rec.fn, cfg)
	t.Cleanup(func() { _ = p.Shutdown(2 * time.Second) })

	appendWrite(t, l, "ns", "p", "f.md")
	p.Notify()

	waitDrained(t, l) // it leaves the WAL only by being quarantined

	assert.Equal(t, int64(1), p.Stats().QuarantinedTotal, "the stuck entry quarantined after the backstop")
	require.Len(t, rec.deposited(), 1, "its content was deposited")
	assert.GreaterOrEqual(t, attempts.Load(), int64(3), "it was retried up to the backstop before quarantine")
}

// TestPool_TransientWithinBackstopCommits pins that a transient failure recovering
// WITHIN MaxCommitAttempts commits normally — no false quarantine.
func TestPool_TransientWithinBackstopCommits(t *testing.T) {
	l := openWAL(t)
	rec := &quarantineRecorder{}

	var attempts atomic.Int64
	commit := func(_ context.Context, _ wal.Entry) error {
		if attempts.Add(1) <= 3 {
			return errors.New("transient")
		}
		return nil
	}
	cfg := fastConfig()
	cfg.MaxCommitAttempts = 10 // 3 transient failures < 10 backstop
	p := NewPool(l, commit, rec.fn, cfg)
	t.Cleanup(func() { _ = p.Shutdown(2 * time.Second) })

	appendWrite(t, l, "ns", "p", "f.md")
	p.Notify()

	waitDrained(t, l)

	st := p.Stats()
	assert.Equal(t, int64(1), st.CommitsTotal, "the entry committed after the transient cleared")
	assert.Equal(t, int64(0), st.QuarantinedTotal, "no false quarantine within the backstop")
	assert.Empty(t, rec.deposited(), "nothing was deposited")
}
