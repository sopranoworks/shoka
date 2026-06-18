package storage

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/sopranoworks/shoka/internal/storage/opmeta"
)

// newDeletedLogStorage builds storage (deleted-log default-on) with ns/proj and
// the given deleted-log options, and returns it plus its project path.
func newDeletedLogStorage(t *testing.T, opt DeletedLogOptions) *FSGitStorage {
	t.Helper()
	dir, err := os.MkdirTemp("", "shoka-dellog-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := NewFSGitStorageWithOptions(dir, Options{DeletedLog: opt})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatal(err)
	}
	return s
}

func drainWAL(t *testing.T, s *FSGitStorage) {
	t.Helper()
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL did not drain in time")
	}
}

func deletedSet(t *testing.T, s *FSGitStorage) map[string]string {
	t.Helper()
	recs, err := s.ListDeleted("ns", "proj")
	if err != nil {
		t.Fatalf("ListDeleted: %v", err)
	}
	out := make(map[string]string, len(recs))
	for _, r := range recs {
		out[r.Path] = r.DeletionCommit
	}
	return out
}

// headMessage returns the commit message at HEAD.
func headMessage(t *testing.T, s *FSGitStorage) string {
	t.Helper()
	r, err := git.PlainOpen(s.baseDir + "/ns/proj")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	return c.Message
}

// --- Mandatory test #1: write-at-commit + live net-state + Shoka-Op trailer ---

func TestDeletedLog_WriteAtCommit_LiveNetState(t *testing.T) {
	s := newDeletedLogStorage(t, DeletedLogOptions{})

	// Create then delete a.md.
	mustWrite(t, s, "a.md", "alpha")
	drainWAL(t, s)
	if m := headMessage(t, s); !hasOpTrailer(m, opmeta.OpWrite, "a.md", "") {
		t.Errorf("write commit missing/!valid Shoka-Op trailer: %q", m)
	}
	if err := s.DeleteFile("ns", "proj", "a.md"); err != nil {
		t.Fatal(err)
	}
	drainWAL(t, s)
	if m := headMessage(t, s); !hasOpTrailer(m, opmeta.OpDelete, "a.md", "") {
		t.Errorf("delete commit missing/!valid Shoka-Op trailer: %q", m)
	}
	set := deletedSet(t, s)
	if set["a.md"] == "" {
		t.Fatalf("delete should upsert a.md with a deletion commit; got %v", set)
	}

	// Re-create a.md (write) → dropped from the deleted set (delete-then-revive).
	mustWrite(t, s, "a.md", "alpha2")
	drainWAL(t, s)
	if _, ok := deletedSet(t, s)["a.md"]; ok {
		t.Errorf("re-created a.md must be dropped from the deleted set")
	}

	// Move b.md -> c.md: the SOURCE is relocated, NOT deleted.
	mustWrite(t, s, "b.md", "beta")
	drainWAL(t, s)
	if _, _, err := s.Move(context.Background(), "", "ns", "proj", "b.md", "c.md", nil); err != nil {
		t.Fatal(err)
	}
	drainWAL(t, s)
	if m := headMessage(t, s); !hasOpTrailer(m, opmeta.OpMove, "c.md", "b.md") {
		t.Errorf("move commit missing/!valid Shoka-Op trailer: %q", m)
	}
	set = deletedSet(t, s)
	if _, ok := set["b.md"]; ok {
		t.Errorf("move source b.md must NOT be listed as deleted; got %v", set)
	}
	if _, ok := set["c.md"]; ok {
		t.Errorf("move destination c.md must NOT be listed as deleted; got %v", set)
	}
}

func hasOpTrailer(message, op, path, from string) bool {
	m, ok := opmeta.Parse(message)
	return ok && m.Op == op && m.Path == path && m.From == from
}

// --- Mandatory test #2: cheap list + FIFO cap ---

