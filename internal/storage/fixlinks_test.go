package storage

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// I3 §3.2 — fix_links, the asynchronous write-to-truth edge. move_file stays a
// pure rename; fix_links is the worker-driven reconciliation that repairs
// referrers after a move, as ordinary if_match writes that back off on conflict
// and converge. These tests drive fixLinks directly (like the sweep tests drive
// reconcileIndex), not through the kick goroutine.

func readBody(t *testing.T, s *FSGitStorage, ns, proj, rel string) string {
	t.Helper()
	body, _, err := s.ReadFileWithETag(ns, proj, rel)
	require.NoError(t, err)
	return body
}

// makeIndexHealthy drains the WAL and reconciles the index so its marker == HEAD
// (IndexHealthy true), the precondition for fix_links' index-driven referrer
// lookup.
func makeIndexHealthy(t *testing.T, s *FSGitStorage, ns, proj string) {
	t.Helper()
	drain(t, s)
	s.reconcileIndex(ns, proj)
	require.True(t, s.IndexHealthy(ns, proj), "index should be healthy after reconcile")
}

// Healthy path: a post-move fix_links finds referrers via the reverse-link index
// and rewrites each one's link to point at the destination.
func TestFixLinks_HealthyIndexRepairsReferrers(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "ref.md", "see [t](old.md)", nil)
	require.NoError(t, err)
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "old.md", "# Old", nil)
	require.NoError(t, err)

	_, _, err = s.Move(context.Background(), "sess", "ns", "proj", "old.md", "new.md", nil)
	require.NoError(t, err)
	makeIndexHealthy(t, s, "ns", "proj")
	require.True(t, s.IndexHealthy("ns", "proj"))

	s.fixLinks(context.Background(), "ns", "proj", "old.md", "new.md")

	assert.Equal(t, "see [t](new.md)", readBody(t, s, "ns", "proj", "ref.md"),
		"the referrer's link must be repaired to point at the destination")
}

// Not-healthy path: with the index unavailable (unhealthy), fix_links truth-scans
// via discoverReferrers and repairs correctly — repair is independent of index
// health (the index is an optional speedup, never a precondition).
func TestFixLinks_NotHealthyTruthScanRepairs(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "ref.md", "see [t](old.md)", nil)
	require.NoError(t, err)
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "old.md", "# Old", nil)
	require.NoError(t, err)
	_, _, err = s.Move(context.Background(), "sess", "ns", "proj", "old.md", "new.md", nil)
	require.NoError(t, err)

	// Index never reconciled → marker lags HEAD → IndexHealthy is false.
	require.False(t, s.IndexHealthy("ns", "proj"))

	s.fixLinks(context.Background(), "ns", "proj", "old.md", "new.md")

	assert.Equal(t, "see [t](new.md)", readBody(t, s, "ns", "proj", "ref.md"),
		"truth-scan must repair the referrer even with no healthy index")
}

// A corrupt index store (the most explicit "broken index") must never drive a
// rewrite: fix_links falls to the truth-scan and repairs correctly, never missing
// or over-rewriting from partial referrer knowledge.
func TestFixLinks_BrokenIndexNeverRewritesWrong(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "ref.md", "[t](old.md)", nil)
	require.NoError(t, err)
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "other.md", "no link here", nil)
	require.NoError(t, err)
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "old.md", "# Old", nil)
	require.NoError(t, err)
	_, _, err = s.Move(context.Background(), "sess", "ns", "proj", "old.md", "new.md", nil)
	require.NoError(t, err)

	// Corrupt the on-disk index so IndexHealthy is false and Referrers is unusable.
	evictIndexHandle(s, "ns", "proj")
	require.NoError(t, os.WriteFile(s.indexPath("ns", "proj"), []byte("not a bbolt db"), 0o600))
	require.False(t, s.IndexHealthy("ns", "proj"))

	s.fixLinks(context.Background(), "ns", "proj", "old.md", "new.md")

	assert.Equal(t, "[t](new.md)", readBody(t, s, "ns", "proj", "ref.md"), "the real referrer is repaired via truth-scan")
	assert.Equal(t, "no link here", readBody(t, s, "ns", "proj", "other.md"), "a non-referrer is never touched")
}

