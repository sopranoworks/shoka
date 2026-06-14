package storage

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// headHash returns the project's current HEAD commit hash. Combined with drain
// (WAL fully flushed to git), it gives each version a deterministic commit hash
// — WriteFileVersioned returns the content etag, not a commit hash, and
// GetHistory's committer-time ordering can be ambiguous for same-second commits.
func headHash(t *testing.T, s *FSGitStorage, ns, proj string) string {
	t.Helper()
	r, err := git.PlainOpen(projectWTRoot(s, ns, proj))
	require.NoError(t, err)
	h, err := r.Head()
	require.NoError(t, err)
	return h.Hash().String()
}

// writeVer writes content to ns/proj path and returns the resulting commit hash.
func writeVer(t *testing.T, s *FSGitStorage, path, content string) string {
	t.Helper()
	_, err := s.Write(context.Background(), "sess", "ns", "proj", path, content, nil)
	require.NoError(t, err)
	drain(t, s)
	return headHash(t, s, "ns", "proj")
}

// deleteVer deletes ns/proj path and returns the resulting commit hash.
func deleteVer(t *testing.T, s *FSGitStorage, path string) string {
	t.Helper()
	err := s.Delete(context.Background(), "sess", "ns", "proj", path, nil)
	require.NoError(t, err)
	drain(t, s)
	return headHash(t, s, "ns", "proj")
}

// TestDiffVersions_Modified — two versions differing by one line: the hunk has
// the expected equal/delete/add ops at the right line numbers.
func TestDiffVersions_Modified(t *testing.T) {
	s := newTestStorage(t)
	h1 := writeVer(t, s, "a.md", "line1\nline2\nline3\n")
	h2 := writeVer(t, s, "a.md", "line1\nLINE2\nline3\n")

	fd, err := s.DiffVersions(context.Background(), "ns", "proj", "a.md", h1, h2)
	require.NoError(t, err)
	assert.Equal(t, "modified", fd.Status)
	assert.False(t, fd.Binary)
	assert.Empty(t, fd.Suppressed)
	assert.Equal(t, h1, fd.FromHash)
	assert.Equal(t, h2, fd.ToHash)

	require.Len(t, fd.Hunks, 1)
	h := fd.Hunks[0]
	assert.Equal(t, 1, h.OldStart)
	assert.Equal(t, 3, h.OldLines)
	assert.Equal(t, 1, h.NewStart)
	assert.Equal(t, 3, h.NewLines)
	assert.Equal(t, []DiffLine{
		{Op: "equal", Text: "line1"},
		{Op: "delete", Text: "line2"},
		{Op: "add", Text: "LINE2"},
		{Op: "equal", Text: "line3"},
	}, h.Lines)
}

// TestDiffVersions_Added — file present only on the to side ⇒ Status "added",
// a full-add hunk with old side 0,0.
func TestDiffVersions_Added(t *testing.T) {
	s := newTestStorage(t)
	base := writeVer(t, s, "other.md", "x\n") // a.md absent here
	added := writeVer(t, s, "a.md", "n1\nn2\n")

	fd, err := s.DiffVersions(context.Background(), "ns", "proj", "a.md", base, added)
	require.NoError(t, err)
	assert.Equal(t, "added", fd.Status)
	assert.Empty(t, fd.Suppressed)
	require.Len(t, fd.Hunks, 1)
	h := fd.Hunks[0]
	assert.Equal(t, 0, h.OldStart)
	assert.Equal(t, 0, h.OldLines)
	assert.Equal(t, 1, h.NewStart)
	assert.Equal(t, 2, h.NewLines)
	assert.Equal(t, []DiffLine{
		{Op: "add", Text: "n1"},
		{Op: "add", Text: "n2"},
	}, h.Lines)
}

// TestDiffVersions_Deleted — file present only on the from side ⇒ Status
// "deleted", a full-delete hunk with new side 0,0.
func TestDiffVersions_Deleted(t *testing.T) {
	s := newTestStorage(t)
	writeVer(t, s, "keep.md", "k\n") // leave a non-empty tree after delete
	h1 := writeVer(t, s, "a.md", "d1\nd2\n")
	h2 := deleteVer(t, s, "a.md")

	fd, err := s.DiffVersions(context.Background(), "ns", "proj", "a.md", h1, h2)
	require.NoError(t, err)
	assert.Equal(t, "deleted", fd.Status)
	assert.Empty(t, fd.Suppressed)
	require.Len(t, fd.Hunks, 1)
	h := fd.Hunks[0]
	assert.Equal(t, 1, h.OldStart)
	assert.Equal(t, 2, h.OldLines)
	assert.Equal(t, 0, h.NewStart)
	assert.Equal(t, 0, h.NewLines)
	assert.Equal(t, []DiffLine{
		{Op: "delete", Text: "d1"},
		{Op: "delete", Text: "d2"},
	}, h.Lines)
}

// TestDiffVersions_Binary — a blob go-git flags binary (contains a NUL) ⇒
// Binary=true, Suppressed="binary", no hunks; no diff computed.
func TestDiffVersions_Binary(t *testing.T) {
	s := newTestStorage(t)
	h1 := writeVer(t, s, "b.md", "before text\n")
	h2 := writeVer(t, s, "b.md", "with a \x00 nul byte\n")

	fd, err := s.DiffVersions(context.Background(), "ns", "proj", "b.md", h1, h2)
	require.NoError(t, err)
	assert.Equal(t, "modified", fd.Status)
	assert.True(t, fd.Binary)
	assert.Equal(t, "binary", fd.Suppressed)
	assert.Empty(t, fd.Hunks)
}

