package storage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// This file is the atomic ref-write funnel (Anchor 3, 2026-06-02 directive).
//
// THE PROPERTY: every git-ref write in Shoka is atomic from any reader's
// perspective — a concurrent lock-free reader observes the old hash or the new
// one, never an empty or partial ref file. The mechanism is a temp file +
// os.Rename within the ref's own directory (rename(2) is atomic on POSIX), which
// is what real git uses and what the storage redesign's "serialized writer +
// lock-free reads" design already assumed of every write step.
//
// THE ENFORCEMENT: nothing in internal/storage may write a ref through go-git's
// porcelain (Worktree.Commit / Reset / Checkout) or its storer
// (Storer.SetReference / CheckAndSetReference / RemoveReference) — those go
// through filesystem setRefRwfs, which truncates the ref with O_TRUNC before the
// flock and before writing the new hash, exposing a transient empty ref (the
// 2026-06-02 ref-write race). internal/archlint fails the build on any such call.
// writeRefAtomic below is NOT an exception to that ban: it writes the ref file
// directly with os primitives and calls no go-git ref API, so the ban is total.
//
// (git.PlainInit, used to construct a repo, writes the symbolic HEAD
// non-atomically — but that is repo construction before any reader can observe
// the project, not a hash-advancing loose-ref write, so it is deliberately not
// in archlint's blocklist.)

// gitDirName is the per-project git metadata directory (non-bare repos).
const gitDirName = ".git"

// advanceHead atomically points the branch that HEAD references (or HEAD itself,
// if detached) at commitHash. Reading HEAD to find the target ref is a read; the
// write goes through writeRefAtomic.
func advanceHead(projectPath string, r *git.Repository, commitHash plumbing.Hash) error {
	headRef, err := r.Reference(plumbing.HEAD, false)
	if err != nil {
		return err
	}
	target := plumbing.HEAD
	if headRef.Type() == plumbing.SymbolicReference {
		target = headRef.Target()
	}
	return writeRefAtomic(projectPath, target, commitHash)
}

// writeRefAtomic writes commitHash to the loose ref file for refName under the
// project's git dir, atomically, in git's on-disk loose-ref format (40-char hex
// hash + LF). The temp file is created in the ref's own directory so os.Rename is
// an intra-directory move (atomic on POSIX). The parent dir is created if absent
// (the first commit creates refs/heads/<branch>).
//
// This is the single primitive every ref write funnels through. It performs the
// write with os, not go-git, by design (see file header).
func writeRefAtomic(projectPath string, refName plumbing.ReferenceName, commitHash plumbing.Hash) error {
	refPath := filepath.Join(projectPath, gitDirName, filepath.FromSlash(refName.String()))
	dir := filepath.Dir(refPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create ref dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(refPath)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp ref: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(commitHash.String() + "\n"); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("write temp ref: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("close temp ref: %w", err)
	}
	if err := os.Rename(tmpName, refPath); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("rename ref into place: %w", err)
	}
	return nil
}

// resetIndexToTree rebuilds the git index to match treeHash — one entry per file
// ({Name, Hash, Mode}, the same fields go-git's own resetIndex records; the index
// encoder fills the remaining stat fields with zero and git refreshes them on
// first use). It writes ONLY the index, never a ref — the replacement for go-git's
// Reset(MixedReset), whose bundled setHEADCommit performs a redundant, non-atomic
// ref write. Best-effort callers treat the index as cosmetic for external
// `git status`.
func resetIndexToTree(r *git.Repository, treeHash plumbing.Hash) error {
	tree, err := r.TreeObject(treeHash)
	if err != nil {
		return fmt.Errorf("load tree: %w", err)
	}
	idx := &index.Index{Version: 2}
	walker := object.NewTreeWalker(tree, true, nil)
	defer walker.Close()
	for {
		name, entry, werr := walker.Next()
		if werr == io.EOF {
			break
		}
		if werr != nil {
			return fmt.Errorf("walk tree: %w", werr)
		}
		if !entry.Mode.IsFile() {
			continue // skip subtree (directory) entries
		}
		idx.Entries = append(idx.Entries, &index.Entry{
			Name: name,
			Hash: entry.Hash,
			Mode: entry.Mode,
		})
	}
	return r.Storer.SetIndex(idx)
}