func TestDeletedLog_List_FIFOCap(t *testing.T) {
	s := newDeletedLogStorage(t, DeletedLogOptions{MaxEntries: 2})
	for _, p := range []string{"a.md", "b.md", "c.md"} {
		mustWrite(t, s, p, "x")
		drainWAL(t, s)
		if err := s.DeleteFile("ns", "proj", p); err != nil {
			t.Fatal(err)
		}
		drainWAL(t, s)
	}
	set := deletedSet(t, s)
	if len(set) != 2 {
		t.Fatalf("cap=2 should hold 2 deleted entries, got %d: %v", len(set), set)
	}
	if _, ok := set["a.md"]; ok {
		t.Errorf("oldest deletion a.md should have been evicted by the FIFO cap; got %v", set)
	}
	// The list is served from the bucket even with no further git interaction —
	// calling it twice yields the same set (no walk, no re-validation).
	if again := deletedSet(t, s); len(again) != 2 {
		t.Errorf("second list should be the same cheap bucket read; got %v", again)
	}
}

// --- Mandatory test #3: bounded repair, trigger (a), net-state agreement,
// raw-diff fallback for a metadata-less (external) commit, and bounded depth. ---

func TestDeletedLog_Repair_NetStateAgrees(t *testing.T) {
	s := newDeletedLogStorage(t, DeletedLogOptions{})

	// A sequence that exercises every net-state edge.
	mustWrite(t, s, "del.md", "x")
	drainWAL(t, s)
	if err := s.DeleteFile("ns", "proj", "del.md"); err != nil { // stays deleted
		t.Fatal(err)
	}
	drainWAL(t, s)
	mustWrite(t, s, "mv.md", "y")
	drainWAL(t, s)
	if _, _, err := s.Move(context.Background(), "", "ns", "proj", "mv.md", "mv2.md", nil); err != nil { // source not deleted
		t.Fatal(err)
	}
	drainWAL(t, s)
	mustWrite(t, s, "revive.md", "z")
	drainWAL(t, s)
	if err := s.DeleteFile("ns", "proj", "revive.md"); err != nil {
		t.Fatal(err)
	}
	drainWAL(t, s)
	mustWrite(t, s, "revive.md", "z2") // delete-then-revive → nets out
	drainWAL(t, s)

	live := deletedSet(t, s)
	if _, ok := live["del.md"]; !ok {
		t.Fatalf("live: del.md should be deleted; got %v", live)
	}
	for _, p := range []string{"mv.md", "mv2.md", "revive.md"} {
		if _, ok := live[p]; ok {
			t.Fatalf("live: %s should NOT be deleted; got %v", p, live)
		}
	}

	// Trigger (a): remove the store file → next ListDeleted rebuilds from the
	// bounded walk and must reproduce the SAME net set.
	s.removeDeletedLogFile("ns", "proj")
	if s.deletedLogExists("ns", "proj") {
		t.Fatal("store file should be gone")
	}
	rebuilt := deletedSet(t, s)
	if len(rebuilt) != len(live) {
		t.Fatalf("rebuilt net set differs from live: rebuilt=%v live=%v", rebuilt, live)
	}
	for p := range live {
		if _, ok := rebuilt[p]; !ok {
			t.Fatalf("rebuilt missing %s: rebuilt=%v live=%v", p, rebuilt, live)
		}
	}
}

// TestDeletedLog_Repair_RawDiffFallback: a metadata-less external commit (no
// Shoka-Op trailer) is classified by the raw diff alone — a removal is a deletion.
func TestDeletedLog_Repair_RawDiffFallback(t *testing.T) {
	s := newDeletedLogStorage(t, DeletedLogOptions{})
	mustWrite(t, s, "ext.md", "content")
	drainWAL(t, s)
	// Remove ext.md via a raw git commit with a PLAIN message (no Shoka-Op).
	rawCommit(t, s, func(r *git.Repository, baseTree plumbing.Hash) plumbing.Hash {
		h, err := applyToTree(r, baseTree, []string{"ext.md"}, plumbing.ZeroHash, true)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}, "external removal\n")

	s.removeDeletedLogFile("ns", "proj")
	set := deletedSet(t, s)
	if _, ok := set["ext.md"]; !ok {
		t.Fatalf("raw-diff fallback: an unexplained removal must be classified deleted; got %v", set)
	}
}

