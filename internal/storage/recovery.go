package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/sopranoworks/shoka/internal/identity"
)

// Recovery business-intent operations.
//
// These two methods are the *complete* recovery vocabulary the storage submodule
// exposes; there is no caller-selected "mode" on the public API. The mode→intent
// mapping (the Web UI dialog's radio button, the CLI's --accept-* flag) lives at
// the user-input boundary (internal/adminapi, cmd/shoka), which translates a
// user choice into the one intent it means. The submodule receives only the
// resolved intent. (2026-06-01 gitwrap directive: "take option choices away from
// callers"; a mode flag is an option struct by another name.)
//
// Both methods build their git state through atomic primitives (advanceHead /
// resetIndexToTree from refwrite.go, the shared tree helpers from commit.go)
// rather than go-git's porcelain (w.Commit / w.Reset). go-git's porcelain bundles
// a non-atomic ref write (setHEADCommit → SetReference → O_TRUNC) into the
// operation, which would expose the 2026-06-02 ref-write race if recovery ever
// ran concurrently with live reads. Recovery runs on a halted project today, so
// the race does not fire — but Anchor 3 (2026-06-02 directive) closes the latent
// violation structurally: archlint fails the build on any go-git ref-write API
// call in internal/storage, so this code cannot regress to the porcelain.

// ResyncToHead is the in-product recovery for a project that an EXTERNAL git HEAD
// move stranded in a false `corrupted` state (a host `git reset`, the documented
// out-of-band "git add" landing, a revert): it re-derives the project's state
// against the LIVE on-disk git HEAD and re-syncs the (possibly stale) catalog to it,
// then returns the resulting state. It is a thin, explicitly-named entry point over
// DetectDrift, whose HEAD reconciliation does the work — a working tree that is
// clean vs HEAD is restored to healthy and its catalog rebuilt from HEAD.
//
// It is deliberately NON-destructive: it neither commits nor discards anything. A
// working tree with GENUINE uncommitted drift therefore still reports `corrupted`
// (DetectDrift will not rebuild over real divergence); the operator's path for that
// is the destructive RepairTrackedChanges (adopt) or RestoreToLatest (discard)
// intents. This is the "clears a FALSE corrupted flag" recovery the MCP
// `recover_project` tool and the Web UI recover action both invoke.
func (s *FSGitStorage) ResyncToHead(namespace, projectName string) (ProjectState, error) {
	sum, err := s.DetectDrift(namespace, projectName)
	if err != nil {
		return s.State(namespace, projectName), err
	}
	return sum.State, nil
}

