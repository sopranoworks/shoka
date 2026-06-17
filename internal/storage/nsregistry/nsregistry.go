// Package nsregistry is the durable, go-git-free store for Shoka's MANAGED namespace
// set (the B-28 ns/proj-management stage A). It records "what should be" — the
// namespaces Shoka manages and, within each, the managed project names — as a record
// that OUTLIVES the on-disk directories it describes. That outliving property is the
// whole point: detecting a MISSING managed namespace/project (stage B's health check)
// needs a record that survives the directory's disappearance, which an in-directory
// marker cannot provide.
//
// It is a sibling of the per-project catalog, the userstore, and the oauthstore, reusing
// the same embedded DB technology (bbolt) at a single global database
// <base_dir>/namespaces.db. Like those stores it must never go through the go-git storage
// layer (Architectural Anchor 1). Records are JSON-encoded, so a field added in a later
// stage decodes an older record with its zero value — the store evolves with NO migration.
//
// Bucket layout (bbolt buckets are flat/top-level):
//
//	"namespaces"  namespace name -> JSON Record  (the managed namespace + its project names)
//
// Move-readiness (operator decision 6): a project is keyed by its BARE NAME within its
// namespace's Record (Record.Projects), NOT by a global immutable "<ns>/<proj>" identity.
// A future MoveProject(oldNs, proj, newNs) therefore just removes the name from one
// Record and adds it to another — see the "move seam" note on MoveProject-shaped use in
// AddProject/RemoveProject. Project names are unique within a namespace (AddProject
// dedupes; HasProject is the exact check), so that future move can enforce the
// GitHub-repository-transfer rule: refuse when the target namespace already has a project
// of that name (no silent overwrite).
package nsregistry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	namespacesBucket = "namespaces"
	// journalBucket holds the single-entry move-journal (B-28 project move): an
	// in-progress MoveProject records its intent here so an interrupted move can be
	// AUTOMATICALLY resumed or rolled back at startup, with no operator action. Only one
	// move runs at a time (storage serialises via moveMu), so a single key suffices.
	journalBucket = "move_journal"
	journalKey    = "current"
)

// MoveJournal records an in-progress project move so it can be auto-recovered at startup.
// Phase is an opaque string the storage layer advances; nsregistry only persists it.
type MoveJournal struct {
	OldNamespace string `json:"old_namespace"`
	Project      string `json:"project"`
	NewNamespace string `json:"new_namespace"`
	Phase        string `json:"phase"`
}

// Record is one managed namespace: its name, when Shoka took it under management, and the
// set of managed project names within it (bare names, unique within the namespace).
type Record struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	Projects  []string  `json:"projects"`
}

// Registry is the global managed-namespace store handle. Safe for concurrent use; bbolt
// serialises writes and allows concurrent reads.
type Registry struct {
	db   *bolt.DB
	path string
}

// Open opens (creating if absent) the namespace registry at path and ensures its bucket
// exists. The parent directory is created as needed.
func Open(path string) (*Registry, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("nsregistry: create parent dir: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("nsregistry: open %s: %w", path, err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, b := range []string{namespacesBucket, journalBucket} {
			if _, e := tx.CreateBucketIfNotExists([]byte(b)); e != nil {
				return e
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("nsregistry: init buckets: %w", err)
	}
	return &Registry{db: db, path: path}, nil
}

// Close closes the underlying bbolt DB.
func (r *Registry) Close() error {
	if r == nil || r.db == nil {
		return nil
	}
	return r.db.Close()
}

// Path returns the store's filesystem path (useful for logging).
func (r *Registry) Path() string { return r.path }

// IsEmpty reports whether no namespace is managed yet — the "no managed info" condition
// that gates the one-time rescue-adopt migration.
func (r *Registry) IsEmpty() (bool, error) {
	empty := true
	err := r.db.View(func(tx *bolt.Tx) error {
		k, _ := tx.Bucket([]byte(namespacesBucket)).Cursor().First()
		empty = k == nil
		return nil
	})
	return empty, err
}

// List returns every managed namespace name, sorted ascending.
func (r *Registry) List() ([]string, error) {
	var out []string
	err := r.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(namespacesBucket)).ForEach(func(k, _ []byte) error {
			out = append(out, string(k))
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// Get returns the managed Record for ns. found is false (no error) when ns is not managed.
func (r *Registry) Get(ns string) (rec Record, found bool, err error) {
	err = r.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(namespacesBucket)).Get([]byte(ns))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &rec)
	})
	return rec, found, err
}

// EnsureNamespace registers ns as managed if it is not already (idempotent — an existing
// record, with its project list, is left untouched). CreatedAt is set on first registration.
func (r *Registry) EnsureNamespace(ns string) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(namespacesBucket))
		if b.Get([]byte(ns)) != nil {
			return nil
		}
		return putRecord(b, Record{Name: ns, CreatedAt: time.Now().UTC()})
	})
}

// RemoveNamespace deregisters ns (and its project list) from the managed set. Idempotent:
// deregistering an unmanaged namespace is a no-op.
func (r *Registry) RemoveNamespace(ns string) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(namespacesBucket)).Delete([]byte(ns))
	})
}

