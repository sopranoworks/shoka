package storage

import (
	"context"
	"os"
	"path/filepath"
	"time"
)

// The lost+found worker (the 2026-06-02 lost+found worker directive).
//
// A periodic background sweep that enforces Shoka's invariant "a project's
// working tree contains exactly the files git tracks". For every project it
// applies a three-way action to each file:
//
//   - tracked (catalog-known)            -> untouched
//   - untracked, matches shoka.disposable -> deleted
//   - untracked, no match                -> moved to lost+found
//
// "Untracked" is defined against the CATALOG (Shoka's managed-file set), NOT git
// index/HEAD. Shoka commits to git asynchronously (WAL -> background worker) while
// maintaining the catalog synchronously on the write path, so a file that was
// just written through Shoka is catalog-known immediately but not yet in git
// HEAD; keying on git HEAD would move/delete a legitimate in-flight write. The
// catalog-relative untracked set is exactly DriftSummary.Added, and the sweep
// runs only after WaitForWAL so no pending write is mid-flight. (This corrects
// the directive's "not in git index/HEAD" framing — see the 2026-06-02
// lost-found-worker baseline note.)
//
// The worker writes no git commits and no git refs (Anchors 1+2+3 preserved): it
// moves and deletes working-tree files and emits NOTIFY events. Its NOTIFY sender
// is the "shoka-worker" identity so a future worker-side subscriber would be
// excluded from its own events (the 2026-06-01 sender-exclusion seam); it is not
// a subscriber today, so no exclusion fires. No identity.WithAgent is involved —
// that seam only attributes commits, and the worker writes none.

const (
	// lostFoundSender is the NOTIFY sender identity stamped on worker events.
	lostFoundSender = "shoka-worker"
	// kindLostFoundMoved is emitted when an untracked file is moved to lost+found.
	kindLostFoundMoved = "lostfound.moved"
	// kindLostFoundDisposed is emitted when an untracked file is deleted as
	// disposable. Disposals are surfaced too, so an operator can notice if a
	// pattern is matching files they did not expect.
	kindLostFoundDisposed = "lostfound.disposed"
	// kindLostFoundQuarantined is emitted when something that could NOT be processed
	// is set aside in lost+found — a WAL entry that can never commit (D3) or a
	// catalog-init leftover tree that was never a project (D4) — as distinct from
	// lostfound.moved, which is the sweep tidying away a stray untracked file.
	// "quarantined" means "a fault's remains, preserved for an operator"; "moved"
	// means "routine housekeeping". The deposit primitive (depositBytes/depositTree)
	// + notifyQuarantined live in lostfound_area.go; D2 defines the kind and helper,
	// the callers (D3/D4) emit it.
	kindLostFoundQuarantined = "lostfound.quarantined"
)

// StartLostFoundSweep runs an initial sweep, then re-sweeps every project every
// interval, in a single goroutine (so sweeps never overlap). interval <= 0
// disables the worker. The goroutine stops when ctx is cancelled. Mirrors
// StartDriftScan; the startup wiring runs it after StartupInit so catalogs and
// states are ready.
func (s *FSGitStorage) StartLostFoundSweep(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	go func() {
		s.sweepAllProjects()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sweepAllProjects()
			}
		}
	}()
}

// sweepAllProjects waits for the WAL to drain (so no in-flight write is mistaken
// for untracked content), then sweeps every project under the base directory.
func (s *FSGitStorage) sweepAllProjects() {
	s.lostFoundSweeps.Add(1) // one per pass: initial + each tick, distinct from per-file actions
	s.WaitForWAL(2 * time.Minute)
	projects, _ := s.discoverProjects() // leftovers are relocated post-startup, not swept here
	for _, p := range projects {
		s.sweepProject(p.namespace, p.name)
	}
}

