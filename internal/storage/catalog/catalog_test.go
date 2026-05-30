package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newCatalog(t *testing.T) *Catalog {
	t.Helper()
	p := filepath.Join(t.TempDir(), "proj.db")
	c, err := Create(p, "ns", "proj")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func entry(content string) FileEntry {
	sum := sha256.Sum256([]byte(content))
	return FileEntry{
		Etag:       hex.EncodeToString(sum[:]),
		Size:       int64(len(content)),
		ModifiedAt: time.Now().UTC(),
	}
}

func TestCreateOpenRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "proj.db")

	c, err := Create(p, "myns", "myproj")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.Path() != p {
		t.Errorf("Path() = %q, want %q", c.Path(), p)
	}
	files, subdirs, err := c.List("")
	if err != nil {
		t.Fatalf("List root: %v", err)
	}
	if len(files) != 0 || len(subdirs) != 0 {
		t.Errorf("fresh catalog not empty: files=%v subdirs=%v", files, subdirs)
	}
	_ = c.Close()

	c2, err := Open(p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer c2.Close()
	for k, want := range map[string]string{
		MetaSchemaVersion: CurrentSchemaVersion,
		MetaNamespace:     "myns",
		MetaProjectName:   "myproj",
		MetaState:         "healthy",
	} {
		got, err := c2.Meta(k)
		if err != nil {
			t.Fatalf("Meta(%q): %v", k, err)
		}
		if got != want {
			t.Errorf("Meta(%q) = %q, want %q", k, got, want)
		}
	}
	if ca, err := c2.Meta(MetaCreatedAt); err != nil || ca == "" {
		t.Errorf("Meta(created_at) = %q, err=%v; want non-empty", ca, err)
	}
}

func TestPutGetHasRoundTrip(t *testing.T) {
	c := newCatalog(t)
	e := entry("hello")
	if err := c.PutFile("directives/foo.md", e); err != nil {
		t.Fatalf("PutFile: %v", err)
	}
	got, ok, err := c.GetFile("directives/foo.md")
	if err != nil || !ok {
		t.Fatalf("GetFile: ok=%v err=%v", ok, err)
	}
	if got.Etag != e.Etag || got.Size != e.Size {
		t.Errorf("GetFile = %+v, want etag/size %s/%d", got, e.Etag, e.Size)
	}
	has, err := c.HasFile("directives/foo.md")
	if err != nil || !has {
		t.Errorf("HasFile = %v, err=%v; want true", has, err)
	}
	has, err = c.HasFile("directives/missing.md")
	if err != nil || has {
		t.Errorf("HasFile(missing) = %v, err=%v; want false", has, err)
	}
	_, ok, err = c.GetFile("nope.md")
	if err != nil || ok {
		t.Errorf("GetFile(missing) ok=%v err=%v; want false", ok, err)
	}
}

