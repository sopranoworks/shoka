package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/shoka/mcp-server/internal/identity"
)

// Recovery business-intent operations.
//
// These two methods are the *complete* recovery vocabulary the storage submodule
// exposes; there is no caller-selected "mode" on the public API. The mode→intent
// mapping (the Web UI dialog's radio button, the CLI's --accept-* flag) lives at
// the user-input boundary (internal/adminapi, cmd/server), which translates a
// user choice into the one intent it means. The submodule receives only the
// resolved intent. (2026-06-01 gitwrap directive: "take option choices away from
// callers"; a mode flag is an option struct by another name.)

// RepairTrackedChanges adopts the working tree's TRACKED changes as truth: it
// stages only files git already tracks that have been modified or deleted, then
// commits them and returns the project to healthy. It returns the new commit
// hash, or "" when there was nothing tracked to commit.
//
// Untracked files are deliberately NOT staged: they are left in place on disk,
// uncommitted. This is the structural fix for the contamination the 2026-06-01
// recovery investigation found — the old accept-working-tree path used go-git's
// AddOptions{All:true} ("git add -A"), which swept an untracked .DS_Store into a
// commit. Shoka's founding rule is that "git add ." is forbidden: content of
// unknown provenance must never be adopted without intent. The tracked-only
// staging is expressed through go-git's CommitOptions{All:true} ("git commit -a"),
// whose autoAddModifiedAndDeleted stages only Modified/Deleted tracked entries
// and structurally cannot touch an untracked file.
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
		// yet, so nothing is tracked and the commit below is a no-op (see doc).
		r, err = git.PlainInit(projectPath, false)
		if err != nil {
			return "", fmt.Errorf("initialise git repository: %w", err)
		}
	}
	w, err := r.Worktree()
	if err != nil {
		return "", fmt.Errorf("worktree: %w", err)
	}

	now := time.Now()
	recID := recoveryIdentity(ctx, s.identityDefaults)
	var commitHash string
	// CommitOptions{All:true} == "git commit -a": stage tracked modified+deleted
	// only, never untracked. ErrEmptyCommit (nothing tracked changed) is the
	// "nothing to adopt" case and is not an error.
	h, cerr := w.Commit("Recovery: tracked changes adopted\n\n"+recID.Trailers(), &git.CommitOptions{
		All:       true,
		Author:    &object.Signature{Name: recID.AgentName, Email: identity.AgentEmail(recID.AgentName), When: now},
		Committer: &object.Signature{Name: recID.UserName, Email: recID.UserEmail, When: now},
	})
	switch {
	case cerr == nil:
		commitHash = h.String()
	case errors.Is(cerr, git.ErrEmptyCommit):
		// No tracked changes (or a bare re-init): nothing adopted, not an error.
	default:
		return "", fmt.Errorf("commit recovery: %w", cerr)
	}

	// Rebuild the catalog from the new HEAD so it agrees with the adopted tracked
	// state (the catalog is rebuilt as a side effect of recovery).
	if rerr := s.rebuildAndRegister(namespace, projectName); rerr != nil {
		s.log().Warn("recovery: catalog rebuild failed (non-fatal)",
			"namespace", namespace, "project", projectName, "err", rerr)
	}
	s.setState(namespace, projectName, StateHealthy)
	return commitHash, nil
}

// RestoreToLatest discards working-tree changes back to the latest committed
// state: it hard-resets tracked files to HEAD and removes untracked files, then
// returns the project to healthy. It is the inverse of RepairTrackedChanges —
// where that adopts tracked changes, this throws them away. Removing untracked
// files (go-git Clean) is the opposite of contamination, so it is permitted.
// Not available for a dangerous project (no readable HEAD to restore to).
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
	w, err := r.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	// Discard tracked changes back to HEAD, then remove untracked files.
	if err := w.Reset(&git.ResetOptions{Mode: git.HardReset}); err != nil {
		return fmt.Errorf("reset to HEAD: %w", err)
	}
	if err := w.Clean(&git.CleanOptions{Dir: true}); err != nil {
		return fmt.Errorf("clean untracked: %w", err)
	}
	// Rebuild the catalog from HEAD now that the working tree has been reset to it.
	if rerr := s.rebuildAndRegister(namespace, projectName); rerr != nil {
		s.log().Warn("recovery: catalog rebuild failed (non-fatal)",
			"namespace", namespace, "project", projectName, "err", rerr)
	}
	s.setState(namespace, projectName, StateHealthy)
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