// sweepProject applies the three-way action to one project's untracked files. It
// acts only on a HEALTHY project: a dangerous project's git/catalog is
// unreliable, and a corrupted project (tracked Modified/Deleted drift) is left
// for the operator/recovery rather than swept mid-repair. The worker only ever
// touches untracked (catalog-unknown) files; it never modifies a tracked file.
func (s *FSGitStorage) sweepProject(namespace, projectName string) {
	// DetectDrift yields both the catalog-relative untracked set (Added) and the
	// project state, and rebuilds a missing catalog from HEAD as a side effect.
	sum, err := s.DetectDrift(namespace, projectName)
	if err != nil {
		s.log().Error("lost+found sweep: drift detection failed",
			"project", projectKey(namespace, projectName), "error", err)
		return
	}
	if sum.State != StateHealthy {
		// Healthy-only gate: a dangerous project's git/catalog is unreliable, a
		// corrupted project is left for recovery. Count the skip by state — the only
		// two non-healthy ProjectStates (state.go) — so the metric shows the sweep is
		// declining to act and why.
		switch sum.State {
		case StateCorrupted:
			s.lostFoundSkippedCorrupted.Add(1)
		case StateDangerous:
			s.lostFoundSkippedDangerous.Add(1)
		}
		return // skip dangerous/corrupted projects
	}
	if len(sum.Added) == 0 {
		return
	}

	matcher, err := s.effectiveDisposable(namespace, projectName)
	if err != nil {
		s.log().Error("lost+found sweep: load shoka.disposable failed",
			"project", projectKey(namespace, projectName), "error", err)
		return
	}

	now := time.Now()
	target := namespace + "/" + projectName
	projectPath, perr := s.getProjectPath(namespace, projectName)
	if perr != nil {
		return
	}

	for _, rel := range sum.Added {
		if matcher.Match(splitDisposablePath(rel), false) {
			if rerr := os.Remove(filepath.Join(projectPath, filepath.FromSlash(rel))); rerr != nil {
				s.log().Error("lost+found sweep: dispose failed",
					"project", target, "path", rel, "error", rerr)
				continue
			}
			s.lostFoundDisposed.Add(1)
			s.notify.NotifyFrom(lostFoundSender, kindLostFoundDisposed, target, rel)
			continue
		}
		if _, merr := s.moveToLostFound(namespace, projectName, rel, now); merr != nil {
			s.log().Error("lost+found sweep: move to lost+found failed",
				"project", target, "path", rel, "error", merr)
			continue
		}
		s.lostFoundMoved.Add(1)
		s.notify.NotifyFrom(lostFoundSender, kindLostFoundMoved, target, rel)
	}
}

// LostFoundSweeps returns the cumulative number of lost+found sweep passes the
// worker has run, for shoka_lostfound_sweeps_total. A pass that disposes/moves
// nothing still counts (the metric shows the worker is alive and how often it
// sweeps), so it is distinct from the per-file action counts.
func (s *FSGitStorage) LostFoundSweeps() int64 { return s.lostFoundSweeps.Load() }

// LostFoundActions returns the per-file action counts for
// shoka_lostfound_actions_total{action}: disposed (an untracked file matching
// shoka.disposable was deleted) and moved (an untracked non-matching file was
// relocated to lost+found). There is deliberately no "quarantined" action — the
// sweep never quarantines; that count is the walworker's shoka_wal_quarantined_total.
func (s *FSGitStorage) LostFoundActions() (disposed, moved int64) {
	return s.lostFoundDisposed.Load(), s.lostFoundMoved.Load()
}

// LostFoundProjectsSkipped returns the healthy-only-gate skip counts for
// shoka_lostfound_projects_skipped_total{state}: corrupted (tracked-file drift,
// left for recovery) and dangerous (git/catalog unreliable) — the two non-healthy
// ProjectStates on which the sweep declines to act.
func (s *FSGitStorage) LostFoundProjectsSkipped() (corrupted, dangerous int64) {
	return s.lostFoundSkippedCorrupted.Load(), s.lostFoundSkippedDangerous.Load()
}
