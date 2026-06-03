package storage

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/shoka/mcp-server/internal/identity"
	"github.com/shoka/mcp-server/internal/storage/wal"
)

// emptyTreeHash is git's canonical empty-tree object hash.
var emptyTreeHash = plumbing.NewHash("4b825dc642cb6eb9a060e54bf8d69288fbee4904")

// The atomic ref-write funnel (advanceHead / writeRefAtomic) and the index
// rebuild (resetIndexToTree) live in refwrite.go — the Anchor 3 primitive.

// commitEntry is the walworker CommitFunc: it records one WAL entry into git in
// the background. The commit captures the entry's *own* content (from the WAL),
// built via git plumbing — it does NOT stage the live working tree. This keeps
// one faithful commit per write even when later writes have already changed the
// file on disk, and lets reads keep seeing the latest working-tree content with
// no interference. The git index is reset to the new HEAD afterward so external
// `git status` stays meaningful; the working tree is never touched.
func (s *FSGitStorage) commitEntry(ctx context.Context, e wal.Entry) error {
	projectPath := filepath.Join(s.baseDir, e.Namespace, e.Project)
	r, err := git.PlainOpen(projectPath)
	if err != nil {
		s.setState(e.Namespace, e.Project, StateDangerous)
		s.log().Error("walworker: cannot open git repository; project marked dangerous",
			"project", projectKey(e.Namespace, e.Project), "error", err)
		return fmt.Errorf("open repo: %w", err)
	}

	var parents []plumbing.Hash
	var baseTree plumbing.Hash
	if ref, herr := r.Head(); herr == nil {
		parents = []plumbing.Hash{ref.Hash()}
		if c, cerr := r.CommitObject(ref.Hash()); cerr == nil {
			baseTree = c.TreeHash
		}
	}

	// Build the new tree, commit subject, and change-event kind per op. A "move"
	// folds ONLY the rename into one tree (a pure, history-preserving path change),
	// landing as a single atomic commit. Inbound-link auto-update was decoupled on
	// 2026-06-03 (backlog B-33), so a move entry carries no Aux.
	var newTree plumbing.Hash
	var subject, event string
	switch e.Op {
	case "delete":
		newTree, err = applyToTree(r, baseTree, strings.Split(e.Path, "/"), plumbing.ZeroHash, true)
		if err != nil {
			return fmt.Errorf("build tree: %w", err)
		}
		subject, event = "Delete "+e.Path, "file_deleted"
	case "move":
		newTree, err = buildMoveTree(r, baseTree, e)
		if err != nil {
			return fmt.Errorf("build tree: %w", err)
		}
		subject, event = "Move "+e.MoveFrom+" -> "+e.Path, "file_moved"
	default: // "write"
		blob, berr := storeBlob(r, e.Content)
		if berr != nil {
			return fmt.Errorf("store blob: %w", berr)
		}
		newTree, err = applyToTree(r, baseTree, strings.Split(e.Path, "/"), blob, false)
		if err != nil {
			return fmt.Errorf("build tree: %w", err)
		}
		subject, event = "Update "+e.Path, "file_written"
	}
	if newTree == baseTree {
		// Identical rewrite or delete of an untracked path: nothing to record.
		return nil
	}

	now := time.Now()
	// Intentional commit author (the 2026-06-01 identity-config directive):
	// agent-as-author, owning user as committer, all three layers (user/agent/
	// worker) in canonical Shoka-* trailers. Older entries (no identity) fall back
	// to the configured default. PROVISIONAL — see internal/identity (B-28).
	id := identity.CommitIdentity{
		UserName:     e.UserName,
		UserEmail:    e.UserEmail,
		AgentName:    e.AgentName,
		WorkerID:     e.WorkerID,
		AuthorIsUser: e.AuthorIsUser,
	}.WithDefaults(s.identityDefaults)
	// Author is normally the agent; a web /ws/ui SAVE_FILE (AuthorIsUser) makes the
	// owning user the Author instead. Committer stays the owning user either way;
	// the Shoka-* trailers are unchanged.
	author := object.Signature{Name: id.AgentName, Email: identity.AgentEmail(id.AgentName), When: now}
	if id.AuthorIsUser {
		author = object.Signature{Name: id.UserName, Email: id.UserEmail, When: now}
	}
	committer := object.Signature{Name: id.UserName, Email: id.UserEmail, When: now}

	msg := subject + "\n\n" + id.Trailers()
	commit := &object.Commit{Author: author, Committer: committer, Message: msg, TreeHash: newTree, ParentHashes: parents}
	cobj := r.Storer.NewEncodedObject()
	if err := commit.Encode(cobj); err != nil {
		return fmt.Errorf("encode commit: %w", err)
	}
	commitHash, err := r.Storer.SetEncodedObject(cobj)
	if err != nil {
		return fmt.Errorf("store commit: %w", err)
	}

	if err := advanceHead(projectPath, r, commitHash); err != nil {
		return fmt.Errorf("advance HEAD: %w", err)
	}

	// Keep the index consistent with the new commit's tree without touching the
	// working tree, so an operator's `git status` reflects reality. Best-effort.
	// This rebuilds the index directly rather than via go-git's Reset(MixedReset):
	// Reset's setHEADCommit re-advances the ref with a redundant, non-atomic
	// O_TRUNC write that would re-open the race the atomic advanceHead above closes
	// (the 2026-06-02 ref-write race). The index write touches no ref.
	if ierr := resetIndexToTree(r, newTree); ierr != nil {
		s.log().Warn("walworker: index reset after commit failed (non-fatal)",
			"project", projectKey(e.Namespace, e.Project), "error", ierr)
	}

	s.emit(ChangeEvent{
		Event:      event,
		Namespace:  e.Namespace,
		Project:    e.Project,
		Path:       e.Path,
		CommitHash: commitHash.String(),
		Timestamp:  now,
	})
	return nil
}