// TestDeletedLog_Repair_Bounded: the rebuild walk never exceeds repair_depth, so a
// deletion older than the window is not recovered (accepted divergence).
func TestDeletedLog_Repair_Bounded(t *testing.T) {
	s := newDeletedLogStorage(t, DeletedLogOptions{RepairDepth: 2})
	// Oldest: delete old.md.
	mustWrite(t, s, "old.md", "x")
	drainWAL(t, s)
	if err := s.DeleteFile("ns", "proj", "old.md"); err != nil {
		t.Fatal(err)
	}
	drainWAL(t, s)
	// Push old.md's deletion out of the 2-commit window with newer commits.
	for _, p := range []string{"n1.md", "n2.md", "n3.md"} {
		mustWrite(t, s, p, "x")
		drainWAL(t, s)
	}
	s.removeDeletedLogFile("ns", "proj")
	set := deletedSet(t, s)
	if _, ok := set["old.md"]; ok {
		t.Fatalf("deletion older than repair_depth must NOT be recovered; got %v", set)
	}
}

// --- Mandatory test #4: authenticity (the meaning gate) ---

func TestDeletedLog_Authenticity_ContradictedClaimRejected(t *testing.T) {
	s := newDeletedLogStorage(t, DeletedLogOptions{})
	mustWrite(t, s, "a.md", "x")
	drainWAL(t, s)

	// A commit that only REMOVES a.md but LIES that it moved a.md -> b.md. The diff
	// has no Insert(b.md), so the claim is rejected → raw-diff → a.md is deleted.
	rawCommit(t, s, func(r *git.Repository, baseTree plumbing.Hash) plumbing.Hash {
		h, err := applyToTree(r, baseTree, []string{"a.md"}, plumbing.ZeroHash, true)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}, "Move a.md -> b.md\n\n"+opmeta.Trailer(opmeta.Meta{Op: opmeta.OpMove, Path: "b.md", From: "a.md"}))

	s.removeDeletedLogFile("ns", "proj")
	if _, ok := deletedSet(t, s)["a.md"]; !ok {
		t.Fatalf("a contradicted move claim must fall back to raw-diff (a.md deleted)")
	}
}

func TestDeletedLog_Authenticity_MatchingMoveHonoured(t *testing.T) {
	s := newDeletedLogStorage(t, DeletedLogOptions{})
	mustWrite(t, s, "a.md", "payload")
	drainWAL(t, s)

	// A genuine move commit: remove a.md AND add b.md, with a matching claim. The
	// source must NOT be classified deleted.
	rawCommit(t, s, func(r *git.Repository, baseTree plumbing.Hash) plumbing.Hash {
		t1, err := applyToTree(r, baseTree, []string{"a.md"}, plumbing.ZeroHash, true)
		if err != nil {
			t.Fatal(err)
		}
		blob, err := storeBlob(r, []byte("payload"))
		if err != nil {
			t.Fatal(err)
		}
		t2, err := applyToTree(r, t1, []string{"b.md"}, blob, false)
		if err != nil {
			t.Fatal(err)
		}
		return t2
	}, "Move a.md -> b.md\n\n"+opmeta.Trailer(opmeta.Meta{Op: opmeta.OpMove, Path: "b.md", From: "a.md"}))

	s.removeDeletedLogFile("ns", "proj")
	set := deletedSet(t, s)
	if _, ok := set["a.md"]; ok {
		t.Fatalf("a verified move source must NOT be deleted; got %v", set)
	}
}

// --- Mandatory test #5: revival O(1) + divergence + name-specified last resort ---

