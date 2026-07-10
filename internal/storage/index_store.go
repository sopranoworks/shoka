package storage

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/sopranoworks/shoka/internal/storage/index"
)

// The storage-side accessors for the per-project derivative index (the
// 2026-06-04 I1 directive). They mirror catalog_store.go exactly: a lazily-opened
// per-project handle registry, and best-effort indexPut/indexDelete hooks that
// ride the existing per-file lock at the catalog sites and NEVER fail the write.
// A failed index update just leaves the index stale; StartIndexSweep reconciles
// it from the working tree. The index update is strictly additive AFTER the
// authoritative catalog op (the catalog stays first and authoritative).
//
// This file (package storage) may read git for the marker — the go-git-free
// boundary is the internal/storage/index sub-package, not package storage.

// indexPath returns a project's index DB path:
// <base_dir>/<namespace>/<project>.index.db (the sibling of the catalog's
// <project>.project.db, mirroring catalogPath's default-namespace handling).
func (s *FSGitStorage) indexPath(namespace, projectName string) string {
	if namespace == "" {
		namespace = "default"
	}
	return filepath.Join(s.baseDir, namespace, projectName+".index.db")
}

// registerIndex records an already-open index handle, closing any prior handle
// for the key. Used by the repair sweep after a rebuild.
func (s *FSGitStorage) registerIndex(namespace, projectName string, ix *index.Index) {
	key := projectKey(namespace, projectName)
	s.idxMu.Lock()
	if old, ok := s.indexes[key]; ok && old != nil && old != ix {
		_ = old.Close()
	}
	s.indexes[key] = ix
	s.idxMu.Unlock()
}

// indexFor returns the open index for a project, opening it (or creating an empty
// one if the DB file does not yet exist) on demand. It is the mutating path's
// accessor, mirroring catalogFor. A corrupt/schema-mismatched DB is an error here
// (the repair sweep rebuilds); the write-path caller treats any error as
// best-effort and never fails the operation on it.
func (s *FSGitStorage) indexFor(namespace, projectName string) (*index.Index, error) {
	key := projectKey(namespace, projectName)
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	if ix, ok := s.indexes[key]; ok && ix != nil {
		return ix, nil
	}
	p := s.indexPath(namespace, projectName)
	ix, err := index.Open(p)
	if errors.Is(err, index.ErrNotFound) {
		ix, err = index.Create(p, namespace, projectName)
	}
	if err != nil {
		return nil, err
	}
	s.indexes[key] = ix
	return ix, nil
}

// indexForRead returns the open index for a project without ever creating one.
// Used by IndexHealthy so a health check never writes a .index.db. Returns nil if
// no index is registered and none can be opened.
func (s *FSGitStorage) indexForRead(namespace, projectName string) *index.Index {
	key := projectKey(namespace, projectName)
	s.idxMu.Lock()
	defer s.idxMu.Unlock()
	if ix, ok := s.indexes[key]; ok && ix != nil {
		return ix
	}
	ix, err := index.Open(s.indexPath(namespace, projectName))
	if err != nil {
		return nil
	}
	s.indexes[key] = ix
	return ix
}

// indexPut records a write/move-destination in the index. Best-effort: a failure
// is logged + counted but never fails the operation (the working tree, WAL, and
// catalog have already succeeded; the repair sweep reconciles). content is the
// new bytes already in scope at the call site — I2 derives the file's full-text
// bigram set from it here (in addition to the etag), with no change to the three
// call sites (the signature already carried content from I1, decision 5).
func (s *FSGitStorage) indexPut(namespace, projectName, rel string, content []byte, etag string) {
	ix, err := s.indexFor(namespace, projectName)
	if err != nil {
		s.log().Warn("index unavailable for write",
			"namespace", namespace, "project", projectName, "path", rel, "err", err)
		s.idxUpdateFailedWrite.Add(1)
		return
	}
	if perr := ix.PutRecord(rel, index.IndexRecord{
		Etag:          etag,
		Bigrams:       index.Bigrams(string(content)),
		OutboundLinks: derivedOutboundLinks(rel, content),
	}); perr != nil {
		s.log().Warn("index update failed for write",
			"namespace", namespace, "project", projectName, "path", rel, "err", perr)
		s.idxUpdateFailedWrite.Add(1)
	}
}