// AddProject adds proj to ns's managed project set, auto-registering ns if absent (the
// CreateProject safety-net path). Idempotent: a project already present is not duplicated.
//
// move seam: a future MoveProject(oldNs, proj, newNs) is RemoveProject(oldNs, proj) +
// AddProject(newNs, proj) — but it MUST first refuse when HasProject(newNs, proj) is true
// (the GitHub-repository-transfer no-silent-overwrite rule). The bare-name-within-namespace
// keying here is exactly what makes that target-collision check cheap and exact.
func (r *Registry) AddProject(ns, proj string) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(namespacesBucket))
		rec := Record{Name: ns, CreatedAt: time.Now().UTC()}
		if v := b.Get([]byte(ns)); v != nil {
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("nsregistry: decode record: %w", err)
			}
		}
		for _, p := range rec.Projects {
			if p == proj {
				return nil // already managed — idempotent
			}
		}
		rec.Projects = append(rec.Projects, proj)
		sort.Strings(rec.Projects)
		return putRecord(b, rec)
	})
}

// RemoveProject removes proj from ns's managed project set; the namespace record itself
// stays (a namespace survives the deletion of its last project). Idempotent.
//
// move seam: the remove half of a future MoveProject (see AddProject).
func (r *Registry) RemoveProject(ns, proj string) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(namespacesBucket))
		v := b.Get([]byte(ns))
		if v == nil {
			return nil
		}
		var rec Record
		if err := json.Unmarshal(v, &rec); err != nil {
			return fmt.Errorf("nsregistry: decode record: %w", err)
		}
		out := rec.Projects[:0:0]
		for _, p := range rec.Projects {
			if p != proj {
				out = append(out, p)
			}
		}
		rec.Projects = out
		return putRecord(b, rec)
	})
}

// HasProject reports whether proj is a managed project of ns — the exact,
// unique-within-namespace check a future MoveProject uses to enforce the
// no-overwrite-in-target rule.
func (r *Registry) HasProject(ns, proj string) (bool, error) {
	rec, found, err := r.Get(ns)
	if err != nil || !found {
		return false, err
	}
	for _, p := range rec.Projects {
		if p == proj {
			return true, nil
		}
	}
	return false, nil
}

func putRecord(b *bolt.Bucket, rec Record) error {
	val, err := json.Marshal(&rec)
	if err != nil {
		return fmt.Errorf("nsregistry: encode record: %w", err)
	}
	return b.Put([]byte(rec.Name), val)
}

func loadRecord(b *bolt.Bucket, ns string) (Record, bool, error) {
	v := b.Get([]byte(ns))
	if v == nil {
		return Record{}, false, nil
	}
	var rec Record
	if err := json.Unmarshal(v, &rec); err != nil {
		return Record{}, false, fmt.Errorf("nsregistry: decode record: %w", err)
	}
	return rec, true, nil
}

// MoveProject atomically re-keys proj from oldNs to newNs in the managed set (B-28 project
// move). In ONE bbolt transaction it: refuses if newNs already has a project of that name
// (the GitHub-repository-transfer no-overwrite rule); removes proj from oldNs's record; and
// adds it to newNs's record (creating newNs's record if absent — though callers require the
// target namespace to pre-exist and be managed). Idempotent-safe for recovery: if oldNs no
// longer has proj but newNs already does, it is a no-op success (a prior partial move already
// swapped the registry). The bare-name-within-namespace keying makes this a clean re-key.
func (r *Registry) MoveProject(oldNs, proj, newNs string) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(namespacesBucket))

		newRec, newFound, err := loadRecord(b, newNs)
		if err != nil {
			return err
		}
		newHas := false
		for _, p := range newRec.Projects {
			if p == proj {
				newHas = true
				break
			}
		}

		oldRec, _, err := loadRecord(b, oldNs)
		if err != nil {
			return err
		}
		oldHas := false
		kept := oldRec.Projects[:0:0]
		for _, p := range oldRec.Projects {
			if p == proj {
				oldHas = true
				continue
			}
			kept = append(kept, p)
		}

		// Idempotent recovery: registry already swapped (new has it, old doesn't).
		if newHas && !oldHas {
			return nil
		}
		// No-overwrite: target already has a DIFFERENT project of this name (and the source
		// still has it too) — a genuine collision, refuse.
		if newHas && oldHas {
			return fmt.Errorf("nsregistry: project %q already exists in namespace %q", proj, newNs)
		}

		// Remove from old (if present).
		if oldHas {
			oldRec.Projects = kept
			if err := putRecord(b, oldRec); err != nil {
				return err
			}
		}
		// Add to new (ensure the record exists).
		if !newFound {
			newRec = Record{Name: newNs, CreatedAt: time.Now().UTC()}
		}
		newRec.Projects = append(newRec.Projects, proj)
		sort.Strings(newRec.Projects)
		return putRecord(b, newRec)
	})
}

// SetMoveJournal records (or overwrites) the single in-progress move-journal entry.
func (r *Registry) SetMoveJournal(j MoveJournal) error {
	return r.db.Update(func(tx *bolt.Tx) error {
		val, err := json.Marshal(&j)
		if err != nil {
			return fmt.Errorf("nsregistry: encode move journal: %w", err)
		}
		return tx.Bucket([]byte(journalBucket)).Put([]byte(journalKey), val)
	})
}

// GetMoveJournal returns the in-progress move-journal entry; found is false when none.
func (r *Registry) GetMoveJournal() (j MoveJournal, found bool, err error) {
	err = r.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(journalBucket)).Get([]byte(journalKey))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &j)
	})
	return j, found, err
}

// ClearMoveJournal removes the move-journal entry (idempotent) — the move completed or was
// fully rolled back.
func (r *Registry) ClearMoveJournal() error {
	return r.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(journalBucket)).Delete([]byte(journalKey))
	})
}