// Idempotent: re-running fix_links after the links already point at the
// destination rewrites nothing.
func TestFixLinks_Idempotent(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "ref.md", "[t](old.md)", nil)
	require.NoError(t, err)
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "old.md", "# Old", nil)
	require.NoError(t, err)
	_, _, err = s.Move(context.Background(), "sess", "ns", "proj", "old.md", "new.md", nil)
	require.NoError(t, err)
	makeIndexHealthy(t, s, "ns", "proj")

	s.fixLinks(context.Background(), "ns", "proj", "old.md", "new.md")
	first := readBody(t, s, "ns", "proj", "ref.md")
	require.Equal(t, "[t](new.md)", first)

	_, beforeEtag, err := s.ReadFileWithETag("ns", "proj", "ref.md")
	require.NoError(t, err)
	s.fixLinks(context.Background(), "ns", "proj", "old.md", "new.md") // second run
	_, afterEtag, err := s.ReadFileWithETag("ns", "proj", "ref.md")
	require.NoError(t, err)

	assert.Equal(t, first, readBody(t, s, "ns", "proj", "ref.md"), "content unchanged on re-run")
	assert.Equal(t, beforeEtag, afterEtag, "a re-run must not produce a new write")
}

// Concurrent edit: if the referrer changes between fix_links reading it and the
// if_match write, the write is rejected as a conflict and fix_links backs off —
// the concurrent edit survives, it is never clobbered. The file lock provides the
// write-ordering guarantee; the brief pause only lets fix_links' lock-free read
// complete first.
func TestFixLinks_ConflictDoesNotClobberConcurrentEdit(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "ref.md", "[t](old.md)", nil)
	require.NoError(t, err)
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "old.md", "# Old", nil)
	require.NoError(t, err)
	_, _, err = s.Move(context.Background(), "sess", "ns", "proj", "old.md", "new.md", nil)
	require.NoError(t, err)
	makeIndexHealthy(t, s, "ns", "proj")

	projectPath, err := s.getProjectPath("ns", "proj")
	require.NoError(t, err)
	refFull := projectPath + "/ref.md"

	const concurrent = "EDITED concurrently, still [t](old.md)"
	done := make(chan struct{})
	// Hold ref.md's lock; while held, launch fix_links (it reads ref.md lock-free,
	// then blocks on this lock for its write), then overwrite ref.md so the locked
	// re-read sees content whose etag differs from what fix_links read.
	lerr := s.locks.WithLock(context.Background(), "test", refFull, func() error {
		go func() {
			s.fixLinks(context.Background(), "ns", "proj", "old.md", "new.md")
			close(done)
		}()
		time.Sleep(50 * time.Millisecond) // let fix_links' lock-free read happen
		return atomicWriteFile(refFull, []byte(concurrent))
	})
	require.NoError(t, lerr)
	<-done

	assert.Equal(t, concurrent, readBody(t, s, "ns", "proj", "ref.md"),
		"a concurrent edit must survive: fix_links backs off on if_match conflict, never clobbers")
}

// Termination on circular references: A links B and B links A; both are moved and
// both kicked. fix_links must converge (no kick spawns another kick, rewriteLinks
// is idempotent), so the calls return and the final links are correct.
func TestFixLinks_CircularReferencesTerminate(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "a.md", "to [b](b.md)", nil)
	require.NoError(t, err)
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "b.md", "to [a](a.md)", nil)
	require.NoError(t, err)

	_, _, err = s.Move(context.Background(), "sess", "ns", "proj", "a.md", "a2.md", nil)
	require.NoError(t, err)
	_, _, err = s.Move(context.Background(), "sess", "ns", "proj", "b.md", "b2.md", nil)
	require.NoError(t, err)
	makeIndexHealthy(t, s, "ns", "proj")

	// Both kicks; each must return (no infinite loop).
	doneA := make(chan struct{})
	go func() { s.fixLinks(context.Background(), "ns", "proj", "a.md", "a2.md"); close(doneA) }()
	select {
	case <-doneA:
	case <-time.After(5 * time.Second):
		t.Fatal("fix_links(a) did not terminate")
	}
	doneB := make(chan struct{})
	go func() { s.fixLinks(context.Background(), "ns", "proj", "b.md", "b2.md"); close(doneB) }()
	select {
	case <-doneB:
	case <-time.After(5 * time.Second):
		t.Fatal("fix_links(b) did not terminate")
	}

	// b2.md referenced a.md → now a2.md; a2.md referenced b.md → now b2.md.
	assert.Equal(t, "to [a](a2.md)", readBody(t, s, "ns", "proj", "b2.md"))
	assert.Equal(t, "to [b](b2.md)", readBody(t, s, "ns", "proj", "a2.md"))
}
