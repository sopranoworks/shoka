package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// I2 — the full-text bigram fast path. The centrepiece is the identical-results
// invariant: for any query, the healthy index path returns byte-for-byte the same
// matches, in the same order, with the same snippets, as the fallback full scan.
// The index only narrows which files are read; truth-verify (SearchFiles' exact
// substring check) decides every hit, so a broken/absent index changes speed, not
// results.

// corpus is a small mixed-language project exercising: content + filename matches,
// nested paths (WalkDir order), CJK, a 2-rune file, an empty file, a real match,
// and a bigram FALSE POSITIVE (all of "abcd"'s bigrams present but not contiguous).
var fulltextCorpus = map[string]string{
	"notes/hello.md":    "Hello, World. The quick brown fox.",
	"notes/japanese.md": "日本語のテスト文書です。世界は広い。",
	"mixed.md":          "mixed 日本 and ASCII world",
	"fox.md":            "the fox jumps over",
	"empty.md":          "",
	"ab.md":             "ab", // exactly two runes
	"nested/deep/x.md":  "worldly concerns and brown sugar",
	"fp.md":             "ab xcd bc", // contains ab, bc, cd but not "abcd"
	"real.md":           "prefix-abcd-suffix",
}

var fulltextQueries = []struct{ q, in string }{
	{"world", "content"},
	{"world", "both"},
	{"World", "content"}, // case-insensitive
	{"日本", "content"},
	{"世界", "both"},
	{"テスト", "content"},
	{"fox", "both"},
	{"fox", "filename"},
	{"brown", "content"},
	{"quick brown", "content"}, // multi-bigram phrase
	{"a", "content"},           // <2 runes → short-query fallback
	{"ab", "content"},          // 2-rune query
	{"abcd", "content"},        // bigram false positive (fp.md) + a real hit (real.md)
	{"日本語", "content"},
	{"zzzznomatch", "both"},
	{"and", "both"},
}

func writeCorpus(t *testing.T, s *FSGitStorage, files map[string]string) {
	t.Helper()
	ctx := context.Background()
	for p, c := range files {
		_, err := s.Write(ctx, "sess", "ns", "proj", p, c, nil)
		require.NoError(t, err, "write %s", p)
	}
}

// TestSearchFiles_FastPathIdenticalToFallback is the I2 non-negotiable: healthy
// fast path == fallback full scan, for every query, including matches, order, and
// snippets.
func TestSearchFiles_FastPathIdenticalToFallback(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	writeCorpus(t, s, fulltextCorpus)
	drain(t, s)
	s.reconcileIndex("ns", "proj")
	require.True(t, s.IndexHealthy("ns", "proj"), "precondition: index must be healthy so the fast path engages")

	// Pass 1: healthy index → fast path.
	healthy := make(map[string][]SearchMatch, len(fulltextQueries))
	for _, qq := range fulltextQueries {
		got, err := s.SearchFiles("ns", "proj", qq.q, qq.in)
		require.NoError(t, err, "healthy search %q/%s", qq.q, qq.in)
		healthy[qq.q+"/"+qq.in] = got
	}

	// Drop the index → IndexHealthy false → fallback full scan.
	s.removeIndexFile("ns", "proj")
	require.False(t, s.IndexHealthy("ns", "proj"), "index removed → not healthy → fallback")

	// Pass 2: fallback → must match pass 1 exactly.
	for _, qq := range fulltextQueries {
		fallback, err := s.SearchFiles("ns", "proj", qq.q, qq.in)
		require.NoError(t, err, "fallback search %q/%s", qq.q, qq.in)
		assert.Equal(t, healthy[qq.q+"/"+qq.in], fallback,
			"fast path and fallback must be identical for %q/%s", qq.q, qq.in)
	}
}

// TestSearchFiles_FastPathFindsUnindexedFile proves the gate never causes a false
// negative for a tracked file: a file present on disk but missing from the index
// (a failed best-effort update, while IndexHealthy is still true) is still read and
// matched, because an absent record falls through to the truth scan.
func TestSearchFiles_FastPathFindsUnindexedFile(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	writeCorpus(t, s, map[string]string{"tracked.md": "alpha needle here", "ghost.md": "beta needle there"})
	drain(t, s)
	s.reconcileIndex("ns", "proj")
	require.True(t, s.IndexHealthy("ns", "proj"))

	// Simulate a failed incremental update: ghost.md stays on disk, its record is gone.
	ix := s.indexForRead("ns", "proj")
	require.NotNil(t, ix)
	require.NoError(t, ix.DeleteRecord("ghost.md"))
	require.True(t, s.IndexHealthy("ns", "proj"), "deleting a record does not change marker/HEAD → still healthy")

	got, err := s.SearchFiles("ns", "proj", "needle", "content")
	require.NoError(t, err)
	paths := map[string]bool{}
	for _, m := range got {
		paths[m.Path] = true
	}
	assert.True(t, paths["tracked.md"], "the indexed file must match")
	assert.True(t, paths["ghost.md"], "the unindexed file must STILL match (absent record → read)")
}

// TestSearchFiles_DegradesToFallbackWhenIndexCorrupt proves a corrupt index never
// breaks search: it degrades to the full scan and returns the correct matches.
func TestSearchFiles_DegradesToFallbackWhenIndexCorrupt(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	writeCorpus(t, s, map[string]string{"a.md": "findme please"})
	drain(t, s)

	// Corrupt the on-disk store and drop the handle: a fresh open sees garbage.
	evictIndexHandle(s, "ns", "proj")
	require.NoError(t, os.WriteFile(s.indexPath("ns", "proj"), []byte("not a bbolt db"), 0o600))
	require.False(t, s.IndexHealthy("ns", "proj"), "corrupt store → not healthy")

	got, err := s.SearchFiles("ns", "proj", "findme", "content")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "a.md", got[0].Path)
	assert.NotEmpty(t, got[0].Snippet)
}

// TestSearchFiles_SkipsTransientStagingFiles pins the I2 finding-2 alignment: the
// fallback (and the index corpus) skip .tmp-write-* staging files, so a
// half-written staging file is never a search hit on either path.
func TestSearchFiles_SkipsTransientStagingFiles(t *testing.T) {
	s, dir := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	writeCorpus(t, s, map[string]string{"real.md": "the keyword is here"})

	// A transient atomic-write staging file containing the query must be ignored.
	staging := filepath.Join(dir, "ns", "proj", ".tmp-write-12345")
	require.NoError(t, os.WriteFile(staging, []byte("the keyword leaked"), 0o600))

	got, err := s.SearchFiles("ns", "proj", "keyword", "content")
	require.NoError(t, err)
	for _, m := range got {
		assert.NotContains(t, m.Path, ".tmp-write-", "a transient staging file must never be a search hit")
	}
	require.Len(t, got, 1)
	assert.Equal(t, "real.md", got[0].Path)
}
