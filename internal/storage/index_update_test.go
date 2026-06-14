package storage

import (
	"context"
	"os"
	"testing"

	"github.com/sopranoworks/shoka/internal/storage/index"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// I1 §5.3 — the synchronous best-effort index update at the catalogPut / Move /
// delete hooks. The index tracks the catalog op; an injected index-update failure
// never fails the write and leaves the index stale; the catalog op stays
// authoritative.

// idxRecord opens the project's index read-only and returns the record at rel.
func idxRecord(t *testing.T, s *FSGitStorage, ns, proj, rel string) (index.IndexRecord, bool) {
	t.Helper()
	ix := s.indexForRead(ns, proj)
	require.NotNil(t, ix, "index should be openable")
	rec, ok, err := ix.GetRecord(rel)
	require.NoError(t, err)
	return rec, ok
}

func TestIndexUpdate_WriteTracksTheRecord(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))

	etag, err := s.Write(context.Background(), "sess", "ns", "proj", "dir/a.md", "hello", nil)
	require.NoError(t, err)

	rec, ok := idxRecord(t, s, "ns", "proj", "dir/a.md")
	require.True(t, ok, "index must hold the written path")
	assert.Equal(t, etag, rec.Etag, "index record etag must match the write's etag")
}

func TestIndexUpdate_DeleteRemovesTheRecord(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "x", nil)
	require.NoError(t, err)
	if _, ok := idxRecord(t, s, "ns", "proj", "a.md"); !ok {
		t.Fatal("precondition: index should hold a.md after write")
	}

	require.NoError(t, s.Delete(context.Background(), "sess", "ns", "proj", "a.md", nil))
	if _, ok := idxRecord(t, s, "ns", "proj", "a.md"); ok {
		t.Error("index still holds a.md after delete")
	}
}

func TestIndexUpdate_MoveDisownsSourceAdoptsDest(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "src.md", "body", nil)
	require.NoError(t, err)

	newEtag, _, err := s.Move(context.Background(), "sess", "ns", "proj", "src.md", "dst.md", nil)
	require.NoError(t, err)

	if _, ok := idxRecord(t, s, "ns", "proj", "src.md"); ok {
		t.Error("index still holds the move source")
	}
	rec, ok := idxRecord(t, s, "ns", "proj", "dst.md")
	require.True(t, ok, "index must hold the move destination")
	assert.Equal(t, newEtag, rec.Etag)
}

// A corrupt index.db makes every indexFor fail, but the write still succeeds and
// the working tree/catalog are intact. The failure is counted, not surfaced.
func TestIndexUpdate_FailureNeverFailsTheWrite(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))

	// Inject a corrupt index store: garbage at the index path so index.Open returns
	// ErrCorrupt (not ErrNotFound), so indexFor never creates/opens a usable handle.
	require.NoError(t, os.WriteFile(s.indexPath("ns", "proj"), []byte("not a bbolt db"), 0o600))

	etag, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello", nil)
	require.NoError(t, err, "a broken index must never fail the write")
	require.NotEmpty(t, etag)

	// The write is fully durable: content readable, catalog tracks it.
	content, _, rerr := s.ReadFileWithETag("ns", "proj", "a.md")
	require.NoError(t, rerr)
	assert.Equal(t, "hello", content)

	// The index-update failure was counted.
	w, _, _ := s.IndexCounters()
	assert.GreaterOrEqual(t, w, int64(1), "the failed index update must be counted")
}

// The catalog op runs and is authoritative even when the index update fails: with
// the index corrupt, list_files (catalog-backed) still shows the written file.
func TestIndexUpdate_CatalogStaysAuthoritativeWhenIndexBroken(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	require.NoError(t, os.WriteFile(s.indexPath("ns", "proj"), []byte("garbage"), 0o600))

	_, err := s.Write(context.Background(), "sess", "ns", "proj", "kept.md", "v", nil)
	require.NoError(t, err)

	names, _, err := s.ListFiles("ns", "proj", "")
	require.NoError(t, err)
	assert.Contains(t, names, "kept.md", "catalog-backed listing must include the file despite the broken index")
}

// TestIndexUpdate_WriteStoresBigrams (I2 §3.2) — the write hook derives the file's
// full-text bigram set from the content it already carries, matching index.Bigrams.
func TestIndexUpdate_WriteStoresBigrams(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello world", nil)
	require.NoError(t, err)

	rec, ok := idxRecord(t, s, "ns", "proj", "a.md")
	require.True(t, ok)
	assert.Equal(t, index.Bigrams("hello world"), rec.Bigrams, "the write hook must store the content's bigrams")
}
