package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/sopranoworks/shoka/internal/storage/deletedlog"
)

// makeMarkedDeletedLog creates a real (origin-marked) deleted-log at path via the package
// Create; withRecord adds one deletion so it is non-empty.
func makeMarkedDeletedLog(t *testing.T, path, ns, proj string, withRecord bool) {
	t.Helper()
	st, err := deletedlog.Create(path, ns, proj)
	if err != nil {
		t.Fatalf("create marked log %s: %v", path, err)
	}
	if withRecord {
		if err := st.Upsert(deletedlog.DeletedRecord{Path: "gone.md", DeletionCommit: "abc", DeletedAt: time.Unix(1, 0)}, 0); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Close()
}

// makeUnmarkedDeletedLog builds an OLD pre-marker junk file directly: the _meta bucket has the
// schema/created/ns/proj keys but NO `origin` key; withRecord adds one deletion.
func makeUnmarkedDeletedLog(t *testing.T, path, ns, proj string, withRecord bool) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	err = db.Update(func(tx *bolt.Tx) error {
		d, e := tx.CreateBucketIfNotExists([]byte("deleted"))
		if e != nil {
			return e
		}
		m, e := tx.CreateBucketIfNotExists([]byte("_meta"))
		if e != nil {
			return e
		}
		// Old pre-marker meta: schema_version must match so Open succeeds; NO origin key.
		_ = m.Put([]byte(deletedlog.MetaSchemaVersion), []byte(deletedlog.CurrentSchemaVersion))
		_ = m.Put([]byte(deletedlog.MetaCreatedAt), []byte("2026-01-01T00:00:00Z"))
		_ = m.Put([]byte(deletedlog.MetaNamespace), []byte(ns))
		_ = m.Put([]byte(deletedlog.MetaProjectName), []byte(proj))
		if withRecord {
			return d.Put([]byte("gone.md"), []byte(`{"path":"gone.md","deletion_commit":"x","deleted_at":"2026-01-01T00:00:00Z"}`))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// B3: cleanup removes ONLY unmarked + empty; marked (even empty) and any non-empty are kept.
func TestDeletedLogCleanup_RemovesOnlyUnmarkedEmpty(t *testing.T) {
	s := newEmptyStorage(t)
	dir := filepath.Join(s.baseDir, "t")
	ue := filepath.Join(dir, "ue.deleted.db") // unmarked empty  -> REMOVED
	me := filepath.Join(dir, "me.deleted.db") // marked empty    -> kept (legitimate, revived-empty)
	mn := filepath.Join(dir, "mn.deleted.db") // marked nonempty -> kept
	un := filepath.Join(dir, "un.deleted.db") // unmarked nonempty -> kept (record gate)

	makeUnmarkedDeletedLog(t, ue, "t", "ue", false)
	makeMarkedDeletedLog(t, me, "t", "me", false)
	makeMarkedDeletedLog(t, mn, "t", "mn", true)
	makeUnmarkedDeletedLog(t, un, "t", "un", true)

	removed, err := s.CleanupUnmarkedEmptyDeletedLogs()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(ue); !os.IsNotExist(err) {
		t.Fatalf("unmarked EMPTY junk must be REMOVED (err=%v); removed=%v", err, removed)
	}
	for name, p := range map[string]string{"marked-empty": me, "marked-nonempty": mn, "unmarked-nonempty": un} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s must be KEPT, but it is gone: %v", name, err)
		}
	}
	if len(removed) != 1 || removed[0] != projectKey("t", "ue") {
		t.Fatalf("removed = %v, want exactly [t/ue]", removed)
	}
}

// B3 #4: the one-time pass does NOT re-run once the done-flag exists.
func TestDeletedLogCleanup_OnceDoesNotRepeat(t *testing.T) {
	s := newEmptyStorage(t)
	// First invocation writes the done-flag (nothing to clean yet).
	s.deletedLogCleanupOnce()
	flag := filepath.Join(s.baseDir, ".shoka", deletedLogCleanupFlag)
	if _, err := os.Stat(flag); err != nil {
		t.Fatalf("done-flag should be written after the first pass: %v", err)
	}
	// Now drop an unmarked-empty junk file that WOULD be removed if the pass re-ran.
	junk := filepath.Join(s.baseDir, "t", "junk.deleted.db")
	makeUnmarkedDeletedLog(t, junk, "t", "junk", false)
	// Second invocation must SKIP (flag present) — the junk survives (no per-boot re-scan).
	s.deletedLogCleanupOnce()
	if _, err := os.Stat(junk); err != nil {
		t.Fatalf("cleanup re-ran despite the done-flag (junk removed): %v", err)
	}
}
