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
func (s *FSGitStorage) DetectDrift(namespace, projectName string) (DriftSummary, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return DriftSummary{}, err
	}

	// Dangerous: .git is missing/unreadable.
	if _, oerr := git.PlainOpen(projectPath); oerr != nil {
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
	dirSeen := map[string]struct{}{}    // every visited non-root, non-derivative dir
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
