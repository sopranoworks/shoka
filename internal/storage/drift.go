package storage

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/go-git/go-git/v5"
)

// DriftSummary reports how a project differs from its catalog (the set of files
// Shoka manages).
type DriftSummary struct {
	State    ProjectState
	Added    []string // in the working tree, not in the catalog (external/noise; informational)
	Modified []string // in both, content differs from the catalog etag
	Deleted  []string // in the catalog, missing from the working tree (invariant violation)
	// EmptyDirs lists absolute working-tree directories with NO file descendant
	// anywhere beneath them — empty-directory reclaim candidates (B-48,
	// Direction Y). Free by-product of the same working-tree walk that computes
	// Added (the walk visits every directory; a dir with no file under it
	// contributes no Added entry but is recorded here). The lost+found sweep
	// reaps them one level per pass; informational for every other caller.
	EmptyDirs []string
}

// HasDrift reports whether any path differs.
func (d DriftSummary) HasDrift() bool {
	return len(d.Added)+len(d.Modified)+len(d.Deleted) > 0
}

// DetectDrift computes a project's state from the catalog invariant (directive
// §8): catalog → working tree (a catalog entry with no working-tree file is a
// "Deleted" violation), catalog ↔ working tree etag (a mismatch is "Modified"),
// and working tree → catalog (files on disk that the catalog does not know
// about are "Added" — external noise like .DS_Store/.claude/, reported for the
// operator but NOT a corrupting condition). Dangerous is reserved for a project
// whose .git cannot be opened. The catalog is disposable: if it cannot be
// opened it is rebuilt from git HEAD before verification.
//
// Catalog violations are reconciled against the LIVE on-disk git HEAD before they
// become a verdict (see the block below): a catalog left stale by an EXTERNAL HEAD
// move over a clean working tree is re-synced from HEAD and reads healthy, not
// corrupted (2026-06-16 stale-HEAD fix). Only a working tree that genuinely diverges
// from HEAD stays corrupted.
func (s *FSGitStorage) DetectDrift(namespace, projectName string) (DriftSummary, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return DriftSummary{}, err
	}

	// Dangerous: .git is missing/unreadable.
	r, oerr := git.PlainOpen(projectPath)
	if oerr != nil {
		s.setState(namespace, projectName, StateDangerous)
		return DriftSummary{State: StateDangerous}, nil
	}

	cat, err := s.catalogFor(namespace, projectName)
	if err != nil {
		// The catalog is disposable — rebuild it from git HEAD.
		if rerr := s.rebuildAndRegister(namespace, projectName); rerr != nil {
			s.setState(namespace, projectName, StateDangerous)
			return DriftSummary{State: StateDangerous}, nil
		}
		cat, err = s.catalogFor(namespace, projectName)
		if err != nil {
			s.setState(namespace, projectName, StateDangerous)
			return DriftSummary{State: StateDangerous}, nil
		}
	}

	violations, verr := cat.VerifyInvariant(projectPath)
	if verr != nil {
		s.setState(namespace, projectName, StateDangerous)
		return DriftSummary{State: StateDangerous}, nil
	}

	// Reconcile a divergent catalog against the ACTUAL on-disk git HEAD (re-read
	// live, every call). The catalog is the drift baseline and is maintained
	// SYNCHRONOUSLY on the write path (catalogPut/catalogDelete under the per-file
	// lock), so catalog↔working-tree is correct even during the async-commit window
	// where git HEAD still lags — which is why the hot path trusts it. But an
	// EXTERNAL HEAD move (a host `git reset`, the documented out-of-band "git add"
	// landing, a revert) changes the working tree WITHOUT updating the catalog,
	// leaving stale etags that VerifyInvariant reports as drift. That false positive
	// is the stale-HEAD defect: it stranded a clean project in `corrupted`
	// permanently — the `.db` persists, so every restart re-derived it, and the D1
	// lazy-rescan re-confirmed it against the same stale catalog. state.go documents
	// corrupted as "working tree drifted from HEAD"; this restores that intent.
	//
	// When the catalog reports drift but the working tree is CLEAN against the
	// current HEAD, the catalog is merely stale: rebuild it from HEAD (re-sync the
	// baseline) and the project is healthy. A working tree that GENUINELY diverges
	// from HEAD (an un-committed hand-edit/delete) is NOT clean, so it correctly
	// stays corrupted and is never silently rebuilt over. The git status is computed
	// ONLY on this violation path — never on the healthy hot path.
	if len(violations) > 0 {
		if clean, cerr := worktreeCleanVsHead(r); cerr == nil && clean {
			if rerr := s.rebuildAndRegister(namespace, projectName); rerr == nil {
				violations = nil
				if fresh, ferr := s.catalogFor(namespace, projectName); ferr == nil {
					cat = fresh
				}
			} else {
				s.log().Warn("drift: catalog re-sync to HEAD failed (staying with catalog verdict)",
					"namespace", namespace, "project", projectName, "err", rerr)
			}
		}
	}

	var sum DriftSummary
	for _, v := range violations {
		switch v.Kind {
		case "missing_from_working_tree":
			sum.Deleted = append(sum.Deleted, v.Path)
		case "etag_mismatch":
			sum.Modified = append(sum.Modified, v.Path)
		}
	}
	// Added: working-tree files the catalog does not record. Informational only;
	// they never change the state (the catalog deliberately filters this noise).
	// EmptyDirs (B-48) rides the same walk — empty-directory reclaim candidates.
	if wt, emptyDirs, werr := workingTreeContentHashes(projectPath); werr == nil {
		for p := range wt {
			if has, herr := cat.HasFile(p); herr == nil && !has {
				sum.Added = append(sum.Added, p)
			}
		}
		sum.EmptyDirs = emptyDirs
	}
	sort.Strings(sum.Added)
	sort.Strings(sum.EmptyDirs)
	sort.Strings(sum.Modified)
	sort.Strings(sum.Deleted)

	if len(sum.Modified)+len(sum.Deleted) > 0 {
		sum.State = StateCorrupted
	} else {
		sum.State = StateHealthy
	}
	s.setState(namespace, projectName, sum.State)
	return sum, nil
}

