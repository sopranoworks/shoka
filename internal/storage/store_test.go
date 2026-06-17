package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/storage/wal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newStore creates storage in a temp dir with the given options and registers
// Close cleanup. It does not create any project.
func newStore(t *testing.T, opts Options) (*FSGitStorage, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewFSGitStorageWithOptions(dir, opts)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

func contentSHA(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func TestStore_ReadReturnsContentAndETag(t *testing.T) {
	s := newTestStorage(t)
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "hello", nil)
	require.NoError(t, err)

	content, etag, err := s.ReadFileWithETag("ns", "proj", "a.md")
	require.NoError(t, err)
	assert.Equal(t, "hello", content)
	assert.Equal(t, contentSHA("hello"), etag, "etag must be sha256 of content")
}

func TestStore_WriteBasicAndIfMatch(t *testing.T) {
	s := newTestStorage(t)

	etag1, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "v1", nil)
	require.NoError(t, err)
	assert.Equal(t, contentSHA("v1"), etag1)

	readContent, readEtag, err := s.ReadFileWithETag("ns", "proj", "a.md")
	require.NoError(t, err)
	assert.Equal(t, "v1", readContent)
	assert.Equal(t, etag1, readEtag)

	// Stale if_match fails with a conflict carrying the current etag.
	stale := contentSHA("nope")
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "a.md", "v2", &stale)
	var conflict *VersionConflictError
	require.ErrorAs(t, err, &conflict)
	assert.Equal(t, etag1, conflict.Current)

	// Correct if_match succeeds and returns the new etag.
	etag2, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "v2", &etag1)
	require.NoError(t, err)
	assert.Equal(t, contentSHA("v2"), etag2)
}

func TestStore_ConcurrentWritesDistinctFiles(t *testing.T) {
	s := newTestStorage(t) // project ns/proj exists
	const projects = 10
	const perProject = 2
	for i := 0; i < projects; i++ {
		require.NoError(t, s.CreateProject("ns", fmt.Sprintf("p%d", i)))
	}

	var wg sync.WaitGroup
	errs := make(chan error, projects*perProject)
	start := time.Now()
	for i := 0; i < projects; i++ {
		for j := 0; j < perProject; j++ {
			wg.Add(1)
			go func(i, j int) {
				defer wg.Done()
				_, err := s.Write(context.Background(), fmt.Sprintf("s%d", i),
					"ns", fmt.Sprintf("p%d", i), fmt.Sprintf("f%d.md", j), fmt.Sprintf("c-%d-%d", i, j), nil)
				errs <- err
			}(i, j)
		}
	}
	wg.Wait()
	close(errs)
	elapsed := time.Since(start)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Less(t, elapsed, 5*time.Second, "writes should be bounded (lock-free reads, brief write lock)")

	drain(t, s)
	// Every file readable with the right content.
	for i := 0; i < projects; i++ {
		for j := 0; j < perProject; j++ {
			c, _, err := s.ReadFileWithETag("ns", fmt.Sprintf("p%d", i), fmt.Sprintf("f%d.md", j))
			require.NoError(t, err)
			assert.Equal(t, fmt.Sprintf("c-%d-%d", i, j), c)
		}
	}
}

func TestStore_ListFilesReturnsModTimes(t *testing.T) {
	s := newTestStorage(t) // project ns/proj exists

	// Write one file, then a second strictly later (explicit mtimes via Chtimes
	// so the ordering assertion does not depend on wall-clock resolution).
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "first.md", "1", nil)
	require.NoError(t, err)
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "second.md", "2", nil)
	require.NoError(t, err)

	projDir := filepath.Join(s.baseDir, "ns", "proj")
	t1 := time.Now().Add(-2 * time.Hour)
	t2 := time.Now().Add(-1 * time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(projDir, "first.md"), t1, t1))
	require.NoError(t, os.Chtimes(filepath.Join(projDir, "second.md"), t2, t2))

	files, modTimes, err := s.ListFiles("ns", "proj", "")
	require.NoError(t, err)
	assert.Contains(t, files, "first.md")
	assert.Contains(t, files, "second.md")

	// Every listed entry has a non-zero mtime in the map.
	for _, f := range files {
		mt, ok := modTimes[f]
		require.True(t, ok, "modTimes must have an entry for %q", f)
		assert.False(t, mt.IsZero(), "mtime for %q must be non-zero", f)
	}

	// The later-written file has a strictly greater mtime.
	assert.True(t, modTimes["second.md"].After(modTimes["first.md"]),
		"second.md mtime (%s) should be after first.md (%s)", modTimes["second.md"], modTimes["first.md"])
}

func TestStore_ListFilesModTimeIncludesDirectories(t *testing.T) {
	s := newTestStorage(t)
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "sub/inner.md", "x", nil)
	require.NoError(t, err)

	files, modTimes, err := s.ListFiles("ns", "proj", "")
	require.NoError(t, err)
	require.Contains(t, files, "sub/", "directory entry should carry a trailing slash")
	mt, ok := modTimes["sub/"]
	require.True(t, ok, "directory entries must appear in modTimes keyed with the trailing slash")
	assert.False(t, mt.IsZero())
}

