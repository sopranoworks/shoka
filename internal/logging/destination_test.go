package logging

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// TestStderr_DefaultIsOsStderr asserts the unset/"stderr" destination is exactly
// os.Stderr, with no closer and no rotator — today's behaviour, byte-for-byte.
func TestStderr_DefaultIsOsStderr(t *testing.T) {
	d := Stderr()
	if d.Writer != os.Stderr {
		t.Errorf("default destination writer = %v, want os.Stderr", d.Writer)
	}
	if d.closer != nil {
		t.Error("stderr destination should have no closer")
	}
	if d.rotator != nil {
		t.Error("stderr destination should have no rotator")
	}
	// Close and StartDailyRotation must be safe no-ops on stderr.
	if err := d.Close(); err != nil {
		t.Errorf("Close on stderr destination: %v", err)
	}
	d.StartDailyRotation(context.Background(), nil) // must not panic / spawn anything
}

// TestOpenFile_WritesToFile asserts a file destination writes to the selected
// path (the configurable seam), and that Close releases it.
func TestOpenFile_WritesToFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shoka.log")
	d, err := OpenFile(FileConfig{Path: path})
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	if _, err := d.Writer.Write([]byte("hello destination\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(string(data), "hello destination") {
		t.Errorf("file %q missing written line, got %q", path, data)
	}
}

// TestOpenFile_AppliesBoundedDefaults asserts zero knobs resolve to bounded
// defaults rather than lumberjack's "retain everything" behaviour (it keeps
// every backup when MaxBackups and MaxAge are both zero).
func TestOpenFile_AppliesBoundedDefaults(t *testing.T) {
	dir := t.TempDir()
	d, err := OpenFile(FileConfig{Path: filepath.Join(dir, "x.log")})
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()
	lj, ok := d.Writer.(*lumberjack.Logger)
	if !ok {
		t.Fatalf("file destination writer is %T, want *lumberjack.Logger", d.Writer)
	}
	if lj.MaxSize != defaultMaxSizeMB || lj.MaxBackups != defaultMaxBackups || lj.MaxAge != defaultMaxAgeDays {
		t.Errorf("bounded defaults not applied: MaxSize=%d MaxBackups=%d MaxAge=%d",
			lj.MaxSize, lj.MaxBackups, lj.MaxAge)
	}
	if lj.MaxBackups == 0 && lj.MaxAge == 0 {
		t.Error("both retention knobs zero => unbounded backups; footprint not bounded")
	}
}

// TestOpenFile_Bounded drives the active file past a 1 MB size cap and asserts
// rotation happened (a backup appears; the active file is rolled back down), so
// the footprint stays bounded rather than growing without limit.
func TestOpenFile_Bounded(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bounded.log")
	d, err := OpenFile(FileConfig{Path: path, MaxSizeMB: 1, MaxBackups: 3, MaxAgeDays: 1})
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer d.Close()

	// Write ~2 MB so lumberjack rolls at the 1 MB cap at least once.
	chunk := make([]byte, 64*1024)
	for i := range chunk {
		chunk[i] = 'a'
	}
	for written := 0; written < 2<<20; written += len(chunk) {
		if _, err := d.Writer.Write(chunk); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("expected a rotated backup beside the active file, found %d entries", len(entries))
	}
	// The active file must be rolled back below the cap, not growing forever.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat active: %v", err)
	}
	if fi.Size() > 1<<20 {
		t.Errorf("active file %d bytes exceeds the 1 MB cap; not bounded", fi.Size())
	}
}

// TestOpenFile_FailLoud asserts an unopenable destination is a hard error, not a
// silent fall-back to stderr. The parent path component is a regular file, so
// MkdirAll/open fails with ENOTDIR.
func TestOpenFile_FailLoud(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "regular")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, err := OpenFile(FileConfig{Path: filepath.Join(notADir, "sub", "log")})
	if err == nil {
		t.Fatal("expected fail-loud error for an unopenable destination, got nil")
	}
	if !strings.Contains(err.Error(), "log file destination") {
		t.Errorf("error should name the destination, got %v", err)
	}
}

// fakeRotator records Rotate calls for the time-trigger test.
type fakeRotator struct {
	mu    sync.Mutex
	count int
}

func (f *fakeRotator) Rotate() error {
	f.mu.Lock()
	f.count++
	f.mu.Unlock()
	return nil
}

func (f *fakeRotator) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.count
}

// TestRunRotation_TriggersOnTick exercises the at-least-daily time-trigger with
// an injected clock (a channel we control): a tick rotates; cancelling ctx
// stops the loop. This proves rotation happens with NO size pressure — the
// default-must-rotate-daily requirement.
func TestRunRotation_TriggersOnTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tick := make(chan time.Time)
	r := &fakeRotator{}
	done := make(chan struct{})
	go func() {
		runRotation(ctx, tick, r, nil)
		close(done)
	}()

	// Two simulated day boundaries → two rotations, no bytes written.
	tick <- time.Unix(0, 0)
	tick <- time.Unix(0, 0)
	// Allow the second Rotate to land before asserting.
	deadline := time.Now().Add(time.Second)
	for r.calls() < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := r.calls(); got != 2 {
		t.Errorf("Rotate called %d times, want 2", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runRotation did not return after ctx cancel")
	}
}