// indexDelete removes a path (delete / move-source) from the index. Best-effort,
// mirroring catalogDelete.
func (s *FSGitStorage) indexDelete(namespace, projectName, rel string) {
	ix, err := s.indexFor(namespace, projectName)
	if err != nil {
		s.log().Warn("index unavailable for delete",
			"namespace", namespace, "project", projectName, "path", rel, "err", err)
		s.idxUpdateFailedDelete.Add(1)
		return
	}
	if derr := ix.DeleteRecord(rel); derr != nil {
		s.log().Warn("index delete failed",
			"namespace", namespace, "project", projectName, "path", rel, "err", derr)
		s.idxUpdateFailedDelete.Add(1)
	}
}

// IndexHealthy reports whether a project's derivative index is openable AND
// current with HEAD — the cheap, non-blocking gate I2/I3 will use to choose the
// index fast path over the truth-scan fallback. It is O(1)-ish: a single meta read
// plus a HEAD ref read, never a full scan, and it never blocks on a repair (a
// missing/corrupt/stale/mid-repair index simply returns false, so the caller takes
// the slower-but-correct path).
//
// It errs safe (decision 2): the only inaccuracy is a false NEGATIVE during the
// window after a git commit advances HEAD until the next sweep re-advances the
// marker — the incremental index is in fact current then, but reporting "not
// healthy" only costs a fallback to the correct slow path. It is never
// false-healthy. NO caller uses this in I1; I2 wires the first use.
func (s *FSGitStorage) IndexHealthy(namespace, projectName string) bool {
	ix := s.indexForRead(namespace, projectName)
	if ix == nil {
		return false // missing or corrupt store → not healthy
	}
	marker, err := ix.LastIndexedCommit()
	if err != nil {
		return false
	}
	head, ok := s.headCommit(namespace, projectName)
	if !ok {
		return false // repo unopenable (dangerous)
	}
	return marker == head
}

// IndexCounters returns the index observability counters. updateFailedWrite /
// updateFailedDelete feed shoka_index_update_failed_total{operation}; rebuilds is
// the aggregate repair-sweep rebuild total (the sum of the reason-split counts —
// see IndexRebuildCounters for the per-reason values the metric exports).
func (s *FSGitStorage) IndexCounters() (updateFailedWrite, updateFailedDelete, rebuilds int64) {
	return s.idxUpdateFailedWrite.Load(),
		s.idxUpdateFailedDelete.Load(),
		s.idxRebuildsStale.Load() + s.idxRebuildsRecreated.Load()
}

// IndexRebuildCounters returns the repair-sweep rebuild counts split by reason,
// for shoka_index_rebuilds_total{reason}. "stale" is a marker-mismatch rebuild
// of a still-usable index handle; "recreated" is a rebuild after the handle was
// nil (the store was missing or corrupt and had to be discarded and recreated) —
// the index code does not distinguish missing from corrupt, so the two collapse
// to a single "recreated" reason here.
func (s *FSGitStorage) IndexRebuildCounters() (stale, recreated int64) {
	return s.idxRebuildsStale.Load(), s.idxRebuildsRecreated.Load()
}

// IndexSweepRuns returns the cumulative number of index reconcile passes the
// repair sweep has run, for shoka_index_sweep_runs_total. It is distinct from the
// rebuild counts: a pass that finds every project's index already current still
// counts as a run (the metric shows the worker is alive and how often it reconciles).
func (s *FSGitStorage) IndexSweepRuns() int64 { return s.idxSweepRuns.Load() }

// IndexHealthStates returns each tracked project ("namespace/project") mapped to
// whether its derivative index is currently healthy (open and marker == HEAD), for
// the per-project shoka_index_healthy gauge. Projects are enumerated from AllStates
// (the same source ProjectStates uses); health is read at scrape time via
// IndexHealthy — a bbolt meta read plus a HEAD ref read per project, never a full
// scan, never blocking on a repair. No state is stored.
func (s *FSGitStorage) IndexHealthStates() map[string]bool {
	states := s.AllStates()
	out := make(map[string]bool, len(states))
	for key := range states {
		ns, project := splitProjectKeyLocal(key)
		out[key] = s.IndexHealthy(ns, project)
	}
	return out
}

// removeIndexFile deletes a project's on-disk index DB and drops any registered
// handle. Used by the repair sweep to discard a corrupt store before recreating
// it. Best-effort; a missing file is not an error.
func (s *FSGitStorage) removeIndexFile(namespace, projectName string) {
	key := projectKey(namespace, projectName)
	s.idxMu.Lock()
	if old, ok := s.indexes[key]; ok && old != nil {
		_ = old.Close()
		delete(s.indexes, key)
	}
	s.idxMu.Unlock()
	if err := os.Remove(s.indexPath(namespace, projectName)); err != nil && !os.IsNotExist(err) {
		s.log().Warn("index file remove failed",
			"namespace", namespace, "project", projectName, "err", err)
	}
}
