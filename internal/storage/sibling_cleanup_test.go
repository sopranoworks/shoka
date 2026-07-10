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

// #1 (core) — the classifier recognises all compound-suffix siblings: a LIVE project with
// all four sibling DBs produces NO orphaned entry; a genuine stray catalog with no project
// dir still IS one.
func TestBugfix_ClassifierRecognizesDeletedDB(t *testing.T) {
	s := newEmptyStorage(t)
	if err := s.CreateProject("ns", "p"); err != nil { // real .git + p.db
		t.Fatal(err)
	}
	bfTouch(t, s.indexPath("ns", "p"))      // p.index.db
	bfTouch(t, s.deletedLogPath("ns", "p")) // p.deleted.db
	bfTouch(t, s.vectorPath("ns", "p"))     // p.vector.db

	nh := s.CheckNamespaceHealth("ns")
	for _, bad := range []string{"p", "p.deleted", "p.index", "p.vector"} {
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
			if len(o.Files) != 1 || o.Files[0] != "@stray.project.db" {
				t.Fatalf("orphan Files = %v, want [@stray.project.db]", o.Files)
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
	// a sibling from the shared set cannot mask this strand check).
	for _, p := range []string{
		s.catalogPath("ns", "p"),
		s.indexPath("ns", "p"),
		s.deletedLogPath("ns", "p"),
		s.vectorPath("ns", "p"),
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
	bfTouch(t, s.vectorPath("ns", "p"))     // p.vector.db
	// Make it repo-less so discovery classifies it as a leftover.
	if err := os.RemoveAll(filepath.Join(s.baseDir, "ns", "p", ".git")); err != nil {
		t.Fatal(err)
	}
	_, leftovers := s.discoverProjects()
	if len(leftovers) != 1 || len(leftovers[0].dbPaths) != 4 {
		t.Fatalf("leftover should bundle all 4 present siblings, got %+v", leftovers)
	}
	s.relocateLeftovers(leftovers, time.Now())
	for _, p := range s.siblingDBPaths("ns", "p") {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("leftover relocation stranded sibling %s (err=%v)", filepath.Base(p), err)
		}
	}
}

// #3 (core, data-loss) — CleanOrphanedSibling REFUSES a live project's .deleted.db/.index.db/
// .vector.db sibling (the live DB survives), but still cleans a genuine stray's every sibling.
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
	// Likewise the .vector sibling of a live project.
	bfTouch(t, s.vectorPath("ns", "maintenance"))
	if err := s.CleanOrphanedSibling("ns", "maintenance.vector"); err == nil {
		t.Fatal("CleanOrphanedSibling must refuse a live project's vector sibling")
	}

	// A genuine stray (no project dir) still cleans — and ALL its siblings go.
	bfTouch(t, s.catalogPath("ns", "stray"))
	bfTouch(t, s.indexPath("ns", "stray"))
	bfTouch(t, s.deletedLogPath("ns", "stray"))
	bfTouch(t, s.vectorPath("ns", "stray"))
	if err := s.CleanOrphanedSibling("ns", "stray"); err != nil {
		t.Fatalf("cleaning a genuine stray failed: %v", err)
	}
	for _, p := range s.siblingDBPaths("ns", "stray") {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("genuine stray sibling %s not cleaned (err=%v)", filepath.Base(p), err)
		}
	}
}

// --- dbBaseName known-kind vocabulary tests ---

