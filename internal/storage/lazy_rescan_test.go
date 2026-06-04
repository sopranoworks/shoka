package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// D1 (B-25) lazy-rescan-on-corrupted-hit. checkWritable's case StateCorrupted
// re-runs DetectDrift before refusing and proceeds iff the project is genuinely
// healthy now. These tests pin the five §5.2 properties.

// state-stale-but-tree-clean → the write proceeds (the B-25 fix). The in-memory
// state is corrupted but the working tree matches the catalog (the operator
// reconciled drift out-of-band); the lazy rescan recomputes healthy and the write
// goes through. The counter increments exactly once (self-extinguishing).
func TestLazyRescan_StaleCorruptedButClean_WriteProceeds(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "original", nil)
	require.NoError(t, err)
	drain(t, s)

	// Force a STALE corrupted state: nothing on disk diverges from the catalog;
	// only the in-memory mark is out of date (the B-25 condition).
	s.setState("ns", "proj", StateCorrupted)
	require.Equal(t, StateCorrupted, s.State("ns", "proj"))

	// A write hits the corrupted branch, lazily rescans, finds the tree clean, and
	// proceeds.
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "b.md", "x", nil)
	require.NoError(t, err, "stale-corrupted clean project must accept the write after the lazy rescan")

	assert.Equal(t, StateHealthy, s.State("ns", "proj"), "the lazy rescan must have flipped the project to healthy")
	assert.Equal(t, int64(1), s.LazyRescanCount(), "exactly one lazy rescan should have run")

	drain(t, s)
	content, _, err := s.ReadFileWithETag("ns", "proj", "b.md")
	require.NoError(t, err)
	assert.Equal(t, "x", content)

	// Self-extinguishing: a subsequent write hits case StateHealthy and never
	// re-pays the rescan.
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "c.md", "y", nil)
	require.NoError(t, err)
	assert.Equal(t, int64(1), s.LazyRescanCount(), "the healthy follow-up write must not pay the rescan")
}

// content-divergence → the write is STILL refused (truth, not success). The
// operator changed file CONTENT behind Shoka's back, so the catalog etag is stale;
// the lazy rescan re-detects Modified, the state stays corrupted, and the write is
// (correctly) refused. The clear path is recovery, which then unblocks writes.
func TestLazyRescan_ContentDivergence_StillRefused(t *testing.T) {
	s, dir := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "original", nil)
	require.NoError(t, err)
	drain(t, s)

	// Real divergence: hand-edit the tracked file off the write path.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ns", "proj", "a.md"), []byte("hand-edited"), 0o644))
	s.setState("ns", "proj", StateCorrupted) // the in-memory mark (also what DetectDrift would set)

	// The write hits the corrupted branch, lazily rescans, re-finds the divergence,
	// and refuses — the lazy rescan recomputes TRUTH, it does not force success.
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "b.md", "x", nil)
	assert.ErrorIs(t, err, ErrProjectCorrupted, "content divergence must stay refused after the lazy rescan")
	assert.Equal(t, StateCorrupted, s.State("ns", "proj"), "state must remain corrupted on a real divergence")
	assert.GreaterOrEqual(t, s.LazyRescanCount(), int64(1), "the corrupted branch should have run the rescan")

	// The operator's path is recovery, not a write that papers over the divergence.
	_, rerr := s.RepairTrackedChanges(context.Background(), "ns", "proj")
	require.NoError(t, rerr)
	assert.Equal(t, StateHealthy, s.State("ns", "proj"))
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "b.md", "x", nil)
	require.NoError(t, err, "after recovery the write succeeds")
}

// healthy project → never pays the rescan (self-extinguishing; the rescan lives
// only on the corrupted branch). Even when the tree has diverged but the in-memory
// state is still healthy, a write hits case StateHealthy and does NOT re-detect —
// the counter stays 0 and the state is left untouched.
func TestLazyRescan_HealthyNeverPays(t *testing.T) {
	s, dir := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "original", nil)
	require.NoError(t, err)
	drain(t, s)
	require.Equal(t, StateHealthy, s.State("ns", "proj"))

	// Ordinary healthy writes never touch the rescan.
	for i := 0; i < 3; i++ {
		_, err = s.Write(context.Background(), "sess", "ns", "proj", fmt.Sprintf("f%d.md", i), "v", nil)
		require.NoError(t, err)
	}
	assert.Equal(t, int64(0), s.LazyRescanCount(), "healthy writes must never run the lazy rescan")

	// Diverge the tree but leave the in-memory state healthy (no DetectDrift run).
	// A write to another file must still take the healthy path: had the rescan been
	// wrongly on the healthy path, DetectDrift would flip the project to corrupted.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ns", "proj", "a.md"), []byte("hand-edited"), 0o644))
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "post.md", "z", nil)
	require.NoError(t, err)
	assert.Equal(t, int64(0), s.LazyRescanCount(), "the healthy path must not re-detect drift")
	assert.Equal(t, StateHealthy, s.State("ns", "proj"), "the healthy path must leave the state untouched")
}

// dangerous project → still refused, and NOT lazily rescanned (the rescan is
// corrupted-branch-only). case StateDangerous returns before the corrupted branch,
// so the counter stays 0.
func TestLazyRescan_DangerousNotRescanned(t *testing.T) {
	s, dir := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "original", nil)
	require.NoError(t, err)
	drain(t, s)

	// Make .git unreadable → dangerous.
	require.NoError(t, os.Rename(filepath.Join(dir, "ns", "proj", ".git"), filepath.Join(dir, "git-broken")))
	s.setState("ns", "proj", StateDangerous)

	_, err = s.Write(context.Background(), "sess", "ns", "proj", "b.md", "x", nil)
	assert.ErrorIs(t, err, ErrProjectDangerous, "a dangerous project must stay refused")
	assert.Equal(t, int64(0), s.LazyRescanCount(), "a dangerous project must not be lazily rescanned")
}

// no deadlock: the lazy rescan runs with no lock held, and the per-file lock is
// taken immediately after checkWritable returns. Many concurrent writes to a
// stale-corrupted-but-clean project must all complete (bounded), proving the
// rescan introduces no lock-ordering issue with the per-file lock that follows.
func TestLazyRescan_NoDeadlockUnderConcurrentWrites(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "seed.md", "seed", nil)
	require.NoError(t, err)
	drain(t, s)

	// Stale corrupted mark on a clean tree: every concurrent writer hits the
	// corrupted branch and lazily rescans, then takes its per-file lock.
	s.setState("ns", "proj", StateCorrupted)

	const writers = 24
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	start := time.Now()
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, werr := s.Write(context.Background(), fmt.Sprintf("s%d", i),
				"ns", "proj", fmt.Sprintf("f%d.md", i), fmt.Sprintf("c%d", i), nil)
			errs <- werr
		}(i)
	}
	wg.Wait()
	close(errs)
	elapsed := time.Since(start)
	for werr := range errs {
		require.NoError(t, werr, "every concurrent write to a stale-corrupted clean project must succeed")
	}
	assert.Less(t, elapsed, 5*time.Second, "concurrent lazy rescans + per-file locks must not deadlock or stall")
	assert.Equal(t, StateHealthy, s.State("ns", "proj"))
}
