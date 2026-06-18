// Package deletedlog is the per-project derivative store of CURRENTLY-DELETED
// files (the 2026-06-18 deleted-log directive), backed by a single bbolt database
// at the sibling path <base_dir>/<namespace>/<project>.deleted.db (one per
// project, kept open for the server's lifetime, alongside the catalog's
// <project>.db and the index's <project>.index.db).
//
// It is a SEPARATE store, deliberately NOT a bucket in the index: the index holds
// only present paths (its invariant), whereas this holds the inverse — paths that
// no longer exist — so keeping it apart preserves the index's present-paths-only
// invariant.
//
// It is a DERIVATIVE of the truth (git): disposable, rebuilt by a bounded
// recent-commit repair walk when the file is absent (the caller in package
// storage owns that walk). It records, per currently-deleted path, the commit
// that deleted it — so revival is the O(1) deletion-commit->parent->blob path with
// no walk. It is a LIVE set: the commit-land hook upserts on a delete and drops on
// a (re)create or move-source, so a re-created path does not linger as deleted.
// The set is FIFO-CAPPED at max_entries (oldest by DeletedAt evicted past the cap)
// — past that, divergence from git is accepted (the directive's policy 4).
//
// This package reads no git: it imports no go-git and is fed records by its caller
// in internal/storage. (internal/storage/deletedlog sits under the archlint
// allowlist prefix, so go-git-freeness here is a by-construction boundary, like
// internal/storage/index and internal/storage/walworker — not linter-enforced.)
package deletedlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// DeletedRecord is the per-path record of a currently-deleted file. DeletionCommit
// is the git commit that removed it; revival reads the file's content at that
// commit's PARENT (the last-present content) and writes it back as a new commit. It
// is a struct so fields are added with NO migration (an older record decodes with
// new fields zero-valued).
type DeletedRecord struct {
	Path           string    `json:"path"`
	DeletionCommit string    `json:"deletion_commit"`
	DeletedAt      time.Time `json:"deleted_at"`
}

// Meta keys held in the _meta bucket.
const (
	MetaSchemaVersion = "schema_version" // CurrentSchemaVersion
	MetaCreatedAt     = "created_at"     // RFC3339Nano UTC
	MetaNamespace     = "namespace"      //
	MetaProjectName   = "project_name"   //
)

// CurrentSchemaVersion is the schema version this build writes and requires. Since
// the store is disposable, an incompatible bump simply triggers a rebuild.
const CurrentSchemaVersion = "1"

const (
	metaBucket    = "_meta"
	deletedBucket = "deleted"
)

// Errors mirror the index/catalog disposition ladder so the caller treats a
// missing/corrupt/mismatched store identically: rebuild.
var (
	ErrNotFound       = errors.New("deletedlog: not found")
	ErrCorrupt        = errors.New("deletedlog: corrupt")
	ErrSchemaMismatch = errors.New("deletedlog: schema version mismatch")
)

// Store is the per-project deleted-log handle. One per project, kept open for the
// server's lifetime. Safe for concurrent use; bbolt serialises writes.
type Store struct {
	db   *bolt.DB
	path string
}

// Open opens the store at path. Returns ErrNotFound if the file does not exist,
// ErrCorrupt if it is not a valid bbolt database, and ErrSchemaMismatch on a
// schema mismatch. The caller treats all three as "absent" and rebuilds (the
// trigger-(a) repair walk).
func Open(p string) (*Store, error) {
	if _, err := os.Stat(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	db, err := bolt.Open(p, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		if errors.Is(err, bolt.ErrInvalid) ||
			errors.Is(err, bolt.ErrVersionMismatch) ||
			errors.Is(err, bolt.ErrChecksum) {
			return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		return nil, fmt.Errorf("deletedlog: open %s: %w", p, err)
	}
	st := &Store{db: db, path: p}
	sv, err := st.meta(MetaSchemaVersion)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%w: reading schema version: %v", ErrCorrupt, err)
	}
	if sv != CurrentSchemaVersion {
		_ = db.Close()
		return nil, ErrSchemaMismatch
	}
	return st, nil
}

// Create creates a new empty store at path. Fails if the file already exists.
func Create(p, namespace, projectName string) (*Store, error) {
	if _, err := os.Stat(p); err == nil {
		return nil, fmt.Errorf("deletedlog: file already exists: %s", p)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, fmt.Errorf("deletedlog: create parent dir: %w", err)
	}
	db, err := bolt.Open(p, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("deletedlog: create %s: %w", p, err)
	}
	st := &Store{db: db, path: p}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err = db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists([]byte(deletedBucket)); err != nil {
			return err
		}
		meta, err := tx.CreateBucketIfNotExists([]byte(metaBucket))
		if err != nil {
			return err
		}
		rows := []struct{ k, v string }{
			{MetaSchemaVersion, CurrentSchemaVersion},
			{MetaCreatedAt, now},
			{MetaNamespace, namespace},
			{MetaProjectName, projectName},
		}
		for _, r := range rows {
			if err := meta.Put([]byte(r.k), []byte(r.v)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		_ = os.Remove(p)
		return nil, fmt.Errorf("deletedlog: initialise %s: %w", p, err)
	}
	return st, nil
}

// Close closes the underlying bbolt DB.
func (st *Store) Close() error {
	if st == nil || st.db == nil {
		return nil
	}
	return st.db.Close()
}

// Path returns the store's filesystem path (useful for logging).
func (st *Store) Path() string { return st.path }

// Upsert records (or refreshes) a deletion at rec.Path, then enforces the FIFO
// cap: if the record count exceeds maxEntries, the oldest entries by DeletedAt are
// evicted until the count is within the cap. maxEntries <= 0 disables the cap.
func (st *Store) Upsert(rec DeletedRecord, maxEntries int) error {
	np := normalizePath(rec.Path)
	if np == "" {
		return fmt.Errorf("deletedlog: cannot record a deletion at an empty path")
	}
	rec.Path = np
	val, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("deletedlog: encode record: %w", err)
	}
	return st.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(deletedBucket))
		if err != nil {
			return err
		}
		if err := b.Put([]byte(np), val); err != nil {
			return err
		}
		return evictOldest(b, maxEntries)
	})
}

