package storage

import (
	"fmt"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/shoka/mcp-server/internal/identity"
)

// RecoveryMode selects how a corrupted/dangerous project is repaired (§7.5).
type RecoveryMode string

const (
	// RecoverAcceptWorkingTree adopts the current working tree as truth: stage
	// everything (initialising .git if absent) and commit. Available in any state.
	RecoverAcceptWorkingTree RecoveryMode = "accept-working-tree"
	// RecoverAcceptHead discards working-tree changes back to git HEAD. Available
	// only when the project is corrupted (not dangerous).
	RecoverAcceptHead RecoveryMode = "accept-head"
)

// RecoverProject repairs a project and, on success, returns it to healthy.
func (s *FSGitStorage) RecoverProject(namespace, projectName string, mode RecoveryMode) error {
	projectPath, err := s.getProjectPath(namespace, projectName)
	if err != nil {
		return err
	}

	switch mode {
	case RecoverAcceptWorkingTree:
		r, err := git.PlainOpen(projectPath)
		if err != nil {
			// Dangerous: .git missing/unreadable — re-initialise it.
			r, err = git.PlainInit(projectPath, false)
			if err != nil {
				return fmt.Errorf("failed to initialise git repository: %w", err)
			}
		}
		w, err := r.Worktree()
		if err != nil {
			return fmt.Errorf("worktree: %w", err)
		}
		if err := w.AddWithOptions(&git.AddOptions{All: true}); err != nil {
			return fmt.Errorf("stage working tree: %w", err)
		}
		st, err := w.Status()
		if err != nil {
			return fmt.Errorf("status: %w", err)
		}
		if !st.IsClean() {
			// Recovery is an operator/system action: the owning user is the
			// committer, a "shoka-recovery" system agent is the author, identity in
			// trailers (intentional, not the old hardcoded literal). PROVISIONAL —
			// internal/identity (B-28).
			now := time.Now()
			recID := identity.CommitIdentity{
				AgentName: "shoka-recovery",
			}.WithDefaults(s.identityDefaults)
			if _, err := w.Commit("Recovery: working tree adopted as truth\n\n"+recID.Trailers(), &git.CommitOptions{
				Author:    &object.Signature{Name: recID.AgentName, Email: identity.AgentEmail(recID.AgentName), When: now},
				Committer: &object.Signature{Name: recID.UserName, Email: recID.UserEmail, When: now},
			}); err != nil {
				return fmt.Errorf("commit recovery: %w", err)
			}
		}
		// Rebuild the catalog from the new HEAD so it agrees with the adopted
		// working tree (design log §9 / directive §8: recovery rebuilds the
		// catalog as a side effect).
		if rerr := s.rebuildAndRegister(namespace, projectName); rerr != nil {
			s.log().Warn("recovery: catalog rebuild failed (non-fatal)",
				"namespace", namespace, "project", projectName, "err", rerr)
		}
		s.setState(namespace, projectName, StateHealthy)
		return nil

	case RecoverAcceptHead:
		if s.State(namespace, projectName) == StateDangerous {
			return fmt.Errorf("accept-head recovery is not available for a dangerous project")
		}
		r, err := git.PlainOpen(projectPath)
		if err != nil {
			return fmt.Errorf("failed to open git repository: %w", err)
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
		// Rebuild the catalog from HEAD now that the working tree has been reset
		// to it (directive §8: recovery rebuilds the catalog as a side effect).
		if rerr := s.rebuildAndRegister(namespace, projectName); rerr != nil {
			s.log().Warn("recovery: catalog rebuild failed (non-fatal)",
				"namespace", namespace, "project", projectName, "err", rerr)
		}
		s.setState(namespace, projectName, StateHealthy)
		return nil

	default:
		return fmt.Errorf("unknown recovery mode: %q", mode)
	}
}