// RepairTrackedChanges adopts the working tree's TRACKED changes as truth: it
// stages only files git already tracks that have been modified or deleted, then
// commits them and returns the project to healthy. It returns the new commit
// hash, or "" when there was nothing tracked to commit.
//
// Untracked files are deliberately NOT adopted: they are left in place on disk,
// uncommitted. This is the structural fix for the contamination the 2026-06-01
// recovery investigation found — the old accept-working-tree path used go-git's
// AddOptions{All:true} ("git add -A"), which swept an untracked .DS_Store into a
// commit. Shoka's founding rule is that "git add ." is forbidden: content of
// unknown provenance must never be adopted without intent.
//
// The set of changes adopted is exactly the working tree's tracked entries whose
// worktree status is Modified or Deleted — the same set go-git's CommitOptions
// {All:true} ("git commit -a") would auto-stage, but selected explicitly here
// (via w.Status, a read) and committed through the atomic commit-object + ref
// path rather than the porcelain. An Untracked entry is never in this set, so the
// "never adopt untracked" guarantee is now explicit rather than implicit in
// go-git's autoAddModifiedAndDeleted internals.
//
// Note on a freshly re-initialised repository (dangerous state, .git absent):
// after PlainInit there is no HEAD and every working-tree file is untracked, so
// there are no tracked changes to adopt and this method commits nothing. Bringing
// a bare working tree's content under git without adopting junk requires ignore
// patterns to separate content from noise — that is the later lost+found /
// shoka.ignore directive's job, not this one. The structural principle (never
// adopt untracked content) holds uniformly across both states.
//
// The commit is authored by the "shoka-recovery" system agent with the configured
// owning user as committer (per the 2026-06-01 identity-config directive); a user
// carried on ctx substitutes the committer (the future-auth seam). PROVISIONAL —
// see internal/identity (B-28).
func (s *FSGitStorage) RepairTrackedChanges(ctx context.Context, namespace, projectName string) (string, error) {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return "", err
	}

	r, err := git.PlainOpen(projectPath)
	if err != nil {
		// Dangerous: .git missing/unreadable — re-initialise it. There is no HEAD
		// yet, so nothing is tracked and there is nothing to adopt (see doc).
		if _, ierr := git.PlainInit(projectPath, false); ierr != nil {
			return "", fmt.Errorf("initialise git repository: %w", ierr)
		}
		return s.finishRecovery(namespace, projectName, "")
	}

	// No HEAD (fresh repo, no commits): nothing tracked can have changed.
	head, herr := r.Head()
	if herr != nil {
		return s.finishRecovery(namespace, projectName, "")
	}
	headCommit, cerr := r.CommitObject(head.Hash())
	if cerr != nil {
		return "", fmt.Errorf("resolve HEAD commit: %w", cerr)
	}
	baseTree := headCommit.TreeHash

	w, err := r.Worktree()
	if err != nil {
		return "", fmt.Errorf("worktree: %w", err)
	}
	st, err := w.Status()
	if err != nil {
		return "", fmt.Errorf("status: %w", err)
	}

	// Adopt tracked Modified/Deleted entries into a new tree derived from HEAD's
	// tree. Untracked ('?') entries are skipped — never adopted.
	newTree := baseTree
	for path, fileStatus := range st {
		comps := strings.Split(path, "/")
		switch fileStatus.Worktree {
		case git.Modified:
			content, rerr := os.ReadFile(filepath.Join(projectPath, filepath.FromSlash(path)))
			if rerr != nil {
				return "", fmt.Errorf("read tracked change %q: %w", path, rerr)
			}
			blob, berr := storeBlob(r, content)
			if berr != nil {
				return "", fmt.Errorf("store blob for %q: %w", path, berr)
			}
			newTree, err = applyToTree(r, newTree, comps, blob, false)
			if err != nil {
				return "", fmt.Errorf("apply tracked change %q: %w", path, err)
			}
		case git.Deleted:
			newTree, err = applyToTree(r, newTree, comps, plumbing.ZeroHash, true)
			if err != nil {
				return "", fmt.Errorf("apply tracked deletion %q: %w", path, err)
			}
		}
	}

	if newTree == baseTree {
		// Nothing tracked changed: nothing to adopt, not an error.
		return s.finishRecovery(namespace, projectName, "")
	}

	now := time.Now()
	recID := recoveryIdentity(ctx, s.identityDefaults)
	commit := &object.Commit{
		Author:       object.Signature{Name: recID.AgentName, Email: identity.AgentEmail(recID.AgentName), When: now},
		Committer:    object.Signature{Name: recID.UserName, Email: recID.UserEmail, When: now},
		Message:      "Recovery: tracked changes adopted\n\n" + recID.Trailers(),
		TreeHash:     newTree,
		ParentHashes: []plumbing.Hash{head.Hash()},
	}
	cobj := r.Storer.NewEncodedObject()
	if eerr := commit.Encode(cobj); eerr != nil {
		return "", fmt.Errorf("encode recovery commit: %w", eerr)
	}
	commitHash, serr := r.Storer.SetEncodedObject(cobj)
	if serr != nil {
		return "", fmt.Errorf("store recovery commit: %w", serr)
	}
	if aerr := advanceHead(projectPath, r, commitHash); aerr != nil {
		return "", fmt.Errorf("advance HEAD: %w", aerr)
	}
	// Keep the index consistent with the adopted tree (cosmetic for external
	// `git status`); touches no ref. Best-effort.
	if ierr := resetIndexToTree(r, newTree); ierr != nil {
		s.log().Warn("recovery: index reset after commit failed (non-fatal)",
			"namespace", namespace, "project", projectName, "err", ierr)
	}

	return s.finishRecovery(namespace, projectName, commitHash.String())
}