// TestDiffVersions_TooLarge_ByteCap — a blob over the per-side byte cap ⇒
// Suppressed="too_large", no hunks.
func TestDiffVersions_TooLarge_ByteCap(t *testing.T) {
	s := newTestStorage(t)
	big := strings.Repeat("a\n", maxDiffInputBytes) // ~2x the byte cap, not binary
	h1 := writeVer(t, s, "big.md", "small\n")
	h2 := writeVer(t, s, "big.md", big)

	fd, err := s.DiffVersions(context.Background(), "ns", "proj", "big.md", h1, h2)
	require.NoError(t, err)
	assert.False(t, fd.Binary)
	assert.Equal(t, "too_large", fd.Suppressed)
	assert.Empty(t, fd.Hunks)
}

// TestDiffVersions_TooLarge_LineCap — a diff under the byte cap but over the
// total-line cap ⇒ Suppressed="too_large", no hunks (exercises the line cap
// independently of the byte cap).
func TestDiffVersions_TooLarge_LineCap(t *testing.T) {
	s := newTestStorage(t)
	lines := maxDiffLines + 5000            // > line cap
	content := strings.Repeat("a\n", lines) // ~2*(25005) bytes < byte cap
	require.Less(t, len(content), maxDiffInputBytes, "stay under the byte cap to isolate the line cap")
	base := writeVer(t, s, "other.md", "x\n")
	added := writeVer(t, s, "many.md", content)

	fd, err := s.DiffVersions(context.Background(), "ns", "proj", "many.md", base, added)
	require.NoError(t, err)
	assert.Equal(t, "added", fd.Status)
	assert.Equal(t, "too_large", fd.Suppressed)
	assert.Empty(t, fd.Hunks)
}

// TestDiffVersions_Timeout — an already-cancelled context ⇒ Suppressed="timeout"
// via the clean typed cancellation path, no panic.
func TestDiffVersions_Timeout(t *testing.T) {
	s := newTestStorage(t)
	h1 := writeVer(t, s, "a.md", "x\n")
	h2 := writeVer(t, s, "a.md", "y\n")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	fd, err := s.DiffVersions(ctx, "ns", "proj", "a.md", h1, h2)
	require.NoError(t, err)
	assert.Equal(t, "timeout", fd.Suppressed)
	assert.Empty(t, fd.Hunks)
}

// TestDiffVersions_BadHash — unknown from/to hashes ⇒ a clean typed error, no
// panic.
func TestDiffVersions_BadHash(t *testing.T) {
	s := newTestStorage(t)
	h1 := writeVer(t, s, "a.md", "x\n")
	const zero = "0000000000000000000000000000000000000000" // valid hex, no such object

	_, err := s.DiffVersions(context.Background(), "ns", "proj", "a.md", zero, h1)
	require.Error(t, err)

	_, err = s.DiffVersions(context.Background(), "ns", "proj", "a.md", h1, zero)
	require.Error(t, err)

	_, err = s.DiffVersions(context.Background(), "ns", "proj", "a.md", "not-a-hash", h1)
	require.Error(t, err)
}

// TestDiffVersions_ConcurrentWithWrites — THE CRUX: DiffVersions over an
// immutable commit pair runs concurrently with active writes (and their
// background WAL commits) to the SAME project, under -race. Every diff returns
// the identical, correct result (immutable inputs ⇒ no torn read), and BOTH the
// writer and the differ run to completion — proving neither blocks nor is
// blocked through any shared lock. Real post-conditions (B-29), no sleeps.
func TestDiffVersions_ConcurrentWithWrites(t *testing.T) {
	s := newTestStorage(t)
	writeVer(t, s, "keep.md", "k\n")
	h1 := writeVer(t, s, "a.md", "alpha\nbeta\ngamma\n")
	h2 := writeVer(t, s, "a.md", "alpha\nBETA\ngamma\n")

	// The invariant the concurrent differ must keep reproducing.
	want, err := s.DiffVersions(context.Background(), "ns", "proj", "a.md", h1, h2)
	require.NoError(t, err)
	require.Equal(t, "modified", want.Status)
	require.Len(t, want.Hunks, 1)

	const writes = 200
	const diffs = 200
	var writesDone, diffsDone atomic.Int64
	var wg sync.WaitGroup

	// Writer: drive writes + background commits to the same project/path.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < writes; i++ {
			_, werr := s.Write(context.Background(), "sess", "ns", "proj", "a.md",
				fmt.Sprintf("alpha\nbeta-%d\ngamma\n", i), nil)
			assert.NoError(t, werr)
			writesDone.Add(1)
		}
	}()

	// Differ: repeatedly diff the immutable pair; result must never change.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < diffs; i++ {
			got, derr := s.DiffVersions(context.Background(), "ns", "proj", "a.md", h1, h2)
			assert.NoError(t, derr)
			assert.Equal(t, want, got, "diff over immutable commits must be invariant under concurrent writes")
			diffsDone.Add(1)
		}
	}()

	wg.Wait()
	assert.Equal(t, int64(writes), writesDone.Load(), "every write completed — diff never blocked the writer")
	assert.Equal(t, int64(diffs), diffsDone.Load(), "every diff completed — writes never blocked the differ")
	assertNoViolations(t, s, "ns", "proj")
}
