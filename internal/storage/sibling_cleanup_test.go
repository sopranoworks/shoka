package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Tests for the 2026-06-18 orphaned-misdetection bugfix: the .deleted.db-aware classifier,
// the shared per-project sibling-path set routed through every lifecycle op, and the Clean
// live-sibling data-loss guard.

func bfTouch(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatal(err)
	}
}

func bfWriteCommit(t *testing.T, s *FSGitStorage, ns, proj, path, body string) {
	t.Helper()
	if err := s.WriteFile(ns, proj, path, body); err != nil {
		t.Fatalf("write %s/%s/%s: %v", ns, proj, path, err)
	}
	s.WaitForWAL(30 * time.Second)
}

// bfDeleteCommit deletes a file and drains the WAL. A real op:"delete" is what creates the
// <proj>.deleted.db (the lazy-create fix: write/move no longer create it), so these
// sibling-cleanup tests seed the deleted-log via a delete, not a write.
func bfDeleteCommit(t *testing.T, s *FSGitStorage, ns, proj, path string) {
	t.Helper()
	if err := s.DeleteFile(ns, proj, path); err != nil {
		t.Fatalf("delete %s/%s/%s: %v", ns, proj, path, err)
	}
	s.WaitForWAL(30 * time.Second)
}

// #1 (core) — the classifier recognises .deleted.db: a LIVE project with all three sibling
// DBs produces NO orphaned entry; a genuine stray catalog with no project dir still IS one.
func TestBugfix_ClassifierRecognizesDeletedDB(t *testing.T) {
	s := newEmptyStorage(t)
	if err := s.CreateProject("ns", "p"); err != nil { // real .git + p.db
		t.Fatal(err)
	}
	bfTouch(t, s.indexPath("ns", "p"))      // p.index.db
	bfTouch(t, s.deletedLogPath("ns", "p")) // p.deleted.db (the previously-misclassified sibling)

	nh := s.CheckNamespaceHealth("ns")
	for _, bad := range []string{"p", "p.deleted", "p.index"} {
		if hasOrphan(nh, bad) {
			t.Fatalf("live project sibling falsely orphaned as %q: %+v", bad, nh.Orphaned)
		}
	}

	// A genuine stray catalog with no project dir is still flagged, with its real filename.
	bfTouch(t, s.catalogPath("ns", "stray"))
	nh = s.CheckNamespaceHealth("ns")
	if !hasOrphan(nh, "stray") {
		t.Fatalf("genuine stray catalog must be orphaned: %+v", nh.Orphaned)
	}
	for _, o := range nh.Orphaned {
		if o.Name == "stray" {
			if len(o.Files) != 1 || o.Files[0] != "stray.db" {
				t.Fatalf("orphan Files = %v, want [stray.db]", o.Files)
			}
		}
	}
}

// #2 — DeleteProject removes the deleted-log sibling too (no stranded <p>.deleted.db).
func TestBugfix_DeleteProjectRemovesDeletedLog(t *testing.T) {
	s := newEmptyStorage(t)
	if err := s.CreateProject("ns", "p"); err != nil {
		t.Fatal(err)
	}
	bfWriteCommit(t, s, "ns", "p", "a.md", "x")
	bfDeleteCommit(t, s, "ns", "p", "a.md") // a real delete creates p.deleted.db
	if _, err := os.Stat(s.deletedLogPath("ns", "p")); err != nil {
		t.Fatalf("precondition: deleted-log should exist after a delete: %v", err)
	}
	if err := s.DeleteProject(context.Background(), "ns", "p"); err != nil {
		t.Fatal(err)
	}
	// Assert each sibling path EXPLICITLY (not via siblingDBPaths, so a regression that drops
	// the deleted-log from the shared set cannot mask this strand check).
	for _, p := range []string{
		s.catalogPath("ns", "p"),
		s.indexPath("ns", "p"),
		s.deletedLogPath("ns", "p"),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("DeleteProject stranded sibling %s (err=%v)", filepath.Base(p), err)
		}
	}
}

