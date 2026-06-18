package storage

import (
	"fmt"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/utils/merkletrie"
	"github.com/sopranoworks/shoka/internal/storage/deletedlog"
	"github.com/sopranoworks/shoka/internal/storage/opmeta"
)

// rebuildDeletedLog reconstructs a project's deleted-file log from a BOUNDED
// recent-commit walk and replaces the store wholesale. It is the action behind the
// two repair triggers (never a sweep, never on-read scanning):
//   - trigger (a): the store file is absent → build it (ListDeleted calls this).
//   - trigger (b): a revival found its recorded deletion commit gone from git →
//     refresh (ReviveFile calls this after returning the divergence error).
//
// The walk reproduces the SAME net deleted set the live commit-land hook maintains
// (see walkDeletedNetState), so the hook and the walk agree by construction. It
// never exceeds deletedLogRepairDepth commits — the unbounded full walk the cost
// report killed is not used here.
func (s *FSGitStorage) rebuildDeletedLog(namespace, projectName string) error {
	recs, err := s.walkDeletedNetState(namespace, projectName)
	if err != nil {
		return err
	}
	s.removeDeletedLogFile(namespace, projectName)
	p := s.deletedLogPath(namespace, projectName)
	st, err := deletedlog.Create(p, namespace, projectName)
	if err != nil {
		// A concurrent rebuild may have created it; adopt the existing store.
		st, err = deletedlog.Open(p)
		if err != nil {
			return fmt.Errorf("deleted-log rebuild create/open: %w", err)
		}
	}
	if rerr := st.ReplaceAll(recs, s.deletedLogMaxEntries); rerr != nil {
		_ = st.Close()
		return fmt.Errorf("deleted-log rebuild replace: %w", rerr)
	}
	s.registerDeletedLog(namespace, projectName, st)
	return nil
}

// walkDeletedNetState computes the currently-deleted set by a bounded, newest->
// oldest walk of at most deletedLogRepairDepth commits, folding each path to its
// NEWEST operation (first-seen wins). For each commit it derives the real tree
// change (added/removed/modified vs parent — the same merkletrie diff
// ListFilesSince uses) and classifies via the commit's Shoka-Op trailer when it is
// present AND consistent with that diff (the meaning gate); otherwise it falls back
// to the conservative raw diff (a removal is a deletion; NO git rename/similarity
// inference). The net result:
//   - a verified move makes the SOURCE not-deleted (relocated) and the destination
//     present — fixing the double-count and move-source misclassification;
//   - a (re)create/revive (write/add) nets out an earlier delete (delete-then-
//     revive → not deleted), because the newer present-op is seen first;
//   - an unreversed delete remains deleted, recorded with that commit as the
//     deletion commit (so revival is the O(1) parent-blob path).
func (s *FSGitStorage) walkDeletedNetState(namespace, projectName string) ([]deletedlog.DeletedRecord, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return nil, err
	}
	r, err := git.PlainOpen(projectPath)
	if err != nil {
		return nil, fmt.Errorf("deleted-log repair: open git: %w", err)
	}
	head, err := r.Head()
	if err != nil {
		return []deletedlog.DeletedRecord{}, nil // no commits yet
	}
	cIter, err := r.Log(&git.LogOptions{From: head.Hash(), Order: git.LogOrderCommitterTime})
	if err != nil {
		return nil, fmt.Errorf("deleted-log repair: log: %w", err)
	}
	defer cIter.Close()

	depth := s.deletedLogRepairDepth
	if depth <= 0 {
		depth = 50
	}
	decided := make(map[string]bool)
	var out []deletedlog.DeletedRecord
	n := 0

	// mark records the first-seen (newest) decision for a path: deleted records are
	// appended to out; not-deleted paths are only marked decided (to shadow any
	// older delete). Idempotent on an already-decided path.
	mark := func(path string, deleted bool, commitHash string, when time.Time) {
		if path == "" || decided[path] {
			return
		}
		decided[path] = true
		if deleted {
			out = append(out, deletedlog.DeletedRecord{
				Path:           path,
				DeletionCommit: commitHash,
				DeletedAt:      when,
			})
		}
	}

	walkErr := cIter.ForEach(func(c *object.Commit) error {
		if n >= depth {
			return storer.ErrStop
		}
		n++

		added, removed, modified, derr := commitTreeChanges(r, c)
		if derr != nil {
			return nil // skip an unreadable commit; bounded walk continues
		}
		addedSet := toSet(added)
		removedSet := toSet(removed)
		modifiedSet := toSet(modified)
		when := c.Committer.When
		hash := c.Hash.String()

		// Meaning gate: honour the Shoka-Op claim only when it matches the real diff.
		if meta, ok := opmeta.Parse(c.Message); ok && metaMatchesDiff(meta, addedSet, removedSet, modifiedSet) {
			switch meta.Op {
			case opmeta.OpMove:
				mark(meta.From, false, hash, when) // source relocated, NOT deleted
				mark(meta.Path, false, hash, when) // destination present
			case opmeta.OpDelete:
				mark(meta.Path, true, hash, when)
			case opmeta.OpWrite:
				mark(meta.Path, false, hash, when)
			}
		}
		// Conservative raw-diff fallback for every remaining path in the diff
		// (absent/malformed/contradicted metadata, or paths the claim did not
		// cover). Idempotent: paths already decided above are skipped. A removal is a
		// deletion; an add/modify is present. No rename/similarity inference.
		for _, p := range removed {
			mark(p, true, hash, when)
		}
		for _, p := range added {
			mark(p, false, hash, when)
		}
		for _, p := range modified {
			mark(p, false, hash, when)
		}
		return nil
	})
	if walkErr != nil && walkErr != storer.ErrStop {
		return nil, fmt.Errorf("deleted-log repair: walk: %w", walkErr)
	}
	return out, nil
}

// metaMatchesDiff is the MEANING gate: the Shoka-Op claim is honoured only if the
// commit's real tree diff supports it. git's diff is the final authority; the
// metadata only disambiguates delete-vs-move (and only when it agrees).
func metaMatchesDiff(m opmeta.Meta, added, removed, modified map[string]bool) bool {
	switch m.Op {
	case opmeta.OpMove:
		return removed[m.From] && added[m.Path]
	case opmeta.OpDelete:
		return removed[m.Path]
	case opmeta.OpWrite:
		return added[m.Path] || modified[m.Path]
	default:
		return false
	}
}

// commitTreeChanges returns the paths added, removed, and modified by a commit
// relative to its first parent — the same merkletrie classification ListFilesSince
// uses (discovery.go). A root commit reports every file as added.
func commitTreeChanges(r *git.Repository, c *object.Commit) (added, removed, modified []string, err error) {
	cTree, err := c.Tree()
	if err != nil {
		return nil, nil, nil, err
	}
	if c.NumParents() == 0 {
		fIter := cTree.Files()
		_ = fIter.ForEach(func(f *object.File) error {
			added = append(added, f.Name)
			return nil
		})
		return added, nil, nil, nil
	}
	parent, err := c.Parent(0)
	if err != nil {
		return nil, nil, nil, err
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return nil, nil, nil, err
	}
	diff, err := parentTree.Diff(cTree)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, ch := range diff {
		action, aerr := ch.Action()
		if aerr != nil {
			continue
		}
		switch action {
		case merkletrie.Insert:
			added = append(added, ch.To.Name)
		case merkletrie.Delete:
			removed = append(removed, ch.From.Name)
		default:
			modified = append(modified, ch.To.Name)
		}
	}
	return added, removed, modified, nil
}

func toSet(xs []string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}
