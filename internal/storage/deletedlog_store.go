package storage

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/sopranoworks/shoka/internal/storage/deletedlog"
	"github.com/sopranoworks/shoka/internal/storage/wal"
)

// The storage-side accessors for the per-project deleted-file log (the 2026-06-18
// deleted-log directive). They mirror index_store.go: a lazily-opened per-project
// handle registry, and a best-effort commit-land hook that updates the live
// currently-deleted set and NEVER fails the commit. A failed update just leaves
// the log stale; the bounded two-trigger repair reconciles it.
//
// The deleted-log differs from the index in WHERE it is updated: the index rides
// the per-file lock (it needs only the etag), but the deleted-log needs the
// DELETION COMMIT HASH, which exists only AFTER the WAL worker commits — so the
// hook fires in commitEntry, after advanceHead, keyed off the commit's Op.

// deletedLogPath returns a project's deleted-log DB path:
// <base_dir>/<namespace>/<project>.deleted.db (the sibling of the catalog's
// <project>.db and the index's <project>.index.db, mirroring the default-namespace
// handling).
func (s *FSGitStorage) deletedLogPath(namespace, projectName string) string {
	if namespace == "" {
		namespace = "default"
	}
	return filepath.Join(s.baseDir, namespace, projectName+".deleted.db")
}

// registerDeletedLog records an already-open handle, closing any prior handle for
// the key. Used by the repair walk after a rebuild.
func (s *FSGitStorage) registerDeletedLog(namespace, projectName string, st *deletedlog.Store) {
	key := projectKey(namespace, projectName)
	s.dlMu.Lock()
	if old, ok := s.deletedLogs[key]; ok && old != nil && old != st {
		_ = old.Close()
	}
	s.deletedLogs[key] = st
	s.dlMu.Unlock()
}

// deletedLogFor returns the open store for a project, opening it (or creating an
// empty one if the DB file does not yet exist) on demand. It is the mutating
// (hook) path's accessor, mirroring indexFor. The write-path caller treats any
// error as best-effort and never fails the operation on it.
func (s *FSGitStorage) deletedLogFor(namespace, projectName string) (*deletedlog.Store, error) {
	key := projectKey(namespace, projectName)
	s.dlMu.Lock()
	defer s.dlMu.Unlock()
	if st, ok := s.deletedLogs[key]; ok && st != nil {
		return st, nil
	}
	p := s.deletedLogPath(namespace, projectName)
	st, err := deletedlog.Open(p)
	if errors.Is(err, deletedlog.ErrNotFound) {
		st, err = deletedlog.Create(p, namespace, projectName)
	}
	if err != nil {
		return nil, err
	}
	s.deletedLogs[key] = st
	return st, nil
}

// deletedLogForRead returns the open store for a project WITHOUT ever creating
// one. Returns nil if no store is registered and none can be opened (absent or
// corrupt) — the signal the list/repair path uses to decide a trigger-(a) rebuild.
func (s *FSGitStorage) deletedLogForRead(namespace, projectName string) *deletedlog.Store {
	key := projectKey(namespace, projectName)
	s.dlMu.Lock()
	defer s.dlMu.Unlock()
	if st, ok := s.deletedLogs[key]; ok && st != nil {
		return st
	}
	st, err := deletedlog.Open(s.deletedLogPath(namespace, projectName))
	if err != nil {
		return nil
	}
	s.deletedLogs[key] = st
	return st
}

// deletedLogHasOriginMarker reports whether an EXISTING <p>.deleted.db carries the origin
// marker — a no-create, O(1) single key lookup (no record scan). Returns (false, nil) when no
// log exists or it cannot be opened (an absent/unreadable file has no verifiable marker).
func (s *FSGitStorage) deletedLogHasOriginMarker(namespace, projectName string) (bool, error) {
	st := s.deletedLogForRead(namespace, projectName)
	if st == nil {
		return false, nil
	}
	return st.HasOriginMarker()
}

// deletedLogRecordsEmpty reports whether an EXISTING <p>.deleted.db holds zero deletion
// records — a no-create, O(1) first-key probe. Returns (true, nil) when no log exists.
func (s *FSGitStorage) deletedLogRecordsEmpty(namespace, projectName string) (bool, error) {
	st := s.deletedLogForRead(namespace, projectName)
	if st == nil {
		return true, nil
	}
	return st.IsEmpty()
}

// deletedLogExists reports whether a project's deleted-log DB file is present on
// disk. It is the trigger-(a) discriminator: an absent file means "never built"
// (rebuild it); a present file is trusted as-is, even if empty (no proactive scan).
func (s *FSGitStorage) deletedLogExists(namespace, projectName string) bool {
	if _, err := os.Stat(s.deletedLogPath(namespace, projectName)); err == nil {
		return true
	}
	return false
}

// removeDeletedLogFile deletes a project's on-disk store and drops any registered
// handle. Used by the repair path to discard a corrupt store before recreating it.
// Best-effort; a missing file is not an error.
func (s *FSGitStorage) removeDeletedLogFile(namespace, projectName string) {
	key := projectKey(namespace, projectName)
	s.dlMu.Lock()
	if old, ok := s.deletedLogs[key]; ok && old != nil {
		_ = old.Close()
		delete(s.deletedLogs, key)
	}
	s.dlMu.Unlock()
	if err := os.Remove(s.deletedLogPath(namespace, projectName)); err != nil && !os.IsNotExist(err) {
		s.log().Warn("deleted-log file remove failed",
			"namespace", namespace, "project", projectName, "err", err)
	}
}

