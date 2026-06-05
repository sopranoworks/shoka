package storage

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Index-line class-B metric accessors (the 2026-06-05 M2 directive). These pin the
// increment sites and the Source-method shapes the metrics collector reads on
// scrape. They are read-only/additive — the underlying sweep/search/fix_links
// behaviour is exercised by index_sweep_test.go / index_fulltext_test.go /
// fixlinks_test.go and is unchanged.

func TestIndexSweepRuns_CountsPassesNotRebuilds(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello", nil)
	require.NoError(t, err)
	drain(t, s)

	require.Equal(t, int64(0), s.IndexSweepRuns())

	// Pass 1 rebuilds the stale index (marker lagged HEAD) and counts as a run.
	s.reconcileAllIndexes()
	assert.Equal(t, int64(1), s.IndexSweepRuns())

	// Pass 2 finds the index current: it rebuilds nothing, but still counts as a
	// run — the sweep-run counter is distinct from rebuilds.
	_, _, rebuildsBefore := s.IndexCounters()
	s.reconcileAllIndexes()
	assert.Equal(t, int64(2), s.IndexSweepRuns(), "a no-op pass still increments sweep-runs")
	_, _, rebuildsAfter := s.IndexCounters()
	assert.Equal(t, rebuildsBefore, rebuildsAfter, "a no-op pass must not increment rebuilds")
}

func TestSearchFastpathStats_CountsContentQueriesByOutcome(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "alpha beta", nil)
	require.NoError(t, err)
	drain(t, s)
	s.reconcileIndex("ns", "proj") // marker == HEAD: the index is healthy
	require.True(t, s.IndexHealthy("ns", "proj"))

	// Content search, healthy index, multi-rune query (has a bigram) -> fastpath.
	_, err = s.SearchFiles("ns", "proj", "alpha", "content")
	require.NoError(t, err)
	fp, fb := s.SearchFastpathStats()
	assert.Equal(t, int64(1), fp)
	assert.Equal(t, int64(0), fb)

	// Filename-only search never reaches the engage/fallback decision -> neither.
	_, err = s.SearchFiles("ns", "proj", "a", "filename")
	require.NoError(t, err)
	fp, fb = s.SearchFastpathStats()
	assert.Equal(t, int64(1), fp)
	assert.Equal(t, int64(0), fb)

	// Content search with a 1-rune query (no bigram) cannot engage -> fallback.
	_, err = s.SearchFiles("ns", "proj", "a", "content")
	require.NoError(t, err)
	fp, fb = s.SearchFastpathStats()
	assert.Equal(t, int64(1), fp)
	assert.Equal(t, int64(1), fb)

	// Content search while the index is unhealthy (a later write advanced HEAD past
	// the marker) -> fallback, even with a multi-rune query.
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "b.md", "gamma", nil)
	require.NoError(t, err)
	drain(t, s)
	require.False(t, s.IndexHealthy("ns", "proj"))
	_, err = s.SearchFiles("ns", "proj", "gamma", "content")
	require.NoError(t, err)
	fp, fb = s.SearchFastpathStats()
	assert.Equal(t, int64(1), fp)
	assert.Equal(t, int64(2), fb)
}

func TestIndexHealthStates_ReflectsPerProjectHealth(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello", nil)
	require.NoError(t, err)
	drain(t, s)

	// Before a sweep advances the marker, the incremental index lags HEAD, so the
	// project is reported not-healthy (errs safe).
	states := s.IndexHealthStates()
	require.Contains(t, states, "ns/proj")
	assert.False(t, states["ns/proj"], "index lagging HEAD must report unhealthy")

	// After a reconcile advances the marker to HEAD, the project is healthy.
	s.reconcileIndex("ns", "proj")
	states = s.IndexHealthStates()
	assert.True(t, states["ns/proj"], "index current with HEAD must report healthy")
}
