// Package catalog is the per-project cache of the files Shoka manages, backed by
// a single bbolt database at <base_dir>/<namespace>/<project>.project.db (one per
// project, kept open for the server's lifetime).
//
// The catalog records, for every Shoka-managed file, its etag, size, and
// modification time. It exists to make list_files fast and to exclude
// working-tree noise (.DS_Store, .claude/, anything Shoka did not write). It is
// a cache derived from git, disposable, and rebuilt automatically when missing
// or unreadable. One invariant governs it: if the catalog records a path, the
// working tree must contain content at that path (see the 2026-05-30 catalog
// design log and implementation directive).
//
// Bucket layout (bbolt buckets are flat/top-level; the path hierarchy is encoded
// in bucket names): one bucket per directory, named by the directory's absolute
// path with a leading slash — root is "/", a subdirectory is "/directives" or
// "/reports/progress". Within each bucket, file entries are stored as
// key=filename (no path prefix), value=JSON-encoded FileEntry. Subdirectory
// existence is implied by the existence of the corresponding bucket; there are
// no marker keys. One special top-level bucket, "_meta", holds the meta rows
// (the leading underscore avoids collision with a directory literally named
// "meta", whose bucket would be "/meta").
package catalog

import (
	"crypto/sha256"
	"encoding/hex"
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

// FileEntry is what the catalog stores for each managed file.
type FileEntry struct {
	Etag       string    `json:"etag"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"` // RFC3339Nano UTC when serialised
}

// MetaKey constants — fields stored in the meta bucket.
const (
	MetaSchemaVersion = "schema_version" // "1" for this directive's release
	MetaCreatedAt     = "created_at"     // RFC3339Nano UTC
	MetaNamespace     = "namespace"
	MetaProjectName   = "project_name"
	MetaState         = "state" // last-known state; not authoritative at runtime
)

// CurrentSchemaVersion is the schema version this build writes and requires.
const CurrentSchemaVersion = "1"

const (
	metaBucket = "_meta"
	rootBucket = "/"
)

// Errors returned by the package.
var (
	ErrNotFound       = errors.New("catalog: file not found")
	ErrSchemaMismatch = errors.New("catalog: schema version mismatch")
	ErrCorrupt        = errors.New("catalog: corrupt")
)

// Catalog is the per-project catalog handle. One per project, kept open for the
// server's lifetime. Safe for concurrent use; bbolt's transaction model
// serialises writes and allows concurrent reads.
type Catalog struct {
	db   *bolt.DB
	path string
}

// Open opens the catalog at path. Returns ErrNotFound if the file does not
// exist (the caller decides whether to rebuild). Returns ErrSchemaMismatch if
// the file exists but its schema_version is not CurrentSchemaVersion. Other
// errors (corrupt file, permission, lock timeout, etc.) wrap the underlying
// bbolt error.
func Open(path string) (*Catalog, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		// A file that is not a valid bbolt database (e.g. arbitrary garbage, a
		// truncated or version-mismatched DB) is a corruption to be rebuilt from
		// git, distinct from a lock timeout or permission error (unreadable).
		if errors.Is(err, bolt.ErrInvalid) ||
			errors.Is(err, bolt.ErrVersionMismatch) ||
			errors.Is(err, bolt.ErrChecksum) {
			return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		return nil, fmt.Errorf("catalog: open %s: %w", path, err)
	}
	c := &Catalog{db: db, path: path}
	sv, err := c.Meta(MetaSchemaVersion)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("%w: reading schema version: %v", ErrCorrupt, err)
	}
	if sv != CurrentSchemaVersion {
		_ = db.Close()
		return nil, ErrSchemaMismatch
	}
	return c, nil
}

// Create creates a new empty catalog at path. Fails if the file already exists;
// the caller must remove (or rename) the existing file first. namespace and
// projectName populate the meta bucket; created_at is set to time.Now().UTC().
func Create(path, namespace, projectName string) (*Catalog, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("catalog: file already exists: %s", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("catalog: create parent dir: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("catalog: create %s: %w", path, err)
	}
	c := &Catalog{db: db, path: path}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err = db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists([]byte(rootBucket)); err != nil {
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
			{MetaState, "healthy"},
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
		return nil, fmt.Errorf("catalog: initialise %s: %w", path, err)
	}
	return c, nil
}

// Close closes the underlying bbolt DB.
func (c *Catalog) Close() error {
	if c == nil || c.db == nil {
		return nil
	}
	return c.db.Close()
}

// Path returns the catalog's filesystem path (useful for logging).
func (c *Catalog) Path() string { return c.path }

// PutFile inserts or updates a file entry at the given path. The path is
// normalised to forward-slash form (no leading "/"). Intermediate directory
// buckets are created as needed.
func (c *Catalog) PutFile(p string, entry FileEntry) error {
	np := normalizePath(p)
	if np == "" {
		return fmt.Errorf("catalog: cannot put a file at an empty path")
	}
	dir, file := splitDirFile(np)
	val, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("catalog: encode entry: %w", err)
	}
	return c.db.Update(func(tx *bolt.Tx) error {
		b, err := ensureBuckets(tx, dir)
		if err != nil {
			return err
		}
		return b.Put([]byte(file), val)
	})
}

