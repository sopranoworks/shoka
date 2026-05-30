package storage

import (
	"fmt"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
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
			if _, err := w.Commit("Recovery: working tree adopted as truth", &git.CommitOptions{
				Author: &object.Signature{Name: "MCP Server", Email: "mcp-server@shoka.io", When: time.Now()},
			}); err != nil {
				return fmt.Errorf("commit recovery: %w", err)
			}
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
		s.setState(namespace, projectName, StateHealthy)
		return nil

	default:
		return fmt.Errorf("unknown recovery mode: %q", mode)
	}
}
