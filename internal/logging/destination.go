package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Bounded-footprint defaults applied when a file knob is left at zero.
// lumberjack retains EVERY backup when MaxBackups and MaxAge are both zero
// (its documented behaviour), which would grow without bound — so a zero here
// resolves to a bounded value rather than "unlimited". The requirement is
// "bounded": these defaults guarantee it even when the operator sets nothing.
const (
	defaultMaxSizeMB  = 100
	defaultMaxBackups = 7
	defaultMaxAgeDays = 30
)

// dailyRotationInterval is how often the at-least-daily time-trigger fires.
// lumberjack rotates on size alone; this drives a Rotate() so the active file
// cycles at least once per day even with no size pressure.
const dailyRotationInterval = 24 * time.Hour

// FileConfig describes a bounded file log destination. It is the dependency-free
// shape internal/logging accepts; the caller (cmd/server) maps its config onto
// it, so internal/logging stays decoupled from internal/config.
type FileConfig struct {
	Path        string
	MaxSizeMB   int
	MaxBackups  int
	MaxAgeDays  int
	Compress    bool
	RotateDaily bool
}

// rotator is the slice of lumberjack the rotation trigger needs; an interface so
// the time-trigger can be tested without touching the filesystem or a clock.
type rotator interface{ Rotate() error }

// Destination is a resolved log output destination: the io.Writer slog writes
// to, plus the lifecycle the caller drives (Close to flush/release, and an
// optional at-least-daily rotation trigger). The stderr destination has no
// closer and no rotator, so its Close and StartDailyRotation are no-ops —
// today's behaviour, unchanged.
type Destination struct {
	Writer  io.Writer
	closer  io.Closer // nil for stderr
	rotator rotator   // non-nil only for a file destination with daily rotation
}

// Stderr returns the default destination: os.Stderr, no closer, no rotation.
// An unset/"stderr" log output resolves here, preserving the historical sink
// byte-for-byte.
func Stderr() *Destination {
	return &Destination{Writer: os.Stderr}
}

// OpenFile resolves a bounded file destination backed by lumberjack. It FAILS
// LOUD if the path's parent directory cannot be created or the file cannot be
// opened for appending — it never silently falls back to stderr (B-58). The
// returned writer is lumberjack, so the on-disk footprint is bounded by size +
// retention; with RotateDaily the active file also cycles at least daily once
// StartDailyRotation is called.
func OpenFile(cfg FileConfig) (*Destination, error) {
	if cfg.Path == "" {
		return nil, fmt.Errorf("log file destination: empty path")
	}
	dir := filepath.Dir(cfg.Path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("log file destination %q: create dir %q: %w", cfg.Path, dir, err)
	}
	// lumberjack opens lazily on the first write, so probe writability eagerly
	// here to fail startup loud on an unwritable path instead of silently later.
	f, err := os.OpenFile(cfg.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("log file destination %q: %w", cfg.Path, err)
	}
	_ = f.Close()

	maxSize := cfg.MaxSizeMB
	if maxSize <= 0 {
		maxSize = defaultMaxSizeMB
	}
	maxBackups := cfg.MaxBackups
	if maxBackups <= 0 {
		maxBackups = defaultMaxBackups
	}
	maxAge := cfg.MaxAgeDays
	if maxAge <= 0 {
		maxAge = defaultMaxAgeDays
	}

	lj := &lumberjack.Logger{
		Filename:   cfg.Path,
		MaxSize:    maxSize,
		MaxBackups: maxBackups,
		MaxAge:     maxAge,
		Compress:   cfg.Compress,
	}
	d := &Destination{Writer: lj, closer: lj}
	if cfg.RotateDaily {
		d.rotator = lj
	}
	return d, nil
}

// Close flushes and releases the destination. It is safe (a no-op) on a stderr
// destination, which must never close os.Stderr.
func (d *Destination) Close() error {
	if d.closer != nil {
		return d.closer.Close()
	}
	return nil
}

// StartDailyRotation launches a goroutine that rotates the destination at least
// once per day until ctx is cancelled, so the active log file cycles daily even
// with no size pressure. It is a no-op when daily rotation is off or the
// destination has no rotator (stderr). Rotation errors are logged, not fatal.
func (d *Destination) StartDailyRotation(ctx context.Context, logger *slog.Logger) {
	if d.rotator == nil {
		return
	}
	t := time.NewTicker(dailyRotationInterval)
	go func() {
		defer t.Stop()
		runRotation(ctx, t.C, d.rotator, logger)
	}()
}

// runRotation is the testable core of the daily time-trigger: on each tick it
// calls Rotate(); it returns when ctx is done. A test drives it with a channel
// it controls and a fake rotator, so no real clock or file is needed. Rotate is
// mutex-guarded inside lumberjack, so it is safe to call concurrently with the
// shared-sink writes.
func runRotation(ctx context.Context, tick <-chan time.Time, r rotator, logger *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			if err := r.Rotate(); err != nil && logger != nil {
				logger.Error("daily log rotation failed", "error", err)
			}
		}
	}
}