// deletedLogApply is the best-effort commit-land hook. It runs in commitEntry
// AFTER advanceHead succeeds, with the deletion commit hash now known, and applies
// the net-state edge for the commit's Op against the live currently-deleted set:
//
//   - delete: UPSERT {path, deletionCommit, ts} — the path is now deleted.
//   - move:   DROP the source (relocated, not deleted) AND DROP the destination
//     (now present, in case it was previously deleted).
//   - write:  DROP the path — a (re)create makes it present, netting out an earlier
//     delete (the delete-then-revive case; revival rides the write path).
//
// These are exactly the live edges that make the hook and the bounded repair walk
// agree on the same net state. A failure logs + counts (dlUpdateFailed) but NEVER
// fails the commit — the bounded repair is the safety net.
//
// Lazy-create discipline (2026-06-18): the store is created ONLY on a real deletion.
// op:"delete" is the sole record-ADDING op, so it alone uses the create-capable
// deletedLogFor. For write/move, dropping a path from the currently-deleted set is
// meaningful only if a log already exists (a path can be deleted only after a prior
// delete created the log), so they use the no-create deletedLogForRead and SKIP when it
// is absent — a project that never deletes anything never gets an empty <p>.deleted.db
// (the over-broad create-on-write that produced needless empty logs is removed).
func (s *FSGitStorage) deletedLogApply(e wal.Entry, commitHash string, ts time.Time) {
	if !s.deletedLogEnabled {
		return
	}
	var st *deletedlog.Store
	if e.Op == "delete" {
		var err error
		st, err = s.deletedLogFor(e.Namespace, e.Project) // create-capable: a delete records
		if err != nil {
			s.log().Warn("deleted-log unavailable at commit-land",
				"namespace", e.Namespace, "project", e.Project, "op", e.Op, "err", err)
			s.dlUpdateFailed.Add(1)
			return
		}
	} else {
		st = s.deletedLogForRead(e.Namespace, e.Project) // no-create: write/move only update
		if st == nil {
			return // no existing log ⇒ nothing to drop; do NOT create one
		}
	}
	var applyErr error
	switch e.Op {
	case "delete":
		applyErr = st.Upsert(deletedlog.DeletedRecord{
			Path:           e.Path,
			DeletionCommit: commitHash,
			DeletedAt:      ts,
		}, s.deletedLogMaxEntries)
	case "move":
		if derr := st.Drop(e.MoveFrom); derr != nil {
			applyErr = derr
		}
		if derr := st.Drop(e.Path); derr != nil && applyErr == nil {
			applyErr = derr
		}
	default: // "write"
		applyErr = st.Drop(e.Path)
	}
	if applyErr != nil {
		s.log().Warn("deleted-log update failed at commit-land",
			"namespace", e.Namespace, "project", e.Project, "op", e.Op, "path", e.Path, "err", applyErr)
		s.dlUpdateFailed.Add(1)
	}
}

// DeletedFile is one currently-deleted path surfaced to callers (the MCP/ws ops).
// It is the storage-level DTO for a deletedlog.DeletedRecord, so the bbolt store
// type does not leak through the StorageService interface.
type DeletedFile struct {
	Path           string    `json:"path"`
	DeletionCommit string    `json:"deletion_commit"`
	DeletedAt      time.Time `json:"deleted_at"`
}

// ListDeleted returns a project's currently-deleted files — the cheap everyday
// read: a single O(cap) bucket scan, no git walk, no on-read validation against
// git (divergence is discovered only at revival). Trigger (a): if the store file is
// absent (never built), it is first rebuilt by the bounded recent-commit repair
// walk; a present store is trusted as-is.
func (s *FSGitStorage) ListDeleted(namespace, projectName string) ([]DeletedFile, error) {
	if !s.deletedLogEnabled {
		return []DeletedFile{}, nil
	}
	if _, err := s.getProjectPath(namespace, projectName); err != nil {
		return nil, err
	}
	if !s.deletedLogExists(namespace, projectName) {
		// Trigger (a): log absent → bounded recent-commit rebuild.
		if rerr := s.rebuildDeletedLog(namespace, projectName); rerr != nil {
			return nil, rerr
		}
	}
	st := s.deletedLogForRead(namespace, projectName)
	if st == nil {
		// Corrupt despite existing: discard and rebuild once.
		s.removeDeletedLogFile(namespace, projectName)
		if rerr := s.rebuildDeletedLog(namespace, projectName); rerr != nil {
			return nil, rerr
		}
		st = s.deletedLogForRead(namespace, projectName)
		if st == nil {
			return nil, errors.New("deleted-log unavailable after rebuild")
		}
	}
	recs, err := st.List()
	if err != nil {
		return nil, err
	}
	out := make([]DeletedFile, 0, len(recs))
	for _, r := range recs {
		out = append(out, DeletedFile{Path: r.Path, DeletionCommit: r.DeletionCommit, DeletedAt: r.DeletedAt})
	}
	return out, nil
}

// DeletedLogUpdateFailures returns the best-effort hook failure count (for tests +
// observability), the deleted-log analogue of IndexCounters' update failures.
func (s *FSGitStorage) DeletedLogUpdateFailures() int64 { return s.dlUpdateFailed.Load() }
