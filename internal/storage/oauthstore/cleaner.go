package oauthstore

import (
	"context"
	"log/slog"
	"time"
)

// CleanerConfig configures the OAuth dead-series cleaner sweep (the 2026-06-15
// authz/lifecycle foundation). It mirrors the storage sweep workers' config shape
// (Enabled gate + Interval tick) and adds Grace, the delay past a series'
// refresh-expiry before it is swept.
type CleanerConfig struct {
	Enabled  bool          // gate; false starts no goroutine
	Interval time.Duration // tick cadence; <=0 disables the sweep even if Enabled
	Grace    time.Duration // keep a fully-dead series this long past refresh-expiry
	Logger   *slog.Logger  // sweep log; nil → slog.Default()
}

// StartCleaner launches the periodic OAuth dead-series cleaner, mirroring the
// storage sweep workers (StartSnapshotSweep): a single goroutine driven by a
// time.Ticker, stopped on ctx cancellation. With Enabled=false or Interval<=0 it
// starts NO goroutine. Like StartSnapshotSweep it is tick-only — it does NOT run a
// cycle at boot — so a restart never deletes tokens until the first tick. Each
// tick runs one DeleteDeadSeries(now, Grace); an error is logged and the next tick
// retries. The OAuth store accumulates dead series forever otherwise (no other GC
// exists), so this is on by default in config.
func (s *Store) StartCleaner(ctx context.Context, cfg CleanerConfig) {
	if s == nil || s.db == nil || !cfg.Enabled || cfg.Interval <= 0 {
		return
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	go func() {
		t := time.NewTicker(cfg.Interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				deleted, err := s.DeleteDeadSeries(time.Now(), cfg.Grace)
				if err != nil {
					logger.LogAttrs(ctx, slog.LevelWarn, "oauth cleaner cycle failed",
						slog.Any("error", err))
				} else if deleted > 0 {
					logger.LogAttrs(ctx, slog.LevelInfo, "oauth cleaner removed dead series",
						slog.Int("deleted", deleted))
				}
			}
		}
	}()
}
