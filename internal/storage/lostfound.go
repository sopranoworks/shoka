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
	s.WaitForWAL(2 * time.Minute)
	for _, p := range s.discoverProjects() {
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
			s.notify.NotifyFrom(lostFoundSender, kindLostFoundDisposed, target, rel)
			continue
		}
		if _, merr := s.moveToLostFound(namespace, projectName, rel, now); merr != nil {
			s.log().Error("lost+found sweep: move to lost+found failed",
				"project", target, "path", rel, "error", merr)
			continue
		}
		s.notify.NotifyFrom(lostFoundSender, kindLostFoundMoved, target, rel)
	}
}
