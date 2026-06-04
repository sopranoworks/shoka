package storage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/shoka/mcp-server/internal/storage/index"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// I1 §5.4 — the StartIndexSweep repair worker. Detection is last_indexed_commit
// vs HEAD; rebuild is wholesale from working-tree bytes; the worker never blocks a
// query (it runs off the request path, in its own goroutine).

// evictIndexHandle drops the cached open handle for a project so the next
// indexForRead re-opens from disk — used to simulate a fresh process seeing a
// corrupt/missing on-disk store (a single process cannot open the same bbolt file
// twice).
func evictIndexHandle(s *FSGitStorage, ns, proj string) {
	key := projectKey(ns, proj)
	s.idxMu.Lock()
	if ix := s.indexes[key]; ix != nil {
		_ = ix.Close()
	}
	delete(s.indexes, key)
	s.idxMu.Unlock()
}

func openIndexRO(t *testing.T, s *FSGitStorage, ns, proj string) *index.Index {
	t.Helper()
	ix := s.indexForRead(ns, proj)
	require.NotNil(t, ix)
	return ix
}

func TestIndexSweep_StaleMarkerRebuildsAndAdvances(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello", nil)
	require.NoError(t, err)
	drain(t, s) // HEAD now has the commit; the incremental marker is still ""

	head, ok := s.headCommit("ns", "proj")
	require.True(t, ok)
	require.NotEmpty(t, head)

	// Precondition: the marker lags HEAD (incremental updates never advance it).
	require.NotEqual(t, head, mustMarker(t, openIndexRO(t, s, "ns", "proj")))

	before, _, rebuildsBefore := s.IndexCounters()
	_ = before
	s.reconcileIndex("ns", "proj")

	_, _, rebuildsAfter := s.IndexCounters()
	assert.Equal(t, rebuildsBefore+1, rebuildsAfter, "a stale index must be rebuilt once")
	ix := openIndexRO(t, s, "ns", "proj")
	assert.Equal(t, head, mustMarker(t, ix), "marker must advance to HEAD after rebuild")
	rec, found, _ := ix.GetRecord("a.md")
	assert.True(t, found, "rebuilt index must contain the working-tree file")
	assert.NotEmpty(t, rec.Etag)
}

func TestIndexSweep_UpToDateIsCheapNoOp(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "x", nil)
	require.NoError(t, err)
	drain(t, s)

	s.reconcileIndex("ns", "proj") // first pass advances the marker to HEAD
	_, _, rebuildsAfterFirst := s.IndexCounters()

	s.reconcileIndex("ns", "proj") // nothing changed → no-op
	_, _, rebuildsAfterSecond := s.IndexCounters()
	assert.Equal(t, rebuildsAfterFirst, rebuildsAfterSecond, "an up-to-date index must not be rebuilt")
}

func TestIndexSweep_CorruptStoreRebuilt(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello", nil)
	require.NoError(t, err)
	drain(t, s)

	// Simulate a fresh process finding a corrupt on-disk store.
	evictIndexHandle(s, "ns", "proj")
	require.NoError(t, os.WriteFile(s.indexPath("ns", "proj"), []byte("not a bbolt db"), 0o600))

	_, _, rebuildsBefore := s.IndexCounters()
	s.reconcileIndex("ns", "proj")
	_, _, rebuildsAfter := s.IndexCounters()
	assert.Equal(t, rebuildsBefore+1, rebuildsAfter, "a corrupt store must be rebuilt")

	head, _ := s.headCommit("ns", "proj")
	ix := openIndexRO(t, s, "ns", "proj")
	assert.Equal(t, head, mustMarker(t, ix))
	if _, found, _ := ix.GetRecord("a.md"); !found {
		t.Error("rebuilt-from-corrupt index missing the working-tree file")
	}
}

func TestIndexSweep_MissingStoreBuilt(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello", nil)
	require.NoError(t, err)
	drain(t, s)

	// Remove the store entirely (and its handle): the sweep must rebuild it.
	evictIndexHandle(s, "ns", "proj")
	require.NoError(t, os.Remove(s.indexPath("ns", "proj")))

	s.reconcileIndex("ns", "proj")

	_, statErr := os.Stat(s.indexPath("ns", "proj"))
	require.NoError(t, statErr, "the sweep must recreate a missing index")
	head, _ := s.headCommit("ns", "proj")
	ix := openIndexRO(t, s, "ns", "proj")
	assert.Equal(t, head, mustMarker(t, ix))
	if _, found, _ := ix.GetRecord("a.md"); !found {
		t.Error("rebuilt-from-missing index missing the working-tree file")
	}
}

func TestStartIndexSweep_CleanShutdown(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "x", nil)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	s.StartIndexSweep(ctx, 10*time.Millisecond)
	time.Sleep(40 * time.Millisecond) // let a few passes run
	cancel()
	time.Sleep(30 * time.Millisecond) // the goroutine must observe ctx.Done and return
	// No assertion beyond "did not panic / deadlock"; -race covers data races.
}

func TestStartIndexSweep_ZeroIntervalDisabled(t *testing.T) {
	s, _ := newStore(t, Options{})
	// interval <= 0 is a no-op: returns immediately, starts no goroutine.
	s.StartIndexSweep(context.Background(), 0)
}

func mustMarker(t *testing.T, ix *index.Index) string {
	t.Helper()
	m, err := ix.LastIndexedCommit()
	require.NoError(t, err)
	return m
}
