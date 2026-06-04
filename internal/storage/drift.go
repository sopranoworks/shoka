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
	if wt, werr := workingTreeContentHashes(projectPath); werr == nil {
		for p := range wt {
			if has, herr := cat.HasFile(p); herr == nil && !has {
				sum.Added = append(sum.Added, p)
			}
		}
	}
	sort.Strings(sum.Added)
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
func workingTreeContentHashes(projectPath string) (map[string]string, error) {
	m := map[string]string{}
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
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to walk working tree: %w", err)
	}
	return m, nil
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
