//go:build ignore

// Package badrefwrite is a deliberately-bad fixture for the Anchor-3 ref-write
// linter self-test (TestRefWriteScannerDetectsViolation). It performs go-git ref
// writes outside the atomic funnel — both a tier-1 storer write and tier-2
// porcelain writes. The //go:build ignore tag and the testdata/ location keep it
// out of every real build and out of the module-wide scans; only the self-test
// parses it directly. It is never compiled, so it need not be wired to anything.
package badrefwrite

import (
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

func bad(r *git.Repository, w *git.Worktree) {
	// Tier 1: storer ref write (unconditional, go-git-specific name).
	ref := plumbing.NewHashReference("refs/heads/x", plumbing.ZeroHash)
	_ = r.Storer.SetReference(ref)

	// Tier 2: porcelain ref writes (flagged because the args reference go-git).
	_, _ = w.Commit("m", &git.CommitOptions{All: true})
	_ = w.Reset(&git.ResetOptions{Mode: git.HardReset})

	// Not a ref write: Clean is ref-write-free and must NOT be flagged.
	_ = w.Clean(&git.CleanOptions{Dir: true})
}
