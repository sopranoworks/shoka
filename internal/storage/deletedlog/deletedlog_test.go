package deletedlog

import (
	"path/filepath"
	"testing"
	"time"
)

func mustCreate(t *testing.T) *Store {
	t.Helper()
	p := filepath.Join(t.TempDir(), "proj.deleted.db")
	st, err := Create(p, "ns", "proj")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestOpenMissingIsNotFound(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "absent.deleted.db")); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestUpsertDropList(t *testing.T) {
	st := mustCreate(t)
	now := time.Now().UTC()
	if err := st.Upsert(DeletedRecord{Path: "a.md", DeletionCommit: "h1", DeletedAt: now}, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.Upsert(DeletedRecord{Path: "dir/b.md", DeletionCommit: "h2", DeletedAt: now}, 0); err != nil {
		t.Fatal(err)
	}
	list, err := st.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2, got %d", len(list))
	}
	// sorted by path: a.md before dir/b.md
	if list[0].Path != "a.md" || list[0].DeletionCommit != "h1" {
		t.Errorf("unexpected first entry: %+v", list[0])
	}
	if err := st.Drop("a.md"); err != nil {
		t.Fatal(err)
	}
	list, _ = st.List()
	if len(list) != 1 || list[0].Path != "dir/b.md" {
		t.Fatalf("after drop want [dir/b.md], got %+v", list)
	}
	// Drop is idempotent.
	if err := st.Drop("a.md"); err != nil {
		t.Fatalf("idempotent drop: %v", err)
	}
}

// TestFIFOCap: Upsert past the cap evicts the OLDEST by DeletedAt.
func TestFIFOCap(t *testing.T) {
	st := mustCreate(t)
	base := time.Now().UTC()
	// Insert oldest..newest.
	for i, p := range []string{"old.md", "mid.md", "new.md"} {
		at := base.Add(time.Duration(i) * time.Minute)
		if err := st.Upsert(DeletedRecord{Path: p, DeletionCommit: "h", DeletedAt: at}, 2); err != nil {
			t.Fatal(err)
		}
	}
	list, _ := st.List()
	if len(list) != 2 {
		t.Fatalf("cap=2 should hold 2, got %d", len(list))
	}
	for _, r := range list {
		if r.Path == "old.md" {
			t.Errorf("oldest entry should have been evicted, found %q", r.Path)
		}
	}
}

// TestReplaceAllCaps: ReplaceAll keeps the newest maxEntries by DeletedAt.
func TestReplaceAllCaps(t *testing.T) {
	st := mustCreate(t)
	base := time.Now().UTC()
	recs := []DeletedRecord{
		{Path: "a", DeletionCommit: "1", DeletedAt: base.Add(1 * time.Minute)},
		{Path: "b", DeletionCommit: "2", DeletedAt: base.Add(2 * time.Minute)},
		{Path: "c", DeletionCommit: "3", DeletedAt: base.Add(3 * time.Minute)},
	}
	if err := st.ReplaceAll(recs, 2); err != nil {
		t.Fatal(err)
	}
	list, _ := st.List()
	if len(list) != 2 {
		t.Fatalf("want 2, got %d", len(list))
	}
	got := map[string]bool{}
	for _, r := range list {
		got[r.Path] = true
	}
	if got["a"] {
		t.Errorf("oldest 'a' should have been dropped by the cap; got %+v", list)
	}
	if !got["b"] || !got["c"] {
		t.Errorf("newest b,c should be kept; got %+v", list)
	}
}

// TestReplaceAllReplaces: ReplaceAll drops stale entries wholesale.
func TestReplaceAllReplaces(t *testing.T) {
	st := mustCreate(t)
	_ = st.Upsert(DeletedRecord{Path: "stale.md", DeletionCommit: "x", DeletedAt: time.Now()}, 0)
	if err := st.ReplaceAll([]DeletedRecord{{Path: "fresh.md", DeletionCommit: "y", DeletedAt: time.Now()}}, 0); err != nil {
		t.Fatal(err)
	}
	list, _ := st.List()
	if len(list) != 1 || list[0].Path != "fresh.md" {
		t.Fatalf("ReplaceAll should drop stale entries; got %+v", list)
	}
}
