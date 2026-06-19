package storage

import (
	"testing"
)

// Part A (2026-06-18 origin marker): a deleted-log created by a real op:"delete" carries the
// origin marker, and the marker is RETAINED across a revive that empties the deleted set.
func TestDeletedLog_OriginMarker_SetOnDeleteRetainedOnRevive(t *testing.T) {
	s := newEmptyStorage(t)
	if err := s.CreateProject("ns", "p"); err != nil {
		t.Fatal(err)
	}

	// Before any deletion there is no log and therefore no marker.
	if marked, _ := s.deletedLogHasOriginMarker("ns", "p"); marked {
		t.Fatal("no deleted-log should exist yet (no marker)")
	}

	// First real deletion: write del.md, then delete it → the log is CREATED with the marker.
	lcWrite(t, s, "del.md", "doomed")
	lcDelete(t, s, "del.md")

	marked, err := s.deletedLogHasOriginMarker("ns", "p")
	if err != nil {
		t.Fatal(err)
	}
	if !marked {
		t.Fatal("the first op:\"delete\" must create a MARKED deleted-log (origin marker present)")
	}
	if empty, _ := s.deletedLogRecordsEmpty("ns", "p"); empty {
		t.Fatal("the deleted set should hold the one deletion record")
	}
	// The marker lives in a different bucket, so ListDeleted is unaffected.
	df, err := s.ListDeleted("ns", "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(df) != 1 || df[0].Path != "del.md" {
		t.Fatalf("ListDeleted = %+v, want exactly [del.md] (marker must not pollute the count)", df)
	}

	// Revive: re-create del.md (a write) → the deleted set empties, but the marker REMAINS.
	lcWrite(t, s, "del.md", "reborn")

	marked2, err := s.deletedLogHasOriginMarker("ns", "p")
	if err != nil {
		t.Fatal(err)
	}
	if !marked2 {
		t.Fatal("RETENTION: the origin marker MUST survive a revive that empties the deleted set")
	}
	if empty, _ := s.deletedLogRecordsEmpty("ns", "p"); !empty {
		t.Fatal("the revive should have emptied the deleted set")
	}
}
