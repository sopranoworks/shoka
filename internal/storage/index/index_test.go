package index

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func tmpIndexPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "proj.index.db")
}

func TestCreateOpenClose(t *testing.T) {
	p := tmpIndexPath(t)
	idx, err := Create(p, "ns", "proj")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Meta is populated; marker starts empty.
	if v, _ := idx.Meta(MetaNamespace); v != "ns" {
		t.Errorf("namespace meta = %q, want ns", v)
	}
	if v, _ := idx.Meta(MetaProjectName); v != "proj" {
		t.Errorf("project meta = %q, want proj", v)
	}
	if c, _ := idx.LastIndexedCommit(); c != "" {
		t.Errorf("fresh LastIndexedCommit = %q, want empty", c)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen succeeds and preserves meta.
	idx2, err := Open(p)
	if err != nil {
		t.Fatalf("Open after create: %v", err)
	}
	defer idx2.Close()
	if v, _ := idx2.Meta(MetaProjectName); v != "proj" {
		t.Errorf("reopened project meta = %q, want proj", v)
	}
}

func TestCreateRejectsExisting(t *testing.T) {
	p := tmpIndexPath(t)
	idx, err := Create(p, "ns", "proj")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	idx.Close()
	if _, err := Create(p, "ns", "proj"); err == nil {
		t.Fatal("Create over an existing file must fail")
	}
}

func TestOpenMissingIsErrNotFound(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "absent.index.db"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open(missing) = %v, want ErrNotFound", err)
	}
}

func TestOpenGarbageIsErrCorrupt(t *testing.T) {
	p := tmpIndexPath(t)
	if err := os.WriteFile(p, []byte("this is not a bbolt database"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Open(p)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open(garbage) = %v, want ErrCorrupt", err)
	}
}

func TestOpenSchemaMismatch(t *testing.T) {
	p := tmpIndexPath(t)
	idx, err := Create(p, "ns", "proj")
	if err != nil {
		t.Fatal(err)
	}
	// Tamper the schema version, then reopen.
	if err := idx.SetMeta(MetaSchemaVersion, "999"); err != nil {
		t.Fatal(err)
	}
	idx.Close()
	_, err = Open(p)
	if !errors.Is(err, ErrSchemaMismatch) {
		t.Fatalf("Open(schema mismatch) = %v, want ErrSchemaMismatch", err)
	}
}

func TestPutGetDeleteRecord(t *testing.T) {
	idx, err := Create(tmpIndexPath(t), "ns", "proj")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if err := idx.PutRecord("dir/file.md", IndexRecord{Etag: "abc"}); err != nil {
		t.Fatalf("PutRecord: %v", err)
	}
	rec, ok, err := idx.GetRecord("dir/file.md")
	if err != nil || !ok {
		t.Fatalf("GetRecord ok=%v err=%v, want true,nil", ok, err)
	}
	if rec.Etag != "abc" {
		t.Errorf("record etag = %q, want abc", rec.Etag)
	}

	// Path normalisation: a leading slash addresses the same record.
	if rec2, ok2, _ := idx.GetRecord("/dir/file.md"); !ok2 || rec2.Etag != "abc" {
		t.Errorf("normalised lookup ok=%v etag=%q", ok2, rec2.Etag)
	}

	// Upsert replaces.
	if err := idx.PutRecord("dir/file.md", IndexRecord{Etag: "def"}); err != nil {
		t.Fatal(err)
	}
	if rec3, _, _ := idx.GetRecord("dir/file.md"); rec3.Etag != "def" {
		t.Errorf("after upsert etag = %q, want def", rec3.Etag)
	}

	// Delete is idempotent.
	if err := idx.DeleteRecord("dir/file.md"); err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	if _, ok, _ := idx.GetRecord("dir/file.md"); ok {
		t.Error("record present after delete")
	}
	if err := idx.DeleteRecord("dir/file.md"); err != nil {
		t.Errorf("second DeleteRecord (idempotent) errored: %v", err)
	}
}

func TestReplaceAllRebuildsWholesaleAndAdvancesMarker(t *testing.T) {
	idx, err := Create(tmpIndexPath(t), "ns", "proj")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	// Seed a stale record that must NOT survive a wholesale rebuild.
	_ = idx.PutRecord("stale.md", IndexRecord{Etag: "old"})

	records := map[string]IndexRecord{
		"a.md":       {Etag: "h1"},
		"sub/b.md":   {Etag: "h2"},
		"sub/c/d.md": {Etag: "h3"},
	}
	if err := idx.ReplaceAll(records, "commitHEAD"); err != nil {
		t.Fatalf("ReplaceAll: %v", err)
	}

	if _, ok, _ := idx.GetRecord("stale.md"); ok {
		t.Error("stale record survived ReplaceAll")
	}
	for p, want := range records {
		got, ok, _ := idx.GetRecord(p)
		if !ok || got.Etag != want.Etag {
			t.Errorf("after rebuild %q ok=%v etag=%q, want %q", p, ok, got.Etag, want.Etag)
		}
	}
	if n, _ := idx.Count(); n != len(records) {
		t.Errorf("Count after rebuild = %d, want %d", n, len(records))
	}
	if c, _ := idx.LastIndexedCommit(); c != "commitHEAD" {
		t.Errorf("marker after rebuild = %q, want commitHEAD", c)
	}
}

func TestMarkerRoundTrip(t *testing.T) {
	idx, err := Create(tmpIndexPath(t), "ns", "proj")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.SetLastIndexedCommit("deadbeef"); err != nil {
		t.Fatal(err)
	}
	if c, _ := idx.LastIndexedCommit(); c != "deadbeef" {
		t.Errorf("marker = %q, want deadbeef", c)
	}
}

// TestRecordForwardCompatibleDecode proves an old record (only "etag") decodes
// cleanly — the property that lets I2/I3 add fields without a migration.
func TestRecordForwardCompatibleDecode(t *testing.T) {
	idx, err := Create(tmpIndexPath(t), "ns", "proj")
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()
	if err := idx.PutRecord("f.md", IndexRecord{Etag: "x"}); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := idx.GetRecord("f.md")
	if err != nil || !ok || rec.Etag != "x" {
		t.Fatalf("decode: ok=%v err=%v etag=%q", ok, err, rec.Etag)
	}
}
