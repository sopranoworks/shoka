package storage

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// I1 §5.5 — IndexHealthy: false while stale/repairing/absent/corrupt; true only
// when the store is open and its marker equals HEAD. No caller wires it in I1.

func TestIndexHealthy_FalseWhenAbsent(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "x", nil)
	require.NoError(t, err)
	drain(t, s)

	// Remove the store entirely: no store → not healthy.
	evictIndexHandle(s, "ns", "proj")
	require.NoError(t, os.Remove(s.indexPath("ns", "proj")))
	assert.False(t, s.IndexHealthy("ns", "proj"))
}

func TestIndexHealthy_FalseWhenStale(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "x", nil)
	require.NoError(t, err)
	drain(t, s) // HEAD advanced; the incremental marker still lags ("")

	// Stale: marker != HEAD.
	assert.False(t, s.IndexHealthy("ns", "proj"))
}

func TestIndexHealthy_TrueWhenCurrent(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "x", nil)
	require.NoError(t, err)
	drain(t, s)

	require.False(t, s.IndexHealthy("ns", "proj"), "precondition: stale before reconcile")
	s.reconcileIndex("ns", "proj") // advances the marker to HEAD
	assert.True(t, s.IndexHealthy("ns", "proj"), "current after reconcile")
}

func TestIndexHealthy_FalseWhenCorrupt(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "x", nil)
	require.NoError(t, err)
	drain(t, s)

	evictIndexHandle(s, "ns", "proj")
	require.NoError(t, os.WriteFile(s.indexPath("ns", "proj"), []byte("garbage"), 0o600))
	assert.False(t, s.IndexHealthy("ns", "proj"), "a corrupt store is not healthy")
}

// Inertness (§6.6): the substring search fallback is untouched and works the same
// with the index present — the index changes no query behaviour in I1.
func TestI1_Inert_SearchFallbackUnchanged(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "note.md", "the quick brown fox", nil)
	require.NoError(t, err)
	drain(t, s)
	s.reconcileIndex("ns", "proj") // even with a fully healthy index...

	// ...search still uses the truth-scan: content + filename matches both work.
	hits, err := s.SearchFiles("ns", "proj", "brown", "content")
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "note.md", hits[0].Path)

	hits2, err := s.SearchFiles("ns", "proj", "note", "filename")
	require.NoError(t, err)
	require.Len(t, hits2, 1)
}
