package storage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dirExists reports whether p exists and is a directory.
func dirExists(t *testing.T, p string) bool {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	return info.IsDir()
}

// B-48: deleting the last file from a/b/c/foo.md reaps the DIRECT parent (c) only.
// The grandparent (a/b) is left for a later operation or the sweep backstop — no
// chain ascent in the same operation.
func TestReap_DeleteRemovesEmptyParentOneLevel(t *testing.T) {
	s := makeProjectStore(t, "proj")
	ctx := context.Background()
	root := projectWTRoot(s, "ns", "proj")

	_, err := s.Write(ctx, "sess", "ns", "proj", "a/b/c/foo.md", "hello", nil)
	require.NoError(t, err)
	require.True(t, dirExists(t, filepath.Join(root, "a", "b", "c")))

	require.NoError(t, s.Delete(ctx, "sess", "ns", "proj", "a/b/c/foo.md", nil))

	assert.False(t, dirExists(t, filepath.Join(root, "a", "b", "c")), "direct parent c reaped")
	assert.True(t, dirExists(t, filepath.Join(root, "a", "b")), "grandparent a/b left for a later pass (one level only)")
	assert.True(t, dirExists(t, filepath.Join(root, "a")), "great-grandparent a left")
	assert.True(t, dirExists(t, root), "project root never reaped")
}

// B-48: moving the last file out of a/b/c reaps the SOURCE's direct parent (c),
// one level only; the destination's directory is created and kept.
func TestReap_MoveRemovesSourceParentOneLevel(t *testing.T) {
	s := makeProjectStore(t, "proj")
	ctx := context.Background()
	root := projectWTRoot(s, "ns", "proj")

	_, err := s.Write(ctx, "sess", "ns", "proj", "a/b/c/foo.md", "hello", nil)
	require.NoError(t, err)

	_, _, err = s.Move(ctx, "sess", "ns", "proj", "a/b/c/foo.md", "x/y.md", nil)
	require.NoError(t, err)

	assert.False(t, dirExists(t, filepath.Join(root, "a", "b", "c")), "source direct parent c reaped")
	assert.True(t, dirExists(t, filepath.Join(root, "a", "b")), "source grandparent left (one level only)")
	assert.True(t, dirExists(t, filepath.Join(root, "x")), "destination dir created and kept")
	content, _, err := s.ReadFileWithETag("ns", "proj", "x/y.md")
	require.NoError(t, err)
	assert.Equal(t, "hello", content)
}

// B-48 rm semantics: a move within the same directory leaves that directory in
// place — the source-parent reap finds it non-empty (it now holds the target)
// and no-ops.
func TestReap_MoveWithinSameDirKeepsDir(t *testing.T) {
	s := makeProjectStore(t, "proj")
	ctx := context.Background()
	root := projectWTRoot(s, "ns", "proj")

	_, err := s.Write(ctx, "sess", "ns", "proj", "d/foo.md", "hi", nil)
	require.NoError(t, err)
	_, _, err = s.Move(ctx, "sess", "ns", "proj", "d/foo.md", "d/bar.md", nil)
	require.NoError(t, err)

	assert.True(t, dirExists(t, filepath.Join(root, "d")), "dir kept: still holds the moved-to file")
	content, _, err := s.ReadFileWithETag("ns", "proj", "d/bar.md")
	require.NoError(t, err)
	assert.Equal(t, "hi", content)
}

// B-48 rm semantics: deleting one file from a directory that still holds a
// sibling is a reap no-op (ENOTEMPTY) — the directory survives.
func TestReap_DeleteSiblingKeepsParent(t *testing.T) {
	s := makeProjectStore(t, "proj")
	ctx := context.Background()
	root := projectWTRoot(s, "ns", "proj")

	_, err := s.Write(ctx, "sess", "ns", "proj", "d/a.md", "a", nil)
	require.NoError(t, err)
	_, err = s.Write(ctx, "sess", "ns", "proj", "d/b.md", "b", nil)
	require.NoError(t, err)

	require.NoError(t, s.Delete(ctx, "sess", "ns", "proj", "d/a.md", nil))

	assert.True(t, dirExists(t, filepath.Join(root, "d")), "dir kept: sibling b.md remains")
	content, _, err := s.ReadFileWithETag("ns", "proj", "d/b.md")
	require.NoError(t, err)
	assert.Equal(t, "b", content)
}

// B-48: deleting a file at the project root never removes the project root.
func TestReap_NeverRemovesProjectRoot(t *testing.T) {
	s := makeProjectStore(t, "proj")
	ctx := context.Background()
	root := projectWTRoot(s, "ns", "proj")

	_, err := s.Write(ctx, "sess", "ns", "proj", "top.md", "x", nil)
	require.NoError(t, err)
	require.NoError(t, s.Delete(ctx, "sess", "ns", "proj", "top.md", nil))

	assert.True(t, dirExists(t, root), "project root survives even when it holds no files")
}

