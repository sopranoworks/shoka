package storage

import (
	"context"
	"os"
	"path/filepath"
	"strings"
)

// Empty-directory reclamation (B-48, Direction Y): Shoka holds no empty
// directories. A directory exists only as a by-product of a file's path, so when
// the last file leaves a directory that directory is reaped. Reaping is ALWAYS
// one level at a time, with rm semantics (remove iff empty), serialised against
// concurrent writers by the directory-scoped lock. The operation-time reap
// (deleteFile / Move) handles the directly-emptied parent on the spot; the
// lost+found sweep is the backstop for everything else. A chain collapses by
// repeated application of this same rule over later operations / sweep passes —
// there is no chain-ascent and no "reap the ancestor" trigger.

// reapableDir reports whether dir is a directory the reaper may remove. It must
// be strictly inside the project root (never the root itself, never escaping
// above it) and must not pass through a derivative/quarantine directory
// (.git/.shoka/.drafts — the same set the working-tree walks SkipDir). Everything
// else under the project tree is a plain data directory and is reapable.
func reapableDir(projectPath, dir string) bool {
	if dir == projectPath {
		return false // never reap the project root
	}
	rel, err := filepath.Rel(projectPath, dir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false // outside / above the project tree
	}
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if derivativeWalkSkipDir(seg) {
			return false // .git/.shoka/.drafts and anything beneath them
		}
	}
	return true
}

// reapEmptyDir removes dir if it is an empty directory — EXACTLY one level, under
// the directory-scoped lock, with rm semantics. It is the single reclamation
// primitive shared by the operation-time reap (delete/move) and the lost+found
// sweep backstop, so all reaping serialises identically against concurrent
// writers.
//
// rm semantics: os.Remove succeeds iff the directory is empty; a non-empty
// directory returns ENOTEMPTY (a legitimate no-op — the directory still holds
// content, e.g. a sibling a concurrent writer just created) and an
// already-removed directory returns ENOENT. Both are success here, as is any
// other os.Remove error: the file removal that triggered this has already
// succeeded, and a directory left behind is harmless (git tracks no empty dirs)
// and will be retried by the sweep backstop. The directory lock closes the
// writer's MkdirAll→CreateTemp window, so a concurrent write never sees the
// directory vanish mid-create.
//
// One level only: the caller passes the direct parent. If removing it makes ITS
// parent empty, that grandparent is reaped by the same rule on a later operation
// or sweep pass — never in this call (no filepath.Dir loop, no recursion).
func (s *FSGitStorage) reapEmptyDir(ctx context.Context, sessionID, projectPath, dir string) {
	if !reapableDir(projectPath, dir) {
		return
	}
	// A directory removal is a working-tree filesystem action only — like the
	// file os.Remove that preceded it, it writes no WAL entry and makes no commit
	// (git holds no empty directories, so there is nothing to commit; neither the
	// catalog nor the index ever references a bare directory — both are
	// files-only). The error is intentionally discarded: see the rm-semantics note.
	_ = s.locks.WithDirLock(ctx, sessionID, dir, func() error {
		_ = os.Remove(dir)
		return nil
	})
}
