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

// moveToLostFound moves the untracked file at relPath (slash-relative, within the
// project working tree) into the project's lost+found area, preserving its
// original path under a timestamp directory, and returns the destination path.
// Directories are created lazily. If the destination already exists (a prior move
// of the same path within the same timestamp), the name is disambiguated with a
// numeric suffix; an existing file is never overwritten. The move is a rename
// within the same namespace directory (same filesystem), so it is atomic.
func (s *FSGitStorage) moveToLostFound(namespace, projectName, relPath string, now time.Time) (string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", err
	}
	src := filepath.Join(projectPath, filepath.FromSlash(relPath))

	tsDir := filepath.Join(s.lostFoundRoot(namespace, projectName), now.UTC().Format(lostFoundTimeFormat))
	dest := filepath.Join(tsDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("create lost+found dir: %w", err)
	}
	dest = disambiguate(dest)

	if err := os.Rename(src, dest); err != nil {
		return "", fmt.Errorf("move %q to lost+found: %w", relPath, err)
	}
	return dest, nil
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
