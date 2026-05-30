package storage

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// DriftSummary reports how a project's working tree differs from its git HEAD.
type DriftSummary struct {
	State    ProjectState
	Added    []string // in working tree, not in HEAD (untracked)
	Modified []string // in both, content differs
	Deleted  []string // in HEAD, missing from working tree
}

// HasDrift reports whether any path differs.
func (d DriftSummary) HasDrift() bool {
	return len(d.Added)+len(d.Modified)+len(d.Deleted) > 0
}

// DetectDrift compares a project's working tree against its git HEAD by content
// hash and updates the project's state. Per the directive §7.4, but with one
// refinement: a repository that opens cleanly yet has no commits (a freshly
// initialised project) is healthy when its working tree is empty and corrupted
// when it is not — rather than "dangerous". Dangerous is reserved for a .git
// that cannot be opened. (Flagged in the completion report.)
func (s *FSGitStorage) DetectDrift(namespace, projectName string) (DriftSummary, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return DriftSummary{}, err
	}

	r, err := git.PlainOpen(projectPath)
	if err != nil {
		s.setState(namespace, projectName, StateDangerous)
		return DriftSummary{State: StateDangerous}, nil
	}

	headHashes, hasHead, err := headContentHashes(r)
	if err != nil {
		s.setState(namespace, projectName, StateDangerous)
		return DriftSummary{State: StateDangerous}, nil
	}

	wtHashes, err := workingTreeContentHashes(projectPath)
	if err != nil {
		return DriftSummary{}, err
	}

	var sum DriftSummary
	for p := range headHashes {
		if _, ok := wtHashes[p]; !ok {
			sum.Deleted = append(sum.Deleted, p)
		}
	}
	for p, h := range wtHashes {
		hh, ok := headHashes[p]
		if !ok {
			sum.Added = append(sum.Added, p)
			continue
		}
		if hh != h {
			sum.Modified = append(sum.Modified, p)
		}
	}
	sort.Strings(sum.Added)
	sort.Strings(sum.Modified)
	sort.Strings(sum.Deleted)

	switch {
	case !hasHead:
		if len(wtHashes) == 0 {
			sum.State = StateHealthy
		} else {
			sum.State = StateCorrupted
		}
	case sum.HasDrift():
		sum.State = StateCorrupted
	default:
		sum.State = StateHealthy
	}

	s.setState(namespace, projectName, sum.State)
	return sum, nil
}

// headContentHashes maps each path at HEAD to the sha256 of its content. The
// second return is false when the repo has no commits yet (not an error).
func headContentHashes(r *git.Repository) (map[string]string, bool, error) {
	ref, err := r.Head()
	if err != nil {
		return map[string]string{}, false, nil // no commits yet
	}
	commit, err := r.CommitObject(ref.Hash())
	if err != nil {
		return nil, false, err
	}
	tree, err := commit.Tree()
	if err != nil {
		return nil, false, err
	}
	m := map[string]string{}
	err = tree.Files().ForEach(func(f *object.File) error {
		content, cerr := f.Contents()
		if cerr != nil {
			return nil
		}
		m[f.Name] = sha256Hex([]byte(content))
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return m, true, nil
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
			if p != projectPath {
				switch d.Name() {
				case ".git", ".shoka", ".drafts":
					return filepath.SkipDir
				}
			}
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".tmp-write-") {
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

// StartDriftScan runs drift detection over every project on a background
// goroutine: once at startup (after the WAL drains) and then every interval if
// interval > 0. It never blocks the caller.
func (s *FSGitStorage) StartDriftScan(ctx context.Context, interval time.Duration) {
	go func() {
		s.scanAllProjects()
		if interval <= 0 {
			return
		}
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
// <namespace>/<project> under the base directory.
func (s *FSGitStorage) scanAllProjects() {
	s.WaitForWAL(2 * time.Minute)
	nsEntries, err := os.ReadDir(s.baseDir)
	if err != nil {
		s.log().Error("drift scan: cannot read base dir", "error", err)
		return
	}
	for _, ns := range nsEntries {
		if !ns.IsDir() || ns.Name() == ".shoka" {
			continue
		}
		projEntries, err := os.ReadDir(filepath.Join(s.baseDir, ns.Name()))
		if err != nil {
			continue
		}
		for _, pr := range projEntries {
			if !pr.IsDir() {
				continue
			}
			if _, err := s.DetectDrift(ns.Name(), pr.Name()); err != nil {
				s.log().Error("drift scan failed", "project", projectKey(ns.Name(), pr.Name()), "error", err)
			}
		}
	}
}
