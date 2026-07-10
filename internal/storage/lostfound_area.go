package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The lost+found area (the 2026-06-02 lost+found worker directive).
//
// Untracked files the worker does NOT dispose (they do not match any
// shoka.disposable pattern) are MOVED here rather than deleted: their origin and
// intent are unknown, so they are preserved for the operator (or a future
// recovery UI) to recover. After a move the working tree returns to its
// tracked-only state.
//
// The area lives under the namespace root, OUTSIDE the project's git tree (the
// operator's standing constraint that Shoka management artefacts never
// contaminate project history):
//
//	<base>/<namespace>/.shoka-lostfound/<project>/<UTC-timestamp>/<original-rel-path>
//
// The leading dot makes it Shoka-internal; discoverProjects skips dot-prefixed
// project-level entries (see startup.go) so the area is never mistaken for a
// project. The per-project nesting keeps each project's lost+found structurally
// separable for future per-project access control.

// lostFoundTimeFormat is the filesystem-safe UTC timestamp used to group one
// sweep's moves (no colons, sortable).
const lostFoundTimeFormat = "20060102T150405Z"

// lostFoundRoot is the per-project lost+found base directory.
func (s *FSGitStorage) lostFoundRoot(namespace, projectName string) string {
	if namespace == "" {
		namespace = "default"
	}
	return filepath.Join(s.baseDir, namespace, ".shoka-lostfound", projectName)
}

// resolveLostFoundDest is the shared addressing core every deposit mode routes
// through (the D2 shared primitive). It resolves the deposit destination
// <root>/<ts>/<relPath> for one timestamp, creates its parent directory, and
// disambiguates on collision so an existing entry is never overwritten. Passing the
// same now for several deposits groups them under one <ts> directory.
//
// It touches no project repo — the lost+found area lives under the namespace root,
// OUTSIDE the project's git tree (lostFoundRoot) — so it succeeds even when the
// project's repo (or its whole directory) is absent. That is the load-bearing
// property the write-bytes deposit relies on (a WAL entry for a repo-less project).
func (s *FSGitStorage) resolveLostFoundDest(namespace, projectName, relPath string, now time.Time) (string, error) {
	tsDir := filepath.Join(s.lostFoundRoot(namespace, projectName), now.UTC().Format(lostFoundTimeFormat))
	dest := filepath.Join(tsDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("create lost+found dir: %w", err)
	}
	return disambiguate(dest), nil
}

// moveToLostFound moves the untracked file at relPath (slash-relative, within the
// project working tree) into the project's lost+found area, preserving its
// original path under a timestamp directory, and returns the destination path.
// Directories are created lazily. If the destination already exists (a prior move
// of the same path within the same timestamp), the name is disambiguated with a
// numeric suffix; an existing file is never overwritten. The move is a rename
// within the same namespace directory (same filesystem), so it is atomic. This is
// the move-file deposit mode (the existing B-26 sweep path); its behaviour is
// unchanged by the D2 factor-out — it now routes through the shared addressing core.
func (s *FSGitStorage) moveToLostFound(namespace, projectName, relPath string, now time.Time) (string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", err
	}
	src := filepath.Join(projectPath, filepath.FromSlash(relPath))

	dest, err := s.resolveLostFoundDest(namespace, projectName, relPath, now)
	if err != nil {
		return "", err
	}
	if err := os.Rename(src, dest); err != nil {
		return "", fmt.Errorf("move %q to lost+found: %w", relPath, err)
	}
	return dest, nil
}

// depositBytes writes in-memory content into the project's lost+found area at
// originalPath under a timestamp directory, and returns the deposited path. It is
// the write-bytes deposit mode (for D3: a WAL Entry.Content that can never commit).
// Unlike moveToLostFound it has no working-tree source and never calls
// getProjectPath, so it succeeds even when the project repo (or its whole
// directory) does not exist — the area is under the namespace root, outside the
// repo. The write is atomic (temp file in the destination dir + rename), so a crash
// never leaves a torn file. It writes no git ref (Anchor 3 N/A).
func (s *FSGitStorage) depositBytes(namespace, projectName, originalPath string, content []byte, now time.Time) (string, error) {
	dest, err := s.resolveLostFoundDest(namespace, projectName, originalPath, now)
	if err != nil {
		return "", err
	}
	if err := atomicWriteFile(dest, content); err != nil {
		return "", fmt.Errorf("deposit bytes to lost+found: %w", err)
	}
	return dest, nil
}

// depositTree relocates a whole directory (sourceDir) and any sibling files into the
// project's lost+found area, grouping them under one timestamp directory, and
// returns that directory. It is the move-tree deposit mode (for D4: a catalog-init
// leftover — a repo-less <project>/ tree plus its sibling <project>.<kind>.db files). Each move
// is an os.Rename within the same namespace root (same filesystem), so it is atomic;
// the source paths no longer exist afterwards. Each item lands at its basename under
// the one <ts> dir; collisions are disambiguated so nothing is ever overwritten.
func (s *FSGitStorage) depositTree(namespace, projectName, sourceDir string, now time.Time, siblingFiles ...string) (string, error) {
	tsDir := filepath.Join(s.lostFoundRoot(namespace, projectName), now.UTC().Format(lostFoundTimeFormat))

	treeDest, err := s.resolveLostFoundDest(namespace, projectName, filepath.Base(sourceDir), now)
	if err != nil {
		return "", err
	}
	if err := os.Rename(sourceDir, treeDest); err != nil {
		return "", fmt.Errorf("move tree %q to lost+found: %w", sourceDir, err)
	}
	for _, sib := range siblingFiles {
		sibDest, err := s.resolveLostFoundDest(namespace, projectName, filepath.Base(sib), now)
		if err != nil {
			return "", err
		}
		if err := os.Rename(sib, sibDest); err != nil {
			return "", fmt.Errorf("move sibling %q to lost+found: %w", sib, err)
		}
	}
	return tsDir, nil
}

// notifyQuarantined emits a lostfound.quarantined event for ns/projectName at path
// (the deposited or original path). It is the emit helper for the two non-sweep
// deposit modes: D3 calls it after depositBytes for a WAL entry that can never
// commit, D4 after depositTree for a leftover that was never a project. It reuses
// the fixed 5-field notify.Event via NotifyFrom (the "shoka-worker" sender) — no
// wire-shape change. D2 only defines this helper; the callers (D3/D4) invoke it.
func (s *FSGitStorage) notifyQuarantined(namespace, projectName, path string) {
	s.notify.NotifyFrom(lostFoundSender, kindLostFoundQuarantined, namespace+"/"+projectName, path)
}

// disambiguate returns path unchanged if nothing exists there, otherwise the
// first "path.N" (N=1,2,…) that does not exist. Never returns an existing path.
func disambiguate(path string) string {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return path
	}
	for i := 1; ; i++ {
		cand := fmt.Sprintf("%s.%d", path, i)
		if _, err := os.Lstat(cand); os.IsNotExist(err) {
			return cand
		}
	}
}