// buildMoveTree derives a single tree from baseTree that applies the move
// atomically: the source path is removed and the destination is added carrying the
// moved file's (unchanged) bytes. Because the destination blob is rebuilt from the
// unchanged Content, its hash equals the source's last-committed blob hash — which
// is exactly what lets `git log --follow` recognise the rename (the move-file
// directive §1.1). The move is a PURE rename: it folds no other file's content.
// Returns one tree hash; commitEntry turns it into one commit.
//
// RE-ENABLEMENT SEAM (B-33): inbound-link auto-update on move was decoupled on
// 2026-06-03, so e.Aux is always empty for a move. To restore link-update-on-move
// once a reverse-link index exists, re-fold e.Aux here — store each AuxFile.Content
// as a blob and applyToTree it onto the tree — alongside re-wiring storage.Move to
// populate Aux (see rewriteInboundLinksForMove). Until then a move touches only the
// two paths.
func buildMoveTree(r *git.Repository, baseTree plumbing.Hash, e wal.Entry) (plumbing.Hash, error) {
	tree, err := applyToTree(r, baseTree, strings.Split(e.MoveFrom, "/"), plumbing.ZeroHash, true)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("remove source: %w", err)
	}
	destBlob, err := storeBlob(r, e.Content)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("store moved blob: %w", err)
	}
	tree, err = applyToTree(r, tree, strings.Split(e.Path, "/"), destBlob, false)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("add destination: %w", err)
	}
	return tree, nil
}

// storeBlob writes content as a git blob and returns its hash.
func storeBlob(r *git.Repository, content []byte) (plumbing.Hash, error) {
	obj := r.Storer.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(content); err != nil {
		_ = w.Close()
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return r.Storer.SetEncodedObject(obj)
}

// applyToTree returns the hash of a tree derived from baseTree (zero = empty)
// with the path given by comps set to blob (write) or removed (delete). Nested
// directories are rebuilt bottom-up.
func applyToTree(r *git.Repository, baseTree plumbing.Hash, comps []string, blob plumbing.Hash, isDelete bool) (plumbing.Hash, error) {
	entries, err := loadTreeEntries(r, baseTree)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	name := comps[0]

	if len(comps) == 1 {
		out := withoutEntry(entries, name)
		if !isDelete {
			out = append(out, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: blob})
		}
		return storeTree(r, out)
	}

	var subHash plumbing.Hash
	for _, e := range entries {
		if e.Name == name && e.Mode == filemode.Dir {
			subHash = e.Hash
		}
	}
	newSub, err := applyToTree(r, subHash, comps[1:], blob, isDelete)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	out := withoutEntry(entries, name)
	if !(isDelete && newSub == emptyTreeHash) {
		out = append(out, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: newSub})
	}
	return storeTree(r, out)
}

func loadTreeEntries(r *git.Repository, h plumbing.Hash) ([]object.TreeEntry, error) {
	if h.IsZero() {
		return nil, nil
	}
	t, err := r.TreeObject(h)
	if err != nil {
		return nil, err
	}
	return append([]object.TreeEntry(nil), t.Entries...), nil
}

func withoutEntry(entries []object.TreeEntry, name string) []object.TreeEntry {
	out := make([]object.TreeEntry, 0, len(entries))
	for _, e := range entries {
		if e.Name != name {
			out = append(out, e)
		}
	}
	return out
}

// storeTree encodes and stores a tree, sorting entries per git's convention
// (directory names compare as if suffixed with "/").
func storeTree(r *git.Repository, entries []object.TreeEntry) (plumbing.Hash, error) {
	sort.Slice(entries, func(i, j int) bool {
		return treeSortName(entries[i]) < treeSortName(entries[j])
	})
	t := &object.Tree{Entries: entries}
	obj := r.Storer.NewEncodedObject()
	if err := t.Encode(obj); err != nil {
		return plumbing.ZeroHash, err
	}
	return r.Storer.SetEncodedObject(obj)
}

func treeSortName(e object.TreeEntry) string {
	if e.Mode == filemode.Dir {
		return e.Name + "/"
	}
	return e.Name
}