// DeleteFile removes a file entry. Returns nil if the entry did not exist; the
// operation is idempotent. Also removes any now-empty parent directory buckets
// (recursive cleanup), never the root bucket.
func (c *Catalog) DeleteFile(p string) error {
	np := normalizePath(p)
	if np == "" {
		return nil
	}
	dir, file := splitDirFile(np)
	return c.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName(dir)))
		if b == nil {
			return nil
		}
		if err := b.Delete([]byte(file)); err != nil {
			return err
		}
		return cleanupEmptyBuckets(tx, dir)
	})
}

// GetFile returns the entry at path, or false if not present.
func (c *Catalog) GetFile(p string) (FileEntry, bool, error) {
	var entry FileEntry
	np := normalizePath(p)
	if np == "" {
		return entry, false, nil
	}
	dir, file := splitDirFile(np)
	found := false
	err := c.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketName(dir)))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(file))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &entry)
	})
	if err != nil {
		return FileEntry{}, false, err
	}
	return entry, found, nil
}

// HasFile is GetFile without returning the entry. Used by read_file's ENOENT
// path to detect invariant violations.
func (c *Catalog) HasFile(p string) (bool, error) {
	_, ok, err := c.GetFile(p)
	return ok, err
}

// ListEntry is a file returned by List, with its catalog data.
type ListEntry struct {
	Name  string    // last path segment (no leading directory)
	Entry FileEntry // file's data
}

// List returns the direct children of the given directory path. path is the
// directory ("" = root, "directives" = directives directory). Returns the file
// entries and the immediate subdirectory names (without trailing slash), each
// sorted ascending. Returns an empty result (no error) if the directory does
// not exist.
func (c *Catalog) List(p string) (files []ListEntry, subdirs []string, err error) {
	dir := normalizePath(p)
	bn := bucketName(dir)
	files = []ListEntry{}
	subdirs = []string{}
	childPrefix := bn
	if bn != rootBucket {
		childPrefix = bn + "/"
	}
	err = c.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte(bn)); b != nil {
			cur := b.Cursor()
			for k, v := cur.First(); k != nil; k, v = cur.Next() {
				if v == nil {
					continue
				}
				var e FileEntry
				if uerr := json.Unmarshal(v, &e); uerr != nil {
					return uerr
				}
				files = append(files, ListEntry{Name: string(k), Entry: e})
			}
		}
		return tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
			n := string(name)
			if n == metaBucket || n == bn {
				return nil
			}
			if !strings.HasPrefix(n, childPrefix) {
				return nil
			}
			rem := n[len(childPrefix):]
			if rem == "" || strings.Contains(rem, "/") {
				return nil // not an immediate child
			}
			subdirs = append(subdirs, rem)
			return nil
		})
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	sort.Strings(subdirs)
	return files, subdirs, nil
}

// Meta returns a meta-bucket value. Returns "" if unset.
func (c *Catalog) Meta(key string) (string, error) {
	var val string
	err := c.db.View(func(tx *bolt.Tx) error {
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
func (c *Catalog) SetMeta(key, value string) error {
	return c.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(metaBucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(key), []byte(value))
	})
}

// InvariantViolation describes a single inconsistency between the catalog and a
// working tree. Returned by VerifyInvariant.
type InvariantViolation struct {
	Path string
	Kind string // "missing_from_working_tree" | "etag_mismatch"
	Want string // catalog etag (for etag_mismatch)
	Got  string // working-tree etag (for etag_mismatch)
}

// VerifyInvariant walks the catalog and checks every entry against the working
// tree at workingTreeRoot. Returns violations (empty slice if everything is
// consistent). Used by drift detection and tests; not on the hot path.
func (c *Catalog) VerifyInvariant(workingTreeRoot string) ([]InvariantViolation, error) {
	violations := []InvariantViolation{}
	err := c.db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, b *bolt.Bucket) error {
			n := string(name)
			if n == metaBucket {
				return nil
			}
			dir := strings.TrimPrefix(n, "/")
			return b.ForEach(func(k, v []byte) error {
				if v == nil {
					return nil
				}
				var e FileEntry
				if uerr := json.Unmarshal(v, &e); uerr != nil {
					return uerr
				}
				rel := string(k)
				if dir != "" {
					rel = dir + "/" + string(k)
				}
				full := filepath.Join(workingTreeRoot, filepath.FromSlash(rel))
				data, rerr := os.ReadFile(full)
				if rerr != nil {
					if errors.Is(rerr, fs.ErrNotExist) {
						violations = append(violations, InvariantViolation{
							Path: rel, Kind: "missing_from_working_tree",
						})
						return nil
					}
					return rerr
				}
				if got := sha256Hex(data); got != e.Etag {
					violations = append(violations, InvariantViolation{
						Path: rel, Kind: "etag_mismatch", Want: e.Etag, Got: got,
					})
				}
				return nil
			})
		})
	})
	if err != nil {
		return nil, err
	}
	return violations, nil
}

