// Package index is the per-project derivative index store (the 2026-06-04 I1
// directive), backed by a single bbolt database at the sibling path
// <base_dir>/<namespace>/<project>.index.db (one per project, kept open for the
// server's lifetime, alongside the catalog's <project>.db).
//
// I1 lays the substrate only: a forward map keyed by within-project path holding
// the per-file derived record both later indexes build on, plus a per-project
// last_indexed_commit marker. It deliberately does NOT define the full-text
// bigram postings query schema (I2) or the reverse-link referrer inversion (I3);
// those extend this store by adding fields to IndexRecord and/or new buckets.
//
// The store is a DERIVATIVE of the truth (the working tree / git): it is
// disposable and rebuilt wholesale from working-tree bytes when missing, corrupt,
// or stale. Opening a missing or corrupt index.db is NOT an error to a caller —
// it degrades to "absent", which the repair sweep then rebuilds. Crash-safety is
// by fallback-and-repair, not WAL-level durability.
//
// This package reads no git: it imports no go-git and is fed working-tree bytes
// by its caller in internal/storage. (internal/storage/index sits under the
// archlint allowlist prefix, so go-git-freeness here is a by-construction
// boundary, like internal/storage/walworker — not linter-enforced.)
package index

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// IndexRecord is the per-file derived record I1 stores. I1 holds only the
// content etag — enough to make incremental upsert/delete meaningful and to let
// the repair sweep diff cheaply. It is a struct (not a bare string) so I2 can add
// bigram-posting references and I3 can add outbound links as new JSON fields with
// NO migration: a record written by an older build decodes with the new fields at
// their zero values.
type IndexRecord struct {
	Etag string `json:"etag"`
}

// Meta keys held in the _meta bucket.
const (
	MetaSchemaVersion     = "schema_version"      // CurrentSchemaVersion
	MetaCreatedAt         = "created_at"          // RFC3339Nano UTC
	MetaNamespace         = "namespace"           //
	MetaProjectName       = "project_name"        //
	MetaLastIndexedCommit = "last_indexed_commit" // git HEAD the index reflects ("" = none yet)
)

// CurrentSchemaVersion is the schema version this build writes and requires. I2
// and I3 extend IndexRecord additively (zero-valued new fields), so they do not
// bump this; a bump is reserved for an incompatible on-disk layout change, which
// — because the store is disposable — simply triggers a rebuild.
const CurrentSchemaVersion = "1"

const (
	metaBucket  = "_meta"
	filesBucket = "files"
)

// Errors returned by the package, mirroring the catalog's disposition ladder so
// the caller treats a missing/corrupt/mismatched store identically: rebuild.
var (
	ErrNotFound       = errors.New("index: not found")
	ErrCorrupt        = errors.New("index: corrupt")
	ErrSchemaMismatch = errors.New("index: schema version mismatch")
)

// Index is the per-project index handle. One per project, kept open for the
// server's lifetime. Safe for concurrent use; bbolt serialises writes and allows
// concurrent reads.
type Index struct {
	db   *bolt.DB
	path string
}

// Open opens the index at path. Returns ErrNotFound if the file does not exist,
// ErrCorrupt if it is not a valid bbolt database, and ErrSchemaMismatch if its
// schema_version is not CurrentSchemaVersion. The caller treats all three as
// "absent" and rebuilds.
func Open(path string) (*Index, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		// Garbage / truncated / version-mismatched file: a corruption to rebuild
		// from the truth, distinct from a lock timeout or permission error.
		if errors.Is(err, bolt.ErrInvalid) ||
			errors.Is(err, bolt.ErrVersionMismatch) ||
			errors.Is(err, bolt.ErrChecksum) {
			return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		return nil, fmt.Errorf("index: open %s: %w", path, err)
	}
	idx := &Index{db: db, path: path}
	sv, err := idx.Meta(MetaSchemaVersion)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%w: reading schema version: %v", ErrCorrupt, err)
	}
	if sv != CurrentSchemaVersion {
		_ = db.Close()
		return nil, ErrSchemaMismatch
	}
	return idx, nil
}

