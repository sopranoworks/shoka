package storage

import (
	"context"
	"time"
)

// SnapshotSweepConfig is the runtime configuration the snapshot scheduler and
// the on-demand cycle need. It is a storage-local type (not the config package's
// yaml struct) so internal/storage never imports internal/config — the scope
// string is parsed to a Scope by the caller (cmd/shoka via ParseScope), keeping
// the dependency direction config→storage only.
type SnapshotSweepConfig struct {
	Enabled         bool          // scheduler gate; false starts no goroutine
	Interval        time.Duration // scheduler tick; <=0 disables the scheduler
	OutputDir       string        // destination root for <output>/<ns>/<proj>/<ts>.tar.gz
	Scope           Scope         // which projects a cycle covers
	RetentionCount  int           // keep N newest per project (0 = off)
	RetentionMaxAge time.Duration // prune older than this (0 = off)
}

// StartSnapshotSweep launches the periodic backup scheduler, mirroring the
// lost+found/index sweep workers: a single goroutine driven by a time.Ticker,
// stopped on ctx cancellation. enabled is the config gate; with enabled=false or
// Interval<=0 it starts NO goroutine. Unlike StartLostFoundSweep it does NOT run
// an immediate cycle at boot (it mirrors StartDriftScan's tick-only model): a
// full archive of every project on every restart would be surprising and heavy;
// "snapshot now" is the on-demand admin/CLI path. Each tick runs one
// RunSnapshotCycle; a per-project failure is non-fatal (logged) and the next tick
// retries.
func (s *FSGitStorage) StartSnapshotSweep(ctx context.Context, cfg SnapshotSweepConfig) {
	if !cfg.Enabled || cfg.Interval <= 0 {
		return
	}
	go func() {
		t := time.NewTicker(cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				written, pruned, err := s.RunSnapshotCycle(ctx, cfg)
				if err != nil {
					s.log().Warn("snapshot cycle had per-project errors",
						"written", written, "pruned", pruned, "error", err)
				} else {
					s.log().Info("snapshot cycle complete", "written", written, "pruned", pruned)
				}
			}
		}
	}()
}

// RunSnapshotCycle runs one backup cycle: snapshot every project in scope to the
// output dir (resilient fan-out), then prune old snapshots. It returns the number
// of archives written, the number pruned, and an aggregate of any per-project
// snapshot errors (non-fatal — phase-2 resilience; the others still succeed). It
// sequences the phase-2 functions only; no new git/lock/read surface. It is
// exported so the admin endpoint and tests can drive it directly (no ticker).
func (s *FSGitStorage) RunSnapshotCycle(ctx context.Context, cfg SnapshotSweepConfig) (written, pruned int, err error) {
	results, scopeErr := s.SnapshotScope(ctx, cfg.Scope, cfg.OutputDir)
	for _, r := range results {
		if r.Err == nil {
			written++
		}
	}

	// Prune even when some projects failed — successful projects still produced a
	// snapshot whose retention should apply; a failed project simply has nothing
	// new to prune against.
	removed, pruneErr := s.PruneSnapshots(cfg.OutputDir, cfg.Scope, cfg.RetentionCount, cfg.RetentionMaxAge)
	pruned = len(removed)

	// Surface the snapshot fan-out error (per-project failures) as the primary
	// signal; a prune error is reported only if the cycle was otherwise clean.
	if scopeErr != nil {
		return written, pruned, scopeErr
	}
	return written, pruned, pruneErr
}
