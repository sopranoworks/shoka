package storage

import (
	"context"
	"os"
	"testing"
	"time"
)

// item 2 (2026-06-18 lazy-create fix): a <p>.deleted.db is created ONLY on the first real
// op:"delete". write/move use the no-create accessor and skip when no log exists, so a
// project that never deletes anything never gets an empty deleted-log.

func lcWrite(t *testing.T, s *FSGitStorage, path, body string) {
	t.Helper()
	if err := s.WriteFile("ns", "p", path, body); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	s.WaitForWAL(30 * time.Second)
}

func lcDelete(t *testing.T, s *FSGitStorage, path string) {
	t.Helper()
	if err := s.DeleteFile("ns", "p", path); err != nil {
		t.Fatalf("delete %s: %v", path, err)
	}
	s.WaitForWAL(30 * time.Second)
}

// Core (RED-proven): a write-only history creates NO deleted-log file.
func TestDeletedLog_WriteOnlyCreatesNoFile(t *testing.T) {
	s := newEmptyStorage(t)
	if err := s.CreateProject("ns", "p"); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"a.md", "b.md", "a.md"} { // writes only — no delete, no move
		lcWrite(t, s, f, "body "+f)
	}
	// Assert BEFORE any ListDeleted (which would rebuild-on-read and create the file).
	if _, err := os.Stat(s.deletedLogPath("ns", "p")); !os.IsNotExist(err) {
		t.Fatalf("write-only history must NOT create a deleted-log (err=%v)", err)
	}
}

// A move with NO existing log creates none and does not error (write/move share the
// no-create branch).
func TestDeletedLog_MoveWithNoLogNoCreate(t *testing.T) {
	s := newEmptyStorage(t)
	if err := s.CreateProject("ns", "p"); err != nil {
		t.Fatal(err)
	}
	lcWrite(t, s, "a.md", "alpha")
	if _, _, err := s.Move(context.Background(), "", "ns", "p", "a.md", "b.md", nil); err != nil {
		t.Fatalf("move: %v", err)
	}
	s.WaitForWAL(30 * time.Second)
	if _, err := os.Stat(s.deletedLogPath("ns", "p")); !os.IsNotExist(err) {
		t.Fatalf("a move with no existing deleted-log must NOT create one (err=%v)", err)
	}
}

// The first op:"delete" creates the log and records the deletion; a subsequent write
// (revive/recreate) nets it out via the no-create accessor on the now-existing log.
func TestDeletedLog_FirstDeleteCreatesThenWriteNetsOut(t *testing.T) {
	s := newEmptyStorage(t)
	if err := s.CreateProject("ns", "p"); err != nil {
		t.Fatal(err)
	}
	lcWrite(t, s, "del.md", "doomed")
	if _, err := os.Stat(s.deletedLogPath("ns", "p")); !os.IsNotExist(err) {
		t.Fatalf("write before any delete must not create the log (err=%v)", err)
	}

	lcDelete(t, s, "del.md") // first real deletion → creates + records
	if _, err := os.Stat(s.deletedLogPath("ns", "p")); err != nil {
		t.Fatalf("first delete must create the deleted-log: %v", err)
	}
	df, err := s.ListDeleted("ns", "p")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range df {
		if d.Path == "del.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("first delete must be recorded; got %+v", df)
	}

	// Revive via write: the existing log is updated (path dropped from the deleted set).
	lcWrite(t, s, "del.md", "reborn")
	df2, err := s.ListDeleted("ns", "p")
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range df2 {
		if d.Path == "del.md" {
			t.Fatalf("a write after delete (revive) must net out the deletion; still present: %+v", df2)
		}
	}
}
