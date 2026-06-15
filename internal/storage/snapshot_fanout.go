package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// snapshotTimeFormat stamps each snapshot file's <ts>. It matches the
// established lostFoundTimeFormat: UTC, filesystem-safe, and lexicographically
// sortable so "newest" is a plain reverse string sort (the name is authoritative
// for retention, not mtime).
const snapshotTimeFormat = "20060102T150405Z"

const snapshotSuffix = ".tar.gz"

// Scope selects which projects a snapshot/prune run covers. Namespace=="" &&
// Project=="" is the whole store; Namespace set, Project=="" is one namespace;
// both set is one project.
type Scope struct {
	Namespace string
	Project   string
}

// SnapshotResult is the per-project outcome of a SnapshotScope fan-out. Path is
// the written archive (empty on failure); Err is that project's error (nil on
// success). One project's failure never aborts the others.
type SnapshotResult struct {
	Namespace string
	Project   string
	Path      string
	Err       error
}

type nsProj struct {
	ns   string
	proj string
}

// resolveScope expands a Scope to the concrete project list via the
// classifier-backed listings (ListProjects/ListAllProjects), so non-projects
// (.shoka-lostfound, dotdirs, repo-less leftovers) are never included.
func (s *FSGitStorage) resolveScope(scope Scope) ([]nsProj, error) {
	switch {
	case scope.Project != "":
		return []nsProj{{scope.Namespace, scope.Project}}, nil
	case scope.Namespace != "":
		projects, err := s.ListProjects(scope.Namespace)
		if err != nil {
			return nil, err
		}
		out := make([]nsProj, 0, len(projects))
		for _, p := range projects {
			out = append(out, nsProj{scope.Namespace, p})
		}
		return out, nil
	default:
		all, err := s.ListAllProjects()
		if err != nil {
			return nil, err
		}
		out := make([]nsProj, 0, len(all))
		for _, np := range all {
			ns, proj, ok := strings.Cut(np, "/")
			if !ok {
				continue
			}
			out = append(out, nsProj{ns, proj})
		}
		return out, nil
	}
}

// SnapshotProjectToDir snapshots one project to <outputDir>/<ns>/<proj>/<ts>.tar.gz
// atomically: it streams SnapshotProject (the phase-1 lock-free archive) into a
// temp file in the SAME directory, then os.Rename into place — so a half-written
// archive is never presented. On any error the temp is removed and no final file
// is left. Returns the written path. (Mirrors atomicWriteFile's temp+rename
// discipline, streaming since an archive can be large.)
func (s *FSGitStorage) SnapshotProjectToDir(ctx context.Context, namespace, projectName, outputDir string) (string, error) {
	dir := filepath.Join(outputDir, namespace, projectName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create snapshot dir: %w", err)
	}

	ts := time.Now().UTC().Format(snapshotTimeFormat)
	finalPath := filepath.Join(dir, ts+snapshotSuffix)

	tmp, err := os.CreateTemp(dir, "."+ts+snapshotSuffix+".tmp-*")
	if err != nil {
		return "", fmt.Errorf("create temp snapshot: %w", err)
	}
	tmpName := tmp.Name()

	if err := s.SnapshotProject(ctx, namespace, projectName, tmp); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("close temp snapshot: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("chmod temp snapshot: %w", err)
	}
	if err := os.Rename(tmpName, finalPath); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("rename snapshot into place: %w", err)
	}
	return finalPath, nil
}

// SnapshotScope snapshots every project in scope to outputDir, resiliently: one
// project's failure is recorded in its SnapshotResult.Err and the run continues.
// It returns the per-project results plus an aggregate error (errors.Join of the
// per-project failures, nil if all succeeded). ctx cancellation is honoured
// between projects, returning the partial results and the ctx error.
func (s *FSGitStorage) SnapshotScope(ctx context.Context, scope Scope, outputDir string) ([]SnapshotResult, error) {
	projects, err := s.resolveScope(scope)
	if err != nil {
		return nil, fmt.Errorf("resolve scope: %w", err)
	}

	results := make([]SnapshotResult, 0, len(projects))
	var errs []error
	for _, np := range projects {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		path, perr := s.SnapshotProjectToDir(ctx, np.ns, np.proj, outputDir)
		results = append(results, SnapshotResult{Namespace: np.ns, Project: np.proj, Path: path, Err: perr})
		if perr != nil {
			errs = append(errs, fmt.Errorf("%s/%s: %w", np.ns, np.proj, perr))
		}
	}
	return results, errors.Join(errs...)
}

// PruneSnapshots deletes old snapshot files in scope, keeping the keepCount most
// recent per project (by the sortable <ts> name) and/or removing those older than
// maxAge (by the <ts>-encoded time). keepCount<=0 disables the count rule;
// maxAge<=0 disables the age rule; a file is removed if EITHER active rule selects
// it. It is surgical: only files whose name parses as <ts>.tar.gz under the
// snapshot layout are ever considered — any other file is left untouched. Returns
// the removed paths.
func (s *FSGitStorage) PruneSnapshots(outputDir string, scope Scope, keepCount int, maxAge time.Duration) ([]string, error) {
	if keepCount <= 0 && maxAge <= 0 {
		return nil, nil // nothing to do — both rules disabled
	}
	projects, err := s.resolveScope(scope)
	if err != nil {
		return nil, fmt.Errorf("resolve scope: %w", err)
	}

	now := time.Now()
	var ageCutoff time.Time
	if maxAge > 0 {
		ageCutoff = now.Add(-maxAge)
	}

	var removed []string
	for _, np := range projects {
		dir := filepath.Join(outputDir, np.ns, np.proj)
		ents, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue // no snapshots for this project yet
			}
			return removed, fmt.Errorf("read snapshot dir %s: %w", dir, err)
		}

		// Collect only valid snapshot files (name parses as <ts>.tar.gz). Anything
		// else — a stray user file, a leftover temp — is ignored, never deleted.
		type snap struct {
			name string
			ts   time.Time
		}
		var snaps []snap
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			base, ok := strings.CutSuffix(name, snapshotSuffix)
			if !ok {
				continue
			}
			t, perr := time.Parse(snapshotTimeFormat, base)
			if perr != nil {
				continue // not a snapshot file
			}
			snaps = append(snaps, snap{name: name, ts: t})
		}

		// Newest first by name (== by ts, since the format is sortable).
		sort.Slice(snaps, func(i, j int) bool { return snaps[i].name > snaps[j].name })

		for i, sn := range snaps {
			remove := false
			if keepCount > 0 && i >= keepCount {
				remove = true
			}
			if maxAge > 0 && sn.ts.Before(ageCutoff) {
				remove = true
			}
			if !remove {
				continue
			}
			p := filepath.Join(dir, sn.name)
			if err := os.Remove(p); err != nil {
				return removed, fmt.Errorf("remove snapshot %s: %w", p, err)
			}
			removed = append(removed, p)
		}
	}
	return removed, nil
}