// worktreeCleanVsHead reports whether the working tree has NO uncommitted drift of
// tracked files against the current on-disk git HEAD — i.e. no tracked path whose
// git worktree status is Modified or Deleted. This is the live, authoritative test
// that distinguishes a merely-stale catalog (the tree matches a validly-moved HEAD)
// from genuine corruption (the tree diverges from HEAD), and it re-reads HEAD every
// call rather than trusting any cached baseline.
//
// Untracked files ('?': .DS_Store, .claude/, the derivative dirs, an out-of-band
// commit's brand-new file) are NOT drift — they are informational noise
// (DriftSummary.Added), exactly as before. The tracked Modified/Deleted set mirrors
// precisely what RepairTrackedChanges would adopt / RestoreToLatest would discard,
// so "not clean" ⟺ "there are tracked uncommitted changes a recovery would act on".
//
// A repository with no commits yet (unborn HEAD: CreateProject git-inits without an
// initial commit) has no committed baseline, so nothing can have drifted from it —
// reported clean. This also keeps go-git's Status off an unborn HEAD.
func worktreeCleanVsHead(r *git.Repository) (bool, error) {
	if _, herr := r.Head(); herr != nil {
		return true, nil
	}
	w, err := r.Worktree()
	if err != nil {
		return false, err
	}
	st, err := w.Status()
	if err != nil {
		return false, err
	}
	for _, fileStatus := range st {
		if fileStatus.Worktree == git.Modified || fileStatus.Worktree == git.Deleted {
			return false, nil
		}
	}
	return true, nil
}

// workingTreeContentHashes maps each working-tree path to the sha256 of its
// content, skipping .git/.shoka/.drafts and transient atomic-write temp files.
//
// It also returns the empty-directory reclaim candidates (B-48): every visited
// directory (other than the project root and the derivative dirs it SkipDirs)
// that has NO file descendant anywhere beneath it. This rides the same single
// walk — the addendum's finding that WalkDir already VISITS every directory even
// though the d.IsDir() branch omits dirs from the file map. A dir is a candidate
// iff no file marks it as an ancestor; this identifies whole empty chains, but
// the sweep reaps with rm semantics so non-leaf candidates simply no-op
// (ENOTEMPTY) until their subdirs are gone — one level per pass.
func workingTreeContentHashes(projectPath string) (map[string]string, []string, error) {
	m := map[string]string{}
	dirSeen := map[string]struct{}{}     // every visited non-root, non-derivative dir
	hasFileDesc := map[string]struct{}{} // dirs with >=1 file anywhere beneath
	err := filepath.WalkDir(projectPath, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != projectPath && derivativeWalkSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			if p != projectPath {
				dirSeen[p] = struct{}{}
			}
			return nil
		}
		if derivativeWalkSkipFile(d.Name()) {
			return nil // transient atomic-write staging file
		}
		rel, relErr := filepath.Rel(projectPath, p)
		if relErr != nil {
			return nil
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return nil
		}
		m[filepath.ToSlash(rel)] = sha256Hex(data)
		// Mark every ancestor directory (the file's parent up to, but excluding,
		// the project root) as holding a file descendant.
		for dir := filepath.Dir(p); dir != projectPath && len(dir) > len(projectPath); dir = filepath.Dir(dir) {
			hasFileDesc[dir] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("failed to walk working tree: %w", err)
	}
	var emptyDirs []string
	for dir := range dirSeen {
		if _, ok := hasFileDesc[dir]; !ok {
			emptyDirs = append(emptyDirs, dir)
		}
	}
	return m, emptyDirs, nil
}

// StartDriftScan starts a background goroutine that re-runs drift detection over
// every project every interval. The initial pass is the caller's responsibility
// (StartupInit runs it as the blocking startup gate); this only schedules the
// periodic re-scans. interval <= 0 disables it.
func (s *FSGitStorage) StartDriftScan(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.scanAllProjects()
			}
		}
	}()
}

// scanAllProjects waits for the WAL to drain, then runs DetectDrift on every
// project under the base directory.
func (s *FSGitStorage) scanAllProjects() {
	s.WaitForWAL(2 * time.Minute)
	projects, _ := s.discoverProjects() // leftovers are relocated post-startup, not re-scanned here
	for _, p := range projects {
		if _, err := s.DetectDrift(p.namespace, p.name); err != nil {
			s.log().Error("drift scan failed", "project", projectKey(p.namespace, p.name), "error", err)
		}
	}
}