func TestDbBaseName_KnownKindVocabulary(t *testing.T) {
	cases := []struct {
		file     string
		wantBase string
		wantOK   bool
	}{
		// @ prefix required.
		{"@p.project.db", "p", true},
		{"@p.index.db", "p", true},
		{"@p.deleted.db", "p", true},
		{"@p.vector.db", "p", true},

		// Without @ prefix — NOT a sibling DB.
		{"p.project.db", "", false},
		{"p.index.db", "", false},
		{"p.db", "", false},
		{"p.unknown.db", "", false},
		{"notadb.txt", "", false},
		{"", "", false},

		// Dotted project names — the whole point of the known-kind vocabulary.
		{"@vue.js.project.db", "vue.js", true},
		{"@vue.js.index.db", "vue.js", true},
		{"@node.js.deleted.db", "node.js", true},
		{"@socket.io.vector.db", "socket.io", true},
		{"@babel.config.project.db", "babel.config", true},

		// Pathological: project name that IS a known kind.
		{"@index.project.db", "index", true},
		{"@index.index.db", "index", true},
		{"@deleted.deleted.db", "deleted", true},
		{"@vector.vector.db", "vector", true},
		{"@project.project.db", "project", true},

		// Pathological: project "A.index" vs project "A".
		{"@A.index.project.db", "A.index", true},
		{"@A.index.index.db", "A.index", true},
		{"@A.project.db", "A", true},
		{"@A.index.db", "A", true},
		{"@A.deleted.db", "A", true},
		{"@A.vector.db", "A", true},

		// Pathological: project literally named "A.index.db".
		{"@A.index.db.project.db", "A.index.db", true},
		{"@A.index.db.index.db", "A.index.db", true},

		// Multi-dot pathological.
		{"@a.b.c.index.db", "a.b.c", true},
		{"@a.index.b.project.db", "a.index.b", true},
	}
	for _, c := range cases {
		base, ok := dbBaseName(c.file)
		if ok != c.wantOK || base != c.wantBase {
			t.Errorf("dbBaseName(%q) = (%q, %v), want (%q, %v)",
				c.file, base, ok, c.wantBase, c.wantOK)
		}
	}
}

// TestDisambiguation_DottedProjectNames proves that projects with dotted names
// (vue.js, A.index, A.index.db, etc.) produce distinct, non-colliding sibling
// filenames, and that no sibling filename can collide with any project directory.
func TestDisambiguation_DottedProjectNames(t *testing.T) {
	s := newEmptyStorage(t)

	names := []string{"A", "A-index", "vue-js", "node-js", "socket-io"}
	for _, name := range names {
		if err := s.CreateProject("ns", name); err != nil {
			t.Fatalf("CreateProject(%q): %v", name, err)
		}
	}

	// All sibling filenames are distinct across all projects.
	seen := make(map[string]string)
	for _, name := range names {
		for _, p := range s.siblingDBPaths("ns", name) {
			base := filepath.Base(p)
			if prev, dup := seen[base]; dup {
				t.Fatalf("COLLISION: %q produced by both project %q and %q", base, prev, name)
			}
			seen[base] = name
		}
	}

	// No sibling filename can collide with a project directory name (@ prefix guarantee).
	for _, name := range names {
		for _, p := range s.siblingDBPaths("ns", name) {
			base := filepath.Base(p)
			for _, dir := range names {
				if base == dir {
					t.Fatalf("sibling filename %q collides with project directory %q", base, dir)
				}
			}
		}
	}

	// Each project's siblings round-trip back to the correct project via dbBaseName.
	for _, name := range names {
		for _, p := range s.siblingDBPaths("ns", name) {
			base, ok := dbBaseName(filepath.Base(p))
			if !ok {
				t.Fatalf("dbBaseName(%q) returned false for project %q", filepath.Base(p), name)
			}
			if base != name {
				t.Fatalf("dbBaseName(%q) = %q, want %q (project %q)", filepath.Base(p), base, name, name)
			}
		}
	}
}

// TestHealthCheck_DottedProjectNoFalseOrphan: a live project with a dotted name
// and all four sibling DBs does NOT produce any orphaned entry.
func TestHealthCheck_DottedProjectNoFalseOrphan(t *testing.T) {
	s := newEmptyStorage(t)
	for _, name := range []string{"A", "A-index"} {
		if err := s.CreateProject("ns", name); err != nil {
			t.Fatal(err)
		}
	}
	// Touch all sibling DBs for both projects.
	for _, name := range []string{"A", "A-index"} {
		bfTouch(t, s.indexPath("ns", name))
		bfTouch(t, s.deletedLogPath("ns", name))
		bfTouch(t, s.vectorPath("ns", name))
	}

	nh := s.CheckNamespaceHealth("ns")
	if len(nh.Orphaned) > 0 {
		t.Fatalf("no orphans expected with dotted project names, got %+v", nh.Orphaned)
	}
	if !nh.Healthy {
		t.Fatal("namespace must be healthy when all projects and siblings are present")
	}
}