func TestPutThenDeleteLeavesEmpty(t *testing.T) {
	c := newCatalog(t)
	if err := c.PutFile("a.md", entry("x")); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteFile("a.md"); err != nil {
		t.Fatal(err)
	}
	if has, _ := c.HasFile("a.md"); has {
		t.Error("file still present after delete")
	}
	st, err := c.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.FileCount != 0 {
		t.Errorf("FileCount = %d, want 0", st.FileCount)
	}
	if st.DirCount != 1 { // only the root bucket survives
		t.Errorf("DirCount = %d, want 1 (root only)", st.DirCount)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	c := newCatalog(t)
	if err := c.DeleteFile("never/existed.md"); err != nil {
		t.Errorf("DeleteFile on absent path: %v", err)
	}
}

func TestNestedPutCreatesIntermediates(t *testing.T) {
	c := newCatalog(t)
	if err := c.PutFile("reports/progress/x.md", entry("y")); err != nil {
		t.Fatal(err)
	}
	// root -> reports -> reports/progress = 3 directory buckets.
	st, _ := c.Stats()
	if st.DirCount != 3 {
		t.Errorf("DirCount = %d, want 3", st.DirCount)
	}
	if st.FileCount != 1 {
		t.Errorf("FileCount = %d, want 1", st.FileCount)
	}
}

func TestNestedDeleteCleansUpToRoot(t *testing.T) {
	c := newCatalog(t)
	if err := c.PutFile("reports/progress/x.md", entry("y")); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteFile("reports/progress/x.md"); err != nil {
		t.Fatal(err)
	}
	st, _ := c.Stats()
	if st.DirCount != 1 {
		t.Errorf("DirCount = %d, want 1 (intermediates cleaned)", st.DirCount)
	}
	if st.FileCount != 0 {
		t.Errorf("FileCount = %d, want 0", st.FileCount)
	}
}

func TestNestedDeleteStopsAtNonEmptyAncestor(t *testing.T) {
	c := newCatalog(t)
	_ = c.PutFile("reports/a.md", entry("a"))
	_ = c.PutFile("reports/progress/x.md", entry("x"))
	if err := c.DeleteFile("reports/progress/x.md"); err != nil {
		t.Fatal(err)
	}
	// reports/progress is gone (empty), but reports survives (has a.md), as does root.
	st, _ := c.Stats()
	if st.DirCount != 2 {
		t.Errorf("DirCount = %d, want 2 (root + reports)", st.DirCount)
	}
	files, subdirs, _ := c.List("reports")
	if len(files) != 1 || files[0].Name != "a.md" {
		t.Errorf("reports files = %+v, want [a.md]", files)
	}
	if len(subdirs) != 0 {
		t.Errorf("reports subdirs = %v, want []", subdirs)
	}
}

func TestListRootFilesAndSubdirs(t *testing.T) {
	c := newCatalog(t)
	_ = c.PutFile("backlog.md", entry("b"))
	_ = c.PutFile("readme.md", entry("r"))
	_ = c.PutFile("directives/d1.md", entry("d"))
	_ = c.PutFile("reports/r1.md", entry("r"))

	files, subdirs, err := c.List("")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(files); !equal(got, []string{"backlog.md", "readme.md"}) {
		t.Errorf("root files = %v, want [backlog.md readme.md]", got)
	}
	if !equal(subdirs, []string{"directives", "reports"}) {
		t.Errorf("root subdirs = %v, want [directives reports]", subdirs)
	}
}

func TestListSubdirFilesAndSubdirs(t *testing.T) {
	c := newCatalog(t)
	_ = c.PutFile("reports/r1.md", entry("a"))
	_ = c.PutFile("reports/r2.md", entry("b"))
	_ = c.PutFile("reports/progress/p1.md", entry("c"))

	files, subdirs, err := c.List("reports")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(files); !equal(got, []string{"r1.md", "r2.md"}) {
		t.Errorf("reports files = %v, want [r1.md r2.md]", got)
	}
	if !equal(subdirs, []string{"progress"}) {
		t.Errorf("reports subdirs = %v, want [progress]", subdirs)
	}
}

func TestListNoBucketIsEmpty(t *testing.T) {
	c := newCatalog(t)
	files, subdirs, err := c.List("does/not/exist")
	if err != nil {
		t.Fatalf("List nonexistent: %v", err)
	}
	if len(files) != 0 || len(subdirs) != 0 {
		t.Errorf("nonexistent dir: files=%v subdirs=%v, want empty", files, subdirs)
	}
}

func TestListRootWithDeepNesting(t *testing.T) {
	c := newCatalog(t)
	_ = c.PutFile("root.md", entry("r"))
	_ = c.PutFile("dir1/a.md", entry("a"))
	_ = c.PutFile("dir1/dir2/b.md", entry("b"))

	files, subdirs, err := c.List("")
	if err != nil {
		t.Fatal(err)
	}
	if got := names(files); !equal(got, []string{"root.md"}) {
		t.Errorf("root files = %v, want [root.md]", got)
	}
	if !equal(subdirs, []string{"dir1"}) { // dir2 is NOT an immediate child of root
		t.Errorf("root subdirs = %v, want [dir1]", subdirs)
	}
}

func TestMetaSetGet(t *testing.T) {
	c := newCatalog(t)
	if err := c.SetMeta("custom", "value"); err != nil {
		t.Fatal(err)
	}
	got, err := c.Meta("custom")
	if err != nil || got != "value" {
		t.Errorf("Meta(custom) = %q, err=%v; want value", got, err)
	}
	if v, _ := c.Meta("unset-key"); v != "" {
		t.Errorf("Meta(unset) = %q, want empty", v)
	}
}

func TestVerifyInvariant(t *testing.T) {
	c := newCatalog(t)
	wt := t.TempDir()

	writeWT := func(rel, content string) {
		full := filepath.Join(wt, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Consistent: catalog matches working tree.
	writeWT("a.md", "alpha")
	writeWT("d/b.md", "bravo")
	_ = c.PutFile("a.md", entry("alpha"))
	_ = c.PutFile("d/b.md", entry("bravo"))

	v, err := c.VerifyInvariant(wt)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Fatalf("expected no violations, got %+v", v)
	}

	// Missing from working tree.
	_ = c.PutFile("d/gone.md", entry("ghost"))
	v, _ = c.VerifyInvariant(wt)
	if len(v) != 1 || v[0].Kind != "missing_from_working_tree" || v[0].Path != "d/gone.md" {
		t.Fatalf("expected one missing violation, got %+v", v)
	}
	_ = c.DeleteFile("d/gone.md")

	// Etag mismatch.
	writeWT("a.md", "ALPHA-CHANGED")
	v, _ = c.VerifyInvariant(wt)
	if len(v) != 1 || v[0].Kind != "etag_mismatch" || v[0].Path != "a.md" {
		t.Fatalf("expected one etag_mismatch, got %+v", v)
	}
	if v[0].Want != entry("alpha").Etag || v[0].Got != entry("ALPHA-CHANGED").Etag {
		t.Errorf("etag_mismatch want/got wrong: %+v", v[0])
	}
}

func TestStats(t *testing.T) {
	c := newCatalog(t)
	st, _ := c.Stats()
	if st.FileCount != 0 || st.DirCount != 1 || st.SchemaVer != CurrentSchemaVersion {
		t.Errorf("empty stats = %+v", st)
	}
	_ = c.PutFile("a.md", entry("a"))
	_ = c.PutFile("d/b.md", entry("b"))
	st, _ = c.Stats()
	if st.FileCount != 2 {
		t.Errorf("FileCount = %d, want 2", st.FileCount)
	}
	if st.DirCount != 2 { // root + d
		t.Errorf("DirCount = %d, want 2", st.DirCount)
	}
}

func TestOpenNotFound(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "absent.db"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Open(absent) err = %v, want ErrNotFound", err)
	}
}

func TestOpenSchemaMismatch(t *testing.T) {
	p := filepath.Join(t.TempDir(), "proj.db")
	c, err := Create(p, "ns", "proj")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetMeta(MetaSchemaVersion, "999"); err != nil {
		t.Fatal(err)
	}
	_ = c.Close()

	_, err = Open(p)
	if !errors.Is(err, ErrSchemaMismatch) {
		t.Errorf("Open(wrong schema) err = %v, want ErrSchemaMismatch", err)
	}
}

func TestOpenCorruptFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "corrupt.db")
	garbage := make([]byte, 16384) // 4 pages of zero bytes: invalid bbolt magic
	if err := os.WriteFile(p, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Open(p)
	if err == nil {
		_ = c.Close()
		t.Fatal("Open(corrupt) returned nil error")
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrSchemaMismatch) {
		t.Errorf("Open(corrupt) err = %v, want a wrapped bbolt error", err)
	}
}

func TestCreateExistingErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "proj.db")
	c, err := Create(p, "ns", "proj")
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	if _, err := Create(p, "ns", "proj"); err == nil {
		t.Error("Create on existing path returned nil error")
	}
}

// --- helpers ---

func names(files []ListEntry) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.Name
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
