package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// ErrDeletionDiverged is the typed divergence error (the directive's policy 4): a
// revival cannot proceed because the recorded deletion commit is no longer in git
// (FIFO-cap eviction's content, or an external history rewrite like git
// filter-repo), or git simply has no such deletion for a name-specified path. It
// is surfaced clearly, never a silent failure — and a revival that hits it also
// triggers the bounded refresh (trigger b).
var ErrDeletionDiverged = errors.New("deleted file can no longer be restored: the deletion record is out of sync with git history")

// ReviveFile re-creates a deleted file forward-only: it reads the file's content
// at the PARENT of its deletion commit (the last-present content — the O(1)
// deletion-commit->parent->blob path) and writes it back as a NEW commit. History
// is preserved (never a reset). The deletion commit is resolved in order of
// preference:
//
//  1. an explicit fromCommit, if given;
//  2. the deleted-log entry's recorded DeletionCommit (the O(1) common path);
//  3. the name-specified last resort — a path-filtered history scan for the
//     deletion commit, when the path is not in the log (capped-out / diverged).
//
// If the resolved commit is gone from git, it returns ErrDeletionDiverged AND
// triggers the bounded refresh (trigger b). The re-create rides the normal write
// path, so the live hook drops the path from the deleted set when the commit lands.
func (s *FSGitStorage) ReviveFile(ctx context.Context, namespace, projectName, path, fromCommit string) error {
	if !s.deletedLogEnabled {
		return errors.New("deleted-log is disabled")
	}
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return err
	}
	_, rel, err := relWithin(projectPath, path)
	if err != nil {
		return err
	}
	r, err := git.PlainOpen(projectPath)
	if err != nil {
		return fmt.Errorf("failed to open git repository: %w", err)
	}

	deletionHash := fromCommit
	if deletionHash == "" {
		if st := s.deletedLogForRead(namespace, projectName); st != nil {
			if rec, found, gerr := st.Get(rel); gerr == nil && found {
				deletionHash = rec.DeletionCommit
			}
		}
	}
	if deletionHash == "" {
		// Name-specified last resort: find the deletion commit from path-filtered
		// history. Errors clearly (ErrDeletionDiverged) if git has no such deletion.
		h, ferr := findDeletionCommit(r, rel)
		if ferr != nil {
			return ferr
		}
		deletionHash = h
	}

	content, err := s.readContentAtDeletionParent(r, deletionHash, rel)
	if err != nil {
		if errors.Is(err, ErrDeletionDiverged) {
			// Trigger (b): the recorded hash is gone from git — refresh the log from a
			// bounded walk so the stale entry does not persist. Best-effort.
			_ = s.rebuildDeletedLog(namespace, projectName)
		}
		return err
	}

	// Forward-only re-create as a new commit (the existing overwrite-as-new-commit
	// write path). The commit-land hook drops rel from the deleted set when it lands.
	if _, werr := s.Write(ctx, "", namespace, projectName, rel, content, nil); werr != nil {
		return fmt.Errorf("revive write failed: %w", werr)
	}
	return nil
}

// readContentAtDeletionParent reads rel's content at the PARENT of the deletion
// commit — the last-present content — via the O(1) commit->parent->tree->blob
// path. A deletion commit gone from git, or with no parent / no such file at the
// parent, is the divergence signal (ErrDeletionDiverged).
func (s *FSGitStorage) readContentAtDeletionParent(r *git.Repository, deletionHash, rel string) (string, error) {
	cobj, err := r.CommitObject(plumbing.NewHash(deletionHash))
	if err != nil {
		return "", fmt.Errorf("%w: deletion commit %s not found: %v", ErrDeletionDiverged, deletionHash, err)
	}
	if cobj.NumParents() == 0 {
		return "", fmt.Errorf("%w: deletion commit %s has no parent", ErrDeletionDiverged, deletionHash)
	}
	parent, err := cobj.Parent(0)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDeletionDiverged, err)
	}
	ptree, err := parent.Tree()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDeletionDiverged, err)
	}
	f, err := ptree.File(rel)
	if err != nil {
		return "", fmt.Errorf("%w: %q not present at the deletion commit's parent: %v", ErrDeletionDiverged, rel, err)
	}
	content, err := f.Contents()
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDeletionDiverged, err)
	}
	return content, nil
}

// findDeletionCommit locates the most recent commit that deleted rel (rel absent
// in the commit but present in its first parent) via a path-filtered log. Returns
// ErrDeletionDiverged if git has no such deletion (e.g. the path never existed, or
// its history was rewritten away).
func findDeletionCommit(r *git.Repository, rel string) (string, error) {
	head, err := r.Head()
	if err != nil {
		return "", fmt.Errorf("%w: no history", ErrDeletionDiverged)
	}
	cIter, err := r.Log(&git.LogOptions{From: head.Hash(), Order: git.LogOrderCommitterTime, FileName: &rel})
	if err != nil {
		return "", fmt.Errorf("failed to read git log: %w", err)
	}
	defer cIter.Close()

	var found string
	walkErr := cIter.ForEach(func(c *object.Commit) error {
		cTree, terr := c.Tree()
		if terr != nil {
			return nil
		}
		if _, ferr := cTree.File(rel); ferr == nil {
			return nil // present in this commit → not a deletion
		}
		if c.NumParents() == 0 {
			return nil
		}
		parent, perr := c.Parent(0)
		if perr != nil {
			return nil
		}
		ptree, perr := parent.Tree()
		if perr != nil {
			return nil
		}
		if _, ferr := ptree.File(rel); ferr == nil {
			found = c.Hash.String() // present in parent, absent here → the deletion
			return storer.ErrStop
		}
		return nil
	})
	if walkErr != nil && walkErr != storer.ErrStop {
		return "", fmt.Errorf("failed to walk commits: %w", walkErr)
	}
	if found == "" {
		return "", fmt.Errorf("%w: no deletion of %q found in history", ErrDeletionDiverged, rel)
	}
	return found, nil
}