// Stats reports aggregate counts for metrics.
type Stats struct {
	FileCount int
	DirCount  int // count of distinct directory buckets (includes the root)
	SchemaVer string
}

// Stats returns aggregate counts.
func (c *Catalog) Stats() (Stats, error) {
	var st Stats
	err := c.db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, b *bolt.Bucket) error {
			if string(name) == metaBucket {
				return nil
			}
			st.DirCount++
			cur := b.Cursor()
			for k, v := cur.First(); k != nil; k, v = cur.Next() {
				if v != nil {
					st.FileCount++
				}
			}
			return nil
		})
	})
	if err != nil {
		return Stats{}, err
	}
	sv, err := c.Meta(MetaSchemaVersion)
	if err != nil {
		return Stats{}, err
	}
	st.SchemaVer = sv
	return st, nil
}

// --- internal helpers -------------------------------------------------------

// normalizePath returns the slash-form within-project path with leading and
// trailing slashes removed and "." / ".." collapsed. "" / "." / "/" all map to
// "" (the root directory).
func normalizePath(p string) string {
	p = filepath.ToSlash(p)
	p = strings.Trim(p, "/")
	if p == "" || p == "." {
		return ""
	}
	p = path.Clean(p)
	return strings.Trim(p, "/")
}

// splitDirFile splits a normalised path into its directory (normalised, no
// leading slash, "" for root) and final segment.
func splitDirFile(np string) (dir, file string) {
	if i := strings.LastIndex(np, "/"); i >= 0 {
		return np[:i], np[i+1:]
	}
	return "", np
}

// bucketName maps a normalised directory path to its bucket name: "" -> "/",
// "reports/progress" -> "/reports/progress".
func bucketName(dir string) string {
	if dir == "" {
		return rootBucket
	}
	return "/" + dir
}

// ensureBuckets creates the root bucket and every ancestor bucket for dir,
// returning the bucket for dir itself.
func ensureBuckets(tx *bolt.Tx, dir string) (*bolt.Bucket, error) {
	root, err := tx.CreateBucketIfNotExists([]byte(rootBucket))
	if err != nil {
		return nil, err
	}
	if dir == "" {
		return root, nil
	}
	var b *bolt.Bucket = root
	cum := ""
	for _, seg := range strings.Split(dir, "/") {
		cum += "/" + seg
		b, err = tx.CreateBucketIfNotExists([]byte(cum))
		if err != nil {
			return nil, err
		}
	}
	return b, nil
}

// cleanupEmptyBuckets deletes dir's bucket and walks up its ancestors, deleting
// each that has become empty (no file keys and no descendant directory
// buckets). The root bucket is never deleted.
func cleanupEmptyBuckets(tx *bolt.Tx, dir string) error {
	for dir != "" {
		bn := bucketName(dir)
		if b := tx.Bucket([]byte(bn)); b != nil {
			if bucketHasKeys(b) || hasDescendantBuckets(tx, bn) {
				return nil // not empty; stop climbing
			}
			if err := tx.DeleteBucket([]byte(bn)); err != nil {
				return err
			}
		}
		if i := strings.LastIndex(dir, "/"); i >= 0 {
			dir = dir[:i]
		} else {
			dir = ""
		}
	}
	return nil
}

func bucketHasKeys(b *bolt.Bucket) bool {
	k, _ := b.Cursor().First()
	return k != nil
}

func hasDescendantBuckets(tx *bolt.Tx, bn string) bool {
	prefix := bn + "/"
	found := false
	_ = tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
		if strings.HasPrefix(string(name), prefix) {
			found = true
		}
		return nil
	})
	return found
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
