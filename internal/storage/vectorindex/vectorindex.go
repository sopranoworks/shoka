// Package vectorindex is the per-project vector embedding store, backed by a
// single bbolt database at <base_dir>/<namespace>/<project>.vector.db (one per
// project, a sibling alongside the catalog, index, and deleted-log DBs).
//
// KV structure: K = project-relative file path, V = binary-encoded float64 vector.
// Metadata bucket stores model name and dimensions so a config change (different
// model or resulting dimensions) is detected and triggers a full rebuild.
//
// The store is a DERIVATIVE of the truth (the working tree): it is disposable and
// rebuilt when missing, corrupt, or when the model/dimensions change. Like the
// fulltext index, crash-safety is by fallback-and-repair, not WAL-level durability.
//
// This package has no LLM/embedder dependency — it stores pre-computed vectors
// passed in by the caller (the storage layer's vector worker).
package vectorindex

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	metaBucket    = "_meta"
	vectorsBucket = "vectors"
)

const (
	MetaSchemaVersion = "schema_version"
	MetaCreatedAt     = "created_at"
	MetaNamespace     = "namespace"
	MetaProjectName   = "project_name"
	MetaModel         = "model"
	MetaDimensions    = "dimensions"
)

const CurrentSchemaVersion = "1"

var (
	ErrNotFound       = errors.New("vectorindex: not found")
	ErrCorrupt        = errors.New("vectorindex: corrupt")
	ErrSchemaMismatch = errors.New("vectorindex: schema version mismatch")
	ErrModelMismatch  = errors.New("vectorindex: model or dimensions changed")
)

// Store is the per-project vector index handle. One per project, kept open for
// the server's lifetime. Safe for concurrent use; bbolt serialises writes and
// allows concurrent reads.
type Store struct {
	db   *bolt.DB
	path string
}

// Open opens the vector index at path. Returns ErrNotFound if the file does not
// exist, ErrCorrupt if it is not a valid bbolt database, and ErrSchemaMismatch
// if its schema_version is not CurrentSchemaVersion.
func Open(path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		if errors.Is(err, bolt.ErrInvalid) ||
			errors.Is(err, bolt.ErrVersionMismatch) ||
			errors.Is(err, bolt.ErrChecksum) {
			return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
		}
		return nil, fmt.Errorf("vectorindex: open %s: %w", path, err)
	}
	st := &Store{db: db, path: path}
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

// CheckModel verifies the stored model and dimensions match the expected values.
// Returns ErrModelMismatch if they differ.
func (st *Store) CheckModel(model string, dimensions int) error {
	m, _ := st.meta(MetaModel)
	d, _ := st.meta(MetaDimensions)
	if m != model || d != fmt.Sprintf("%d", dimensions) {
		return ErrModelMismatch
	}
	return nil
}

// Create creates a new empty vector index at path with the given model and
// dimensions metadata.
func Create(path, namespace, projectName, model string, dimensions int) (*Store, error) {
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("vectorindex: file already exists: %s", path)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("vectorindex: create parent dir: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("vectorindex: create %s: %w", path, err)
	}
	st := &Store{db: db, path: path}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	err = db.Update(func(tx *bolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists([]byte(vectorsBucket)); err != nil {
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
			{MetaModel, model},
			{MetaDimensions, fmt.Sprintf("%d", dimensions)},
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
		return nil, fmt.Errorf("vectorindex: initialise %s: %w", path, err)
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

// Path returns the store's filesystem path.
func (st *Store) Path() string { return st.path }

// Put inserts or updates the vector at a within-project path.
func (st *Store) Put(p string, vector []float64) error {
	np := normalizePath(p)
	if np == "" {
		return fmt.Errorf("vectorindex: cannot put at an empty path")
	}
	return st.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(vectorsBucket))
		if err != nil {
			return err
		}
		return b.Put([]byte(np), encodeVector(vector))
	})
}

// Delete removes the vector at a path. Idempotent.
func (st *Store) Delete(p string) error {
	np := normalizePath(p)
	if np == "" {
		return nil
	}
	return st.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(vectorsBucket))
		if b == nil {
			return nil
		}
		return b.Delete([]byte(np))
	})
}

// Get returns the vector at a path, or false if not present.
func (st *Store) Get(p string) ([]float64, bool, error) {
	np := normalizePath(p)
	if np == "" {
		return nil, false, nil
	}
	var vec []float64
	found := false
	err := st.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(vectorsBucket))
		if b == nil {
			return nil
		}
		v := b.Get([]byte(np))
		if v == nil {
			return nil
		}
		found = true
		vec = decodeVector(v)
		return nil
	})
	return vec, found, err
}

// Keys returns all stored file paths in sorted order.
func (st *Store) Keys() ([]string, error) {
	var keys []string
	err := st.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(vectorsBucket))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, _ []byte) error {
			keys = append(keys, string(k))
			return nil
		})
	})
	sort.Strings(keys)
	return keys, err
}

// Count returns the number of stored vectors.
func (st *Store) Count() (int, error) {
	n := 0
	err := st.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(vectorsBucket))
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

// ReplaceAll atomically replaces the entire vector store with the given records.
func (st *Store) ReplaceAll(records map[string][]float64) error {
	return st.db.Update(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte(vectorsBucket)); b != nil {
			if err := tx.DeleteBucket([]byte(vectorsBucket)); err != nil {
				return err
			}
		}
		b, err := tx.CreateBucket([]byte(vectorsBucket))
		if err != nil {
			return err
		}
		for p, vec := range records {
			np := normalizePath(p)
			if np == "" {
				continue
			}
			if err := b.Put([]byte(np), encodeVector(vec)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Model returns the stored model name.
func (st *Store) Model() (string, error) { return st.meta(MetaModel) }

// Dimensions returns the stored dimensions as a string.
func (st *Store) Dimensions() (string, error) { return st.meta(MetaDimensions) }

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

func encodeVector(v []float64) []byte {
	buf := make([]byte, len(v)*8)
	for i, f := range v {
		binary.LittleEndian.PutUint64(buf[i*8:], math.Float64bits(f))
	}
	return buf
}

func decodeVector(b []byte) []float64 {
	n := len(b) / 8
	v := make([]float64, n)
	for i := range v {
		v[i] = math.Float64frombits(binary.LittleEndian.Uint64(b[i*8:]))
	}
	return v
}

func normalizePath(p string) string {
	p = filepath.ToSlash(p)
	p = strings.Trim(p, "/")
	if p == "" || p == "." {
		return ""
	}
	p = path.Clean(p)
	return strings.Trim(p, "/")
}