// RestoreToLatest discards working-tree changes back to the latest committed
// state: it restores tracked files to HEAD's content and removes untracked files,
// then returns the project to healthy. It is the inverse of RepairTrackedChanges —
// where that adopts tracked changes, this throws them away. Removing untracked
// files (go-git Clean) is the opposite of contamination, so it is permitted.
// Not available for a dangerous project (no readable HEAD to restore to).
//
// The working tree is restored from HEAD's tree directly (write each tracked
// blob to disk), the index is rebuilt to HEAD, and untracked files are cleaned —
// rather than go-git's Reset(HardReset), which bundles a redundant, non-atomic
// ref write (it would advance the ref to HEAD's own hash via setHEADCommit →
// SetReference → O_TRUNC). HEAD is being restored to itself, so no ref write is
// needed at all; eliminating it closes the latent ref-write race in recovery.
//
// ctx is accepted for symmetry with RepairTrackedChanges and a future
// sender/identity seam; no commit is produced here, so no identity is resolved.
func (s *FSGitStorage) RestoreToLatest(ctx context.Context, namespace, projectName string) error {
	if s.State(namespace, projectName) == StateDangerous {
		return fmt.Errorf("restore-to-latest is not available for a dangerous project")
	}
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return err
	}
	r, err := git.PlainOpen(projectPath)
	if err != nil {
		return fmt.Errorf("open git repository: %w", err)
	}
	head, err := r.Head()
	if err != nil {
		return fmt.Errorf("resolve HEAD: %w", err)
	}
	headCommit, err := r.CommitObject(head.Hash())
	if err != nil {
		return fmt.Errorf("resolve HEAD commit: %w", err)
	}
	headTree, err := headCommit.Tree()
	if err != nil {
		return fmt.Errorf("resolve HEAD tree: %w", err)
	}

	// Restore tracked files to HEAD content (overwrite modified, recreate deleted).
	if rerr := restoreWorktreeToTree(r, projectPath, headTree); rerr != nil {
		return fmt.Errorf("restore working tree: %w", rerr)
	}
	// Index = HEAD; any previously-staged addition is now untracked.
	if ierr := resetIndexToTree(r, headCommit.TreeHash); ierr != nil {
		return fmt.Errorf("reset index to HEAD: %w", ierr)
	}
	// Remove untracked files (former staged additions + genuine noise like .DS_Store).
	w, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	if cerr := w.Clean(&git.CleanOptions{Dir: true}); cerr != nil {
		return fmt.Errorf("clean untracked: %w", cerr)
	}

	// Rebuild the catalog from HEAD now that the working tree has been reset to it.
	if rerr := s.rebuildAndRegister(namespace, projectName); rerr != nil {
		s.log().Warn("recovery: catalog rebuild failed (non-fatal)",
			"namespace", namespace, "project", projectName, "err", rerr)
	}
	s.setState(namespace, projectName, StateHealthy)
	return nil
}

// finishRecovery rebuilds the catalog from the new HEAD, marks the project
// healthy, and returns the commit hash (possibly "") — the shared tail of every
// RepairTrackedChanges exit path.
func (s *FSGitStorage) finishRecovery(namespace, projectName, commitHash string) (string, error) {
	if rerr := s.rebuildAndRegister(namespace, projectName); rerr != nil {
		s.log().Warn("recovery: catalog rebuild failed (non-fatal)",
			"namespace", namespace, "project", projectName, "err", rerr)
	}
	s.setState(namespace, projectName, StateHealthy)
	return commitHash, nil
}

// restoreWorktreeToTree writes every file in tree to its path under projectPath,
// creating parent directories and applying the tree's file mode. It is the
// worktree half of a hard reset, done directly so no ref is touched (HEAD stays
// put). Files present on disk but absent from the tree are left for w.Clean to
// remove (after the index is reset to the tree, they read as untracked).
func restoreWorktreeToTree(r *git.Repository, projectPath string, tree *object.Tree) error {
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
			continue // subtrees are materialised by their file entries' paths
		}
		blob, berr := r.BlobObject(entry.Hash)
		if berr != nil {
			return fmt.Errorf("load blob %q: %w", name, berr)
		}
		rc, rerr := blob.Reader()
		if rerr != nil {
			return fmt.Errorf("open blob %q: %w", name, rerr)
		}
		content, rerr := io.ReadAll(rc)
		rc.Close()
		if rerr != nil {
			return fmt.Errorf("read blob %q: %w", name, rerr)
		}
		osMode, merr := entry.Mode.ToOSFileMode()
		if merr != nil {
			osMode = 0o644
		}
		dest := filepath.Join(projectPath, filepath.FromSlash(name))
		if derr := os.MkdirAll(filepath.Dir(dest), 0o755); derr != nil {
			return fmt.Errorf("create dir for %q: %w", name, derr)
		}
		if werr := os.WriteFile(dest, content, osMode.Perm()); werr != nil {
			return fmt.Errorf("write %q: %w", name, werr)
		}
	}
	return nil
}

// recoveryIdentity builds the commit identity for a recovery commit: the
// "shoka-recovery" system agent as author, the configured owning user as
// committer. A user carried on ctx (future authenticated request, B-28)
// substitutes the committer; the author stays the system recovery agent.
func recoveryIdentity(ctx context.Context, d identity.Defaults) identity.CommitIdentity {
	id := identity.CommitIdentity{AgentName: "shoka-recovery"}.WithDefaults(d)
	if u, ok := identity.UserFrom(ctx); ok && u.Name != "" {
		id.UserName, id.UserEmail = u.Name, u.Email
	}
	return id
}