// B-48: reapableDir guards the root, escapes, and derivative/quarantine dirs.
func TestReapableDir_Guards(t *testing.T) {
	root := filepath.Join("base", "ns", "proj")
	assert.False(t, reapableDir(root, root), "never the project root")
	assert.False(t, reapableDir(root, filepath.Dir(root)), "never above the root")
	assert.False(t, reapableDir(root, filepath.Join(root, ".git")), ".git skipped")
	assert.False(t, reapableDir(root, filepath.Join(root, ".git", "objects")), "beneath .git skipped")
	assert.False(t, reapableDir(root, filepath.Join(root, ".shoka")), ".shoka skipped")
	assert.False(t, reapableDir(root, filepath.Join(root, ".drafts")), ".drafts skipped")
	assert.True(t, reapableDir(root, filepath.Join(root, "docs")), "plain data dir reapable")
	assert.True(t, reapableDir(root, filepath.Join(root, "a", "b", "c")), "nested data dir reapable")
}

// B-48 backstop: the lost+found sweep reaps a pre-existing empty directory the
// operation-time reap never saw, riding the existing per-project walk. A chain
// collapses one level per pass; the root always survives.
func TestReap_SweepBackstopRemovesEmptyDirs(t *testing.T) {
	s := makeProjectStore(t, "proj")
	root := projectWTRoot(s, "ns", "proj")

	// A lone empty dir and a deeper empty chain, both created out-of-band.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "stale"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "x", "y", "z"), 0o755))

	// One pass removes every leaf; non-leaf candidates no-op (ENOTEMPTY) this pass.
	s.sweepProject("ns", "proj")
	assert.False(t, dirExists(t, filepath.Join(root, "stale")), "lone empty dir reaped in one pass")
	assert.False(t, dirExists(t, filepath.Join(root, "x", "y", "z")), "deepest leaf reaped in one pass")
	assert.True(t, dirExists(t, filepath.Join(root, "x", "y")), "non-leaf left this pass (one level per pass)")

	// Subsequent passes collapse the rest of the chain, gently.
	for i := 0; i < 5 && dirExists(t, filepath.Join(root, "x")); i++ {
		s.sweepProject("ns", "proj")
	}
	assert.False(t, dirExists(t, filepath.Join(root, "x")), "chain fully collapsed over passes")
	assert.True(t, dirExists(t, root), "project root never reaped by the sweep")
}

// B-48: a directory holding a real file is never reaped by the sweep.
func TestReap_SweepKeepsNonEmptyDir(t *testing.T) {
	s := makeProjectStore(t, "proj")
	root := projectWTRoot(s, "ns", "proj")

	_, err := s.Write(context.Background(), "sess", "ns", "proj", "keep/me.md", "x", nil)
	require.NoError(t, err)
	s.sweepProject("ns", "proj")
	assert.True(t, dirExists(t, filepath.Join(root, "keep")), "dir with a file survives the sweep")
}

// B-48 the load-bearing concurrency proof: an empty-dir reap and a write into the
// SAME parent are serialised by the directory-scoped lock with rm semantics, so
// BOTH interleavings are correct and a concurrent writer NEVER gets a spurious
// error (no ENOENT from the closed MkdirAll→CreateTemp window). Every goroutine
// hammers one shared parent by writing a unique file then deleting it, so the
// parent oscillates empty↔non-empty under maximum contention. Run under -race.
func TestReap_ConcurrentReapVsWriteNoSpuriousError(t *testing.T) {
	// A generous WAL cap so the background commit worker is never the bottleneck —
	// this test exercises the reap/write lock interaction, not WAL backpressure.
	s, _ := newStore(t, Options{WALMaxEntries: 100000})
	require.NoError(t, s.CreateProject("ns", "proj"))
	ctx := context.Background()

	const goroutines = 8
	const iters = 150

	var failures atomic.Int64
	var firstErr atomic.Value // string
	record := func(stage string, err error) {
		failures.Add(1)
		firstErr.CompareAndSwap(nil, fmt.Sprintf("%s: %v", stage, err))
	}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for j := 0; j < iters; j++ {
				path := fmt.Sprintf("hot/g%d-%d.md", g, j)
				if _, err := s.Write(ctx, "sess", "ns", "proj", path, "x", nil); err != nil {
					record("write", err) // a spurious ENOENT would land here
					continue
				}
				if err := s.Delete(ctx, "sess", "ns", "proj", path, nil); err != nil {
					record("delete", err)
				}
			}
		}(g)
	}
	wg.Wait()

	assert.Equal(t, int64(0), failures.Load(),
		"no operation may fail under reap/write contention; first: %v", firstErr.Load())
	assertNoViolations(t, s, "ns", "proj")
}