// Create creates a new empty index at path. Fails if the file already exists; the
// caller removes (or renames) the existing file first. namespace and projectName
// populate the meta bucket; created_at is time.Now().UTC(); last_indexed_commit
// starts "" (nothing indexed yet).
func Create(path, namespace, projectName string) (*Index, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("index: file already exists: %s", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("index: create parent dir: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("index: create %s: %w", path, err)
	}
	idx := &Index{db: db, path: path}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err = db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists([]byte(filesBucket)); err != nil {
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
			{MetaLastIndexedCommit, ""},
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
		_ = os.Remove(path)
		return nil, fmt.Errorf("index: initialise %s: %w", path, err)
	}
	return idx, nil
}

// Close closes the underlying bbolt DB.
func (idx *Index) Close() error {
	if idx == nil || idx.db == nil {
		return nil
	}
	return idx.db.Close()
}

// Path returns the index's filesystem path (useful for logging).
func (idx *Index) Path() string { return idx.path }

// PutRecord inserts or updates the record at a within-project path. The path is
// normalised to forward-slash form (no leading "/").
func (idx *Index) PutRecord(p string, rec IndexRecord) error {
	np := normalizePath(p)
	if np == "" {
		return fmt.Errorf("index: cannot put a record at an empty path")
	}
	val, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("index: encode record: %w", err)
	}
	return idx.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(filesBucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(np), val)
	})
}

// DeleteRecord removes the record at a path. Idempotent: removing an absent path
// is not an error.
func (idx *Index) DeleteRecord(p string) error {
	np := normalizePath(p)
	if np == "" {
		return nil
	}
	return idx.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(filesBucket))
		if b == nil {
			return nil
		}
		return b.Delete([]byte(np))
	})
}

// GetRecord returns the record at a path, or false if not present.
func (idx *Index) GetRecord(p string) (IndexRecord, bool, error) {
	var rec IndexRecord
	np := normalizePath(p)
	if np == "" {
		return rec, false, nil
	}
	found := false
	err := idx.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(filesBucket))
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
		return IndexRecord{}, false, err
	}
	return rec, found, nil
}

// ReplaceAll atomically replaces the entire forward map with records and sets
// last_indexed_commit to commit, in a single transaction. This is the wholesale
// rebuild the repair sweep performs from working-tree bytes (I1 build order §3.3,
// decision 4): the files bucket is dropped and recreated so stale paths cannot
// survive, then every record is written and the marker advanced — all-or-nothing.
func (idx *Index) ReplaceAll(records map[string]IndexRecord, commit string) error {
	return idx.db.Update(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte(filesBucket)); b != nil {
			if err := tx.DeleteBucket([]byte(filesBucket)); err != nil {
				return err
			}
		}
		b, err := tx.CreateBucket([]byte(filesBucket))
		if err != nil {
			return err
		}
		for p, rec := range records {
			np := normalizePath(p)
			if np == "" {
				continue
			}
			val, merr := json.Marshal(rec)
			if merr != nil {
				return fmt.Errorf("index: encode record %q: %w", p, merr)
			}
			if err := b.Put([]byte(np), val); err != nil {
				return err
			}
		}
		meta, err := tx.CreateBucketIfNotExists([]byte(metaBucket))
		if err != nil {
			return err
		}
		return meta.Put([]byte(MetaLastIndexedCommit), []byte(commit))
	})
}

// Count returns the number of records in the forward map (for tests + inertness
// assertions).
func (idx *Index) Count() (int, error) {
	n := 0
	err := idx.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(filesBucket))
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

// LastIndexedCommit returns the git HEAD the index currently reflects, or "" if
// nothing has been indexed yet.
func (idx *Index) LastIndexedCommit() (string, error) {
	return idx.Meta(MetaLastIndexedCommit)
}

// SetLastIndexedCommit records the git HEAD the index reflects.
func (idx *Index) SetLastIndexedCommit(commit string) error {
	return idx.SetMeta(MetaLastIndexedCommit, commit)
}

// Meta returns a meta-bucket value. Returns "" if unset.
func (idx *Index) Meta(key string) (string, error) {
	var val string
	err := idx.db.View(func(tx *bolt.Tx) error {
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

// SetMeta sets a meta-bucket value.
func (idx *Index) SetMeta(key, value string) error {
	return idx.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(metaBucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(key), []byte(value))
	})
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