// #2 — MoveProject relocates the deleted-log sibling to the destination namespace.
func TestBugfix_MoveRelocatesDeletedLog(t *testing.T) {
	s := newEmptyStorage(t)
	if err := s.CreateNamespace("dst"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject("src", "p"); err != nil {
		t.Fatal(err)
	}
	bfWriteCommit(t, s, "src", "p", "a.md", "x")
	bfDeleteCommit(t, s, "src", "p", "a.md") // creates src p.deleted.db
	if err := s.MoveProject(context.Background(), "src", "p", "dst"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.deletedLogPath("src", "p")); !os.IsNotExist(err) {
		t.Fatalf("move left the deleted-log in source: %v", err)
	}
	if _, err := os.Stat(s.deletedLogPath("dst", "p")); err != nil {
		t.Fatalf("move did not relocate the deleted-log to dst: %v", err)
	}
}

// #2 — RenameProject relocates the deleted-log sibling to the new name.
func TestBugfix_RenameRelocatesDeletedLog(t *testing.T) {
	s := newEmptyStorage(t)
	if err := s.CreateProject("ns", "old"); err != nil {
		t.Fatal(err)
	}
	bfWriteCommit(t, s, "ns", "old", "a.md", "x")
	bfDeleteCommit(t, s, "ns", "old", "a.md") // creates ns/old.deleted.db
	if err := s.RenameProject(context.Background(), "ns", "old", "new"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.deletedLogPath("ns", "old")); !os.IsNotExist(err) {
		t.Fatalf("rename left the old deleted-log: %v", err)
	}
	if _, err := os.Stat(s.deletedLogPath("ns", "new")); err != nil {
		t.Fatalf("rename did not relocate the deleted-log: %v", err)
	}
}

// #2 — leftover relocation bundles the index AND deleted-log siblings (not just the catalog),
// so a repo-less leftover never strands one.
func TestBugfix_LeftoverBundlesAllSiblings(t *testing.T) {
	s := newEmptyStorage(t)
	if err := s.CreateProject("ns", "p"); err != nil {
		t.Fatal(err)
	}
	bfWriteCommit(t, s, "ns", "p", "a.md", "x")
	bfDeleteCommit(t, s, "ns", "p", "a.md") // p.deleted.db (real, via delete)
	bfTouch(t, s.indexPath("ns", "p"))      // p.index.db
	// Make it repo-less so discovery classifies it as a leftover.
	if err := os.RemoveAll(filepath.Join(s.baseDir, "ns", "p", ".git")); err != nil {
		t.Fatal(err)
	}
	_, leftovers := s.discoverProjects()
	if len(leftovers) != 1 || len(leftovers[0].dbPaths) != 3 {
		t.Fatalf("leftover should bundle all 3 present siblings, got %+v", leftovers)
	}
	s.relocateLeftovers(leftovers, time.Now())
	for _, p := range s.siblingDBPaths("ns", "p") {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("leftover relocation stranded sibling %s (err=%v)", filepath.Base(p), err)
		}
	}
}

// #3 (core, data-loss) — CleanOrphanedSibling REFUSES a live project's .deleted.db/.index.db
// sibling (the live log survives), but still cleans a genuine stray's every sibling.
func TestBugfix_CleanRefusesLiveSiblingButCleansStray(t *testing.T) {
	s := newEmptyStorage(t)
	if err := s.CreateProject("ns", "maintenance"); err != nil {
		t.Fatal(err)
	}
	bfWriteCommit(t, s, "ns", "maintenance", "a.md", "x")
	bfDeleteCommit(t, s, "ns", "maintenance", "a.md") // live maintenance.deleted.db (via delete)
	dl := s.deletedLogPath("ns", "maintenance")
	if _, err := os.Stat(dl); err != nil {
		t.Fatalf("precondition: live deleted-log should exist: %v", err)
	}
	// The pre-fix data-loss path: cleaning "maintenance.deleted" must be REFUSED.
	if err := s.CleanOrphanedSibling("ns", "maintenance.deleted"); err == nil {
		t.Fatal("CleanOrphanedSibling must refuse a live project's deleted-log sibling")
	}
	if _, err := os.Stat(dl); err != nil {
		t.Fatalf("DATA LOSS: live deleted-log deleted despite refusal: %v", err)
	}
	// Likewise the .index sibling of a live project.
	if err := s.CleanOrphanedSibling("ns", "maintenance.index"); err == nil {
		t.Fatal("CleanOrphanedSibling must refuse a live project's index sibling")
	}

	// A genuine stray (no project dir) still cleans — and ALL its siblings go.
	bfTouch(t, s.catalogPath("ns", "stray"))
	bfTouch(t, s.indexPath("ns", "stray"))
	bfTouch(t, s.deletedLogPath("ns", "stray"))
	if err := s.CleanOrphanedSibling("ns", "stray"); err != nil {
		t.Fatalf("cleaning a genuine stray failed: %v", err)
	}
	for _, p := range s.siblingDBPaths("ns", "stray") {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("genuine stray sibling %s not cleaned (err=%v)", filepath.Base(p), err)
		}
	}
}
