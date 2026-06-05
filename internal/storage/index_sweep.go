package storage

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/shoka/mcp-server/internal/storage/index"
)

// The index repair worker (the 2026-06-04 I1 directive): the fourth periodic
// single-goroutine sweep of the B-26 pattern, beside StartDriftScan and
// StartLostFoundSweep. It keeps each project's derivative index reconciled with
// the truth in the background, NEVER on a query's path.
//
// Detection is last_indexed_commit vs HEAD (decision 2): the index leads git HEAD
// on the write path (the incremental update fires before the async WAL→git
// commit), so the marker is advanced only here — after WaitForWAL has let HEAD
// catch up — to the HEAD the rebuilt index reflects. A missing, corrupt, or
// schema-mismatched store is treated as stale and rebuilt wholesale from
// working-tree bytes (decision 4). The repair runs off the request path; a query
// mid-repair (I2/I3) just sees IndexHealthy=false and uses the fallback.
//
// The HEAD read here uses go-git, which is fine in package storage. The
// internal/storage/index sub-package stays working-tree-bytes-only (go-git-free).

// StartIndexSweep runs an initial reconcile pass, then re-reconciles every project
// every interval, in a single goroutine (so passes never overlap). interval <= 0
// disables the worker. The goroutine stops when ctx is cancelled. Mirrors
// StartLostFoundSweep; the startup wiring runs it after StartupInit so catalogs
// and states are ready.
func (s *FSGitStorage) StartIndexSweep(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		s.reconcileAllIndexes()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.reconcileAllIndexes()
			case k := <-s.fixLinksKicks:
				// Post-move fix_links (I3): drained immediately by this same
				// goroutine (no new subsystem), delayed only while a reconcile pass
				// above is mid-flight. fixLinks finds referrers via the index when
				// healthy, else truth-scans; it never rewrites from a broken index.
				s.fixLinks(ctx, k.namespace, k.project, k.src, k.dst)
			}
		}
	}()
}

// reconcileAllIndexes waits for the WAL to drain (so HEAD has caught up to the
// working tree before the marker is set), then reconciles every project.
func (s *FSGitStorage) reconcileAllIndexes() {
	s.WaitForWAL(2 * time.Minute)
	projects, _ := s.discoverProjects() // leftovers are relocated post-startup, not swept here
	for _, p := range projects {
		s.reconcileIndex(p.namespace, p.name)
	}
}

// reconcileIndex brings one project's index up to date with HEAD. It is the unit a
// future user-triggered "reindex this project" action would call. Cheap when the
// marker already equals HEAD (the common case); otherwise a wholesale rebuild from
// working-tree bytes.
func (s *FSGitStorage) reconcileIndex(namespace, projectName string) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return
	}
	head, ok := s.headCommit(namespace, projectName)
	if !ok {
		return // repo unopenable (dangerous): leave the index untouched
	}

	// Reuse the registry's single handle (a second bbolt open of the same file in
	// this process would deadlock on the file lock). nil means no usable handle —
	// the store is missing or corrupt and must be recreated.
	if ix := s.indexForRead(namespace, projectName); ix != nil {
		marker, merr := ix.LastIndexedCommit()
		if merr == nil && marker == head {
			return // up to date — the cheap no-op fast check
		}
		// A usable handle whose marker lags HEAD: a stale rebuild.
		s.rebuildIndexInto(namespace, projectName, projectPath, head, ix, rebuildStale)
		return
	}

	// Missing or corrupt: discard whatever is on disk, recreate empty, rebuild.
	// indexForRead returns nil for both missing and corrupt, so these collapse to
	// the single "recreated" reason — the index code cannot tell them apart today.
	s.removeIndexFile(namespace, projectName)
	ix, cerr := index.Create(s.indexPath(namespace, projectName), namespace, projectName)
	if cerr != nil {
		s.log().Error("index reconcile: create failed",
			"project", projectKey(namespace, projectName), "error", cerr)
		return
	}
	s.registerIndex(namespace, projectName, ix)
	s.rebuildIndexInto(namespace, projectName, projectPath, head, ix, rebuildRecreated)
}

// indexRebuildReason labels a repair-sweep rebuild for the
// shoka_index_rebuilds_total{reason} metric. It reflects the branch the rebuild
// was reached through, which is the only reason distinction the index code makes.
type indexRebuildReason int

const (
	rebuildStale     indexRebuildReason = iota // usable handle, marker lagged HEAD
	rebuildRecreated                           // nil handle (missing or corrupt), store recreated
)

// rebuildIndexInto rebuilds the forward map wholesale from working-tree bytes and
// advances the marker to head, atomically (one bbolt transaction via ReplaceAll).
// workingTreeIndexRecords walks the same managed-file corpus as the catalog/drift
// (the shared derivativeWalkSkip* predicates) and SearchFiles, so the rebuilt index
// reflects exactly the set the fast path's fallback would scan.
func (s *FSGitStorage) rebuildIndexInto(namespace, projectName, projectPath, head string, ix *index.Index, reason indexRebuildReason) {
	records, werr := workingTreeIndexRecords(projectPath)
	if werr != nil {
		s.log().Error("index reconcile: walk working tree failed",
			"project", projectKey(namespace, projectName), "error", werr)
		return
	}
	if rerr := ix.ReplaceAll(records, head); rerr != nil {
		s.log().Error("index reconcile: rebuild failed",
			"project", projectKey(namespace, projectName), "error", rerr)
		return
	}
	switch reason {
	case rebuildRecreated:
		s.idxRebuildsRecreated.Add(1)
	default:
		s.idxRebuildsStale.Add(1)
	}
}

// workingTreeIndexRecords walks a project's working tree and derives the full
// index record for every managed file: the content etag (sha256) and the
// full-text bigram set (I2). It reads one file at a time (memory is bounded by the
// accumulating record map, not the whole corpus held at once) and shares the
// derivativeWalkSkip* predicates with SearchFiles and workingTreeContentHashes so
// the index corpus is identical to the fallback's scan corpus by construction.
// Unlike drift's workingTreeContentHashes (which keeps only the hash), this keeps
// the bytes long enough to derive bigrams, then drops them.
func workingTreeIndexRecords(projectPath string) (map[string]index.IndexRecord, error) {
	records := map[string]index.IndexRecord{}
	err := filepath.WalkDir(projectPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != projectPath && derivativeWalkSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if derivativeWalkSkipFile(d.Name()) {
			return nil
		}
		rel, relErr := filepath.Rel(projectPath, p)
		if relErr != nil {
			return nil
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		records[relSlash] = index.IndexRecord{
			Etag:          sha256Hex(data),
			Bigrams:       index.Bigrams(string(data)),
			OutboundLinks: derivedOutboundLinks(relSlash, data),
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("index: walk working tree: %w", err)
	}
	return records, nil
}

// headCommit returns the project's HEAD commit hash and whether the repo could be
// opened. An openable repo with no commits yet returns ("", true). An unopenable
// repo (no .git / dangerous) returns ("", false), signalling the caller to skip.
func (s *FSGitStorage) headCommit(namespace, projectName string) (string, bool) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", false
	}
	r, oerr := git.PlainOpen(projectPath)
	if oerr != nil {
		return "", false
	}
	ref, herr := r.Head()
	if herr != nil {
		if herr == plumbing.ErrReferenceNotFound {
			return "", true // openable, no commits yet
		}
		return "", false
	}
	return ref.Hash().String(), true
}