func TestDeletedLog_Revive_ForwardOnly(t *testing.T) {
	s := newDeletedLogStorage(t, DeletedLogOptions{})
	mustWrite(t, s, "doc.md", "original")
	drainWAL(t, s)
	if err := s.DeleteFile("ns", "proj", "doc.md"); err != nil {
		t.Fatal(err)
	}
	drainWAL(t, s)
	delCommit := deletedSet(t, s)["doc.md"]
	if delCommit == "" {
		t.Fatal("doc.md not in deleted set")
	}

	if err := s.ReviveFile(context.Background(), "ns", "proj", "doc.md", ""); err != nil {
		t.Fatalf("revive: %v", err)
	}
	drainWAL(t, s)

	// File is back with its original content.
	content, err := s.ReadFile("ns", "proj", "doc.md")
	if err != nil {
		t.Fatalf("read revived: %v", err)
	}
	if content != "original" {
		t.Errorf("revived content = %q, want %q", content, "original")
	}
	// Dropped from the deleted set after the revive commit lands.
	if _, ok := deletedSet(t, s)["doc.md"]; ok {
		t.Errorf("revived doc.md must be dropped from the deleted set")
	}
	// History preserved: the deletion commit is still reachable (forward-only).
	r, _ := git.PlainOpen(s.baseDir + "/ns/proj")
	if _, err := r.CommitObject(plumbing.NewHash(delCommit)); err != nil {
		t.Errorf("deletion commit should still exist (history preserved): %v", err)
	}
}

func TestDeletedLog_Revive_DivergenceError(t *testing.T) {
	s := newDeletedLogStorage(t, DeletedLogOptions{})
	mustWrite(t, s, "doc.md", "x")
	drainWAL(t, s)

	// A bogus from_commit (a 40-hex hash git does not have) → typed divergence error.
	bogus := "0123456789abcdef0123456789abcdef01234567"
	err := s.ReviveFile(context.Background(), "ns", "proj", "doc.md", bogus)
	if !errors.Is(err, ErrDeletionDiverged) {
		t.Fatalf("want ErrDeletionDiverged, got %v", err)
	}
}

func TestDeletedLog_Revive_NameSpecifiedLastResort(t *testing.T) {
	s := newDeletedLogStorage(t, DeletedLogOptions{})
	mustWrite(t, s, "gone.md", "lastcontent")
	drainWAL(t, s)
	if err := s.DeleteFile("ns", "proj", "gone.md"); err != nil {
		t.Fatal(err)
	}
	drainWAL(t, s)

	// Simulate a capped-out / diverged entry: drop it from the log so the revival
	// must fall back to the name-specified path-filtered history scan.
	if st := s.deletedLogForRead("ns", "proj"); st != nil {
		if err := st.Drop("gone.md"); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := deletedSet(t, s)["gone.md"]; ok {
		t.Fatal("precondition: gone.md should not be in the log")
	}

	if err := s.ReviveFile(context.Background(), "ns", "proj", "gone.md", ""); err != nil {
		t.Fatalf("name-specified revive: %v", err)
	}
	drainWAL(t, s)
	content, err := s.ReadFile("ns", "proj", "gone.md")
	if err != nil || content != "lastcontent" {
		t.Fatalf("name-specified revive content=%q err=%v", content, err)
	}

	// A path git never had → clear divergence error.
	if err := s.ReviveFile(context.Background(), "ns", "proj", "never-existed.md", ""); !errors.Is(err, ErrDeletionDiverged) {
		t.Fatalf("reviving a never-existed path should diverge, got %v", err)
	}
}

// rawCommit builds a commit directly (bypassing the WAL/commitEntry funnel) from a
// tree mutation and message, advancing HEAD. Used to craft external (no-metadata)
// and lying-metadata commits the live hook never produces, to exercise the repair
// walk's classification.
func rawCommit(t *testing.T, s *FSGitStorage, mutate func(*git.Repository, plumbing.Hash) plumbing.Hash, message string) plumbing.Hash {
	t.Helper()
	projectPath := s.baseDir + "/ns/proj"
	r, err := git.PlainOpen(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	base, err := r.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	newTree := mutate(r, base.TreeHash)
	sig := object.Signature{Name: "ext", Email: "ext@example.invalid", When: time.Now()}
	commit := &object.Commit{Author: sig, Committer: sig, Message: message, TreeHash: newTree, ParentHashes: []plumbing.Hash{ref.Hash()}}
	obj := r.Storer.NewEncodedObject()
	if err := commit.Encode(obj); err != nil {
		t.Fatal(err)
	}
	h, err := r.Storer.SetEncodedObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	if err := advanceHead(projectPath, r, h); err != nil {
		t.Fatal(err)
	}
	return h
}