// Drop removes the record at a path (a (re)create or move-source on that path —
// the path is no longer deleted). Idempotent: dropping an absent path is not an
// error.
func (st *Store) Drop(p string) error {
	np := normalizePath(p)
	if np == "" {
		return nil
	}
	return st.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(deletedBucket))
		if b == nil {
			return nil
		}
		return b.Delete([]byte(np))
	})
}

// Get returns the record at a path, or false if not present.
func (st *Store) Get(p string) (DeletedRecord, bool, error) {
	var rec DeletedRecord
	np := normalizePath(p)
	if np == "" {
		return rec, false, nil
	}
	found := false
	err := st.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(deletedBucket))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(np))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &rec)
	})
	if err != nil {
		return DeletedRecord{}, false, err
	}
	return rec, found, nil
}

// List returns every currently-deleted record, sorted by path. This is the cheap
// everyday read: O(cap), a single bucket scan, no git walk and no on-read
// validation against git (divergence is discovered only at revival).
func (st *Store) List() ([]DeletedRecord, error) {
	var out []DeletedRecord
	err := st.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(deletedBucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(_, v []byte) error {
			var rec DeletedRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return nil // skip an undecodable record rather than fail the scan
			}
			out = append(out, rec)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// Count returns the number of records (for tests + cap assertions).
func (st *Store) Count() (int, error) {
	n := 0
	err := st.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(deletedBucket))
		if b == nil {
			return nil
		}
		cur := b.Cursor()
		for k, _ := cur.First(); k != nil; k, _ = cur.Next() {
			n++
		}
		return nil
	})
	return n, err
}

// ReplaceAll atomically replaces the entire deleted set with records, applying the
// FIFO cap (keeping the newest maxEntries by DeletedAt). This is the wholesale
// rebuild the bounded repair walk performs: the bucket is dropped and recreated so
// stale entries cannot survive, then the capped record set is written — all or
// nothing.
func (st *Store) ReplaceAll(records []DeletedRecord, maxEntries int) error {
	kept := capNewest(records, maxEntries)
	return st.db.Update(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte(deletedBucket)); b != nil {
			if err := tx.DeleteBucket([]byte(deletedBucket)); err != nil {
				return err
			}
		}
		b, err := tx.CreateBucket([]byte(deletedBucket))
		if err != nil {
			return err
		}
		for _, rec := range kept {
			np := normalizePath(rec.Path)
			if np == "" {
				continue
			}
			rec.Path = np
			val, merr := json.Marshal(rec)
			if merr != nil {
				return fmt.Errorf("deletedlog: encode record %q: %w", rec.Path, merr)
			}
			if err := b.Put([]byte(np), val); err != nil {
				return err
			}
		}
		return nil
	})
}

func (st *Store) meta(key string) (string, error) {
	var val string
	err := st.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(metaBucket))
		if b == nil {
			return nil
		}
		if v := b.Get([]byte(key)); v != nil {
			val = string(v)
		}
		return nil
	})
	return val, err
}

// evictOldest deletes the oldest records by DeletedAt until the bucket holds at
// most maxEntries (no-op when maxEntries <= 0 or already within the cap). Runs
// inside the Upsert write transaction.
func evictOldest(b *bolt.Bucket, maxEntries int) error {
	if maxEntries <= 0 {
		return nil
	}
	type kv struct {
		key []byte
		at  time.Time
	}
	var all []kv
	if err := b.ForEach(func(k, v []byte) error {
		var rec DeletedRecord
		if err := json.Unmarshal(v, &rec); err != nil {
			// An undecodable record is the best eviction candidate (zero time).
			all = append(all, kv{key: append([]byte(nil), k...)})
			return nil
		}
		all = append(all, kv{key: append([]byte(nil), k...), at: rec.DeletedAt})
		return nil
	}); err != nil {
		return err
	}
	if len(all) <= maxEntries {
		return nil
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
	for _, e := range all[:len(all)-maxEntries] {
		if err := b.Delete(e.key); err != nil {
			return err
		}
	}
	return nil
}

// capNewest returns the newest maxEntries records by DeletedAt (all when
// maxEntries <= 0 or fewer records than the cap).
func capNewest(records []DeletedRecord, maxEntries int) []DeletedRecord {
	if maxEntries <= 0 || len(records) <= maxEntries {
		return records
	}
	sorted := append([]DeletedRecord(nil), records...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].DeletedAt.After(sorted[j].DeletedAt) })
	return sorted[:maxEntries]
}

// normalizePath returns the slash-form within-project path with leading/trailing
// slashes removed and "." / ".." collapsed. "" / "." / "/" all map to "".
func normalizePath(p string) string {
	p = filepath.ToSlash(p)
	p = strings.Trim(p, "/")
	if p == "" || p == "." {
		return ""
	}
	p = path.Clean(p)
	return strings.Trim(p, "/")
}