func TestStore_WriteDisabledWhenWALFull(t *testing.T) {
	s := newTestStorage(t)
	// Stop the worker so the WAL cannot drain, then stuff it past the threshold.
	require.NoError(t, s.pool.Shutdown(5*time.Second))
	for i := 0; i < s.maxWALEntries; i++ {
		_, err := s.wal.Append(wal.Entry{Namespace: "ns", Project: "proj", Path: fmt.Sprintf("f%d", i), Op: "write", Content: []byte("x")})
		require.NoError(t, err)
	}
	require.GreaterOrEqual(t, s.WALPending(), s.maxWALEntries)
	assert.True(t, s.WALWriteDisabled())

	_, err := s.Write(context.Background(), "sess", "ns", "proj", "blocked.md", "x", nil)
	assert.ErrorIs(t, err, ErrWriteDisabled)
}

func TestStore_CorruptedStateAndRecovery(t *testing.T) {
	s, dir := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "original", nil)
	require.NoError(t, err)
	drain(t, s)

	// Modify the working tree directly, bypassing Shoka's write path.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ns", "proj", "a.md"), []byte("hand-edited"), 0o644))

	sum, err := s.DetectDrift("ns", "proj")
	require.NoError(t, err)
	assert.Equal(t, StateCorrupted, sum.State)
	assert.Contains(t, sum.Modified, "a.md")
	assert.Equal(t, StateCorrupted, s.State("ns", "proj"))

	// Writes are refused while corrupted.
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "b.md", "x", nil)
	assert.ErrorIs(t, err, ErrProjectCorrupted)

	// Recovery (A) adopts the working tree's tracked changes and returns to healthy.
	_, rerr := s.RepairTrackedChanges(context.Background(), "ns", "proj")
	require.NoError(t, rerr)
	assert.Equal(t, StateHealthy, s.State("ns", "proj"))
	sum2, err := s.DetectDrift("ns", "proj")
	require.NoError(t, err)
	assert.Equal(t, StateHealthy, sum2.State)

	// Writes work again.
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "b.md", "x", nil)
	require.NoError(t, err)
}

func TestStore_DangerousStateAndRecovery(t *testing.T) {
	s, dir := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "original", nil)
	require.NoError(t, err)
	drain(t, s)

	// Make .git unreadable by moving it out of the project entirely.
	gitDir := filepath.Join(dir, "ns", "proj", ".git")
	require.NoError(t, os.Rename(gitDir, filepath.Join(dir, "git-broken")))

	sum, err := s.DetectDrift("ns", "proj")
	require.NoError(t, err)
	assert.Equal(t, StateDangerous, sum.State)

	// All write/read operations are refused while dangerous.
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "b.md", "x", nil)
	assert.ErrorIs(t, err, ErrProjectDangerous)
	_, _, err = s.ReadFileWithETag("ns", "proj", "a.md")
	assert.ErrorIs(t, err, ErrProjectDangerous)

	// restore-to-latest is not allowed for a dangerous project.
	assert.Error(t, s.RestoreToLatest(context.Background(), "ns", "proj"))

	// Recovery (A) re-initialises .git and returns to healthy. (Tracked-only: the
	// re-init has no HEAD, so nothing is adopted; the project is healthy with its
	// prior files left untracked on disk — bootstrapping a bare tree's content is
	// the later lost+found/shoka.ignore directive's job.)
	_, rerr := s.RepairTrackedChanges(context.Background(), "ns", "proj")
	require.NoError(t, rerr)
	assert.Equal(t, StateHealthy, s.State("ns", "proj"))
}

func TestStore_WALRecoveryOnRestart(t *testing.T) {
	dir := t.TempDir()

	// First instance: create the project, then simulate a crash by stopping the
	// worker and leaving 5 entries pending in the WAL.
	s1, err := NewFSGitStorageWithOptions(dir, Options{})
	require.NoError(t, err)
	require.NoError(t, s1.CreateProject("ns", "proj"))
	require.NoError(t, s1.pool.Shutdown(5*time.Second))
	for i := 0; i < 5; i++ {
		_, err := s1.wal.Append(wal.Entry{
			Namespace: "ns", Project: "proj", Path: fmt.Sprintf("f%d.md", i), Op: "write", Content: []byte(fmt.Sprintf("content-%d", i)),
		})
		require.NoError(t, err)
	}
	require.Equal(t, 5, s1.WALPending())
	s1.locks.Stop()
	_ = s1.wal.Close()
	// A real crash is a process exit, which the OS uses to release every file lock the
	// process held; simulate that for the eagerly-opened namespace registry (a bbolt store)
	// so the second in-process instance below does not block on s1's still-held lock.
	_ = s1.nsReg.Close()

	// Second instance over the same base dir: the WAL should drain into git.
	s2, err := NewFSGitStorageWithOptions(dir, Options{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	require.True(t, s2.WaitForWAL(10*time.Second), "WAL did not drain on restart")
	assert.Equal(t, 0, s2.WALPending())

	hist, err := s2.GetHistory("ns", "proj", "", 0)
	require.NoError(t, err)
	assert.Len(t, hist, 5, "all five recovered WAL entries should appear as commits")
}
