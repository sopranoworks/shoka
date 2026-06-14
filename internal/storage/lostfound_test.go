package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/identity"
	"github.com/sopranoworks/shoka/internal/notify"
)

// newWorkerStorage builds storage with a notification center wired, plus an
// "ns/proj" project, for exercising the lost+found worker.
func newWorkerStorage(t *testing.T) (*FSGitStorage, *notify.Center) {
	t.Helper()
	dir := t.TempDir()
	c := notify.NewCenter(1000)
	s, err := NewFSGitStorageWithOptions(dir, Options{
		NotifyCenter: c,
		Identity: identity.Defaults{
			UserName:  "Osamu Takahashi",
			UserEmail: "forte.nit@gmail.com",
			AgentName: "shoka-agent",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatal(err)
	}
	return s, c
}

// lostFoundEvents returns the worker-emitted events of the given kind.
func lostFoundEvents(c *notify.Center, kind string) []notify.Event {
	var out []notify.Event
	for _, e := range c.Snapshot() {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestSweepProject_DisposableDeleted(t *testing.T) {
	s, c := newWorkerStorage(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, "", "ns", "proj", "real.md", "content", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	projectPath := filepath.Join(s.baseDir, "ns", "proj")
	if err := os.WriteFile(filepath.Join(projectPath, ".DS_Store"), []byte("\x00junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDisposable(t, filepath.Join(s.baseDir, "shoka.disposable"), ".DS_Store")

	s.sweepProject("ns", "proj")

	if _, err := os.Stat(filepath.Join(projectPath, ".DS_Store")); !os.IsNotExist(err) {
		t.Fatalf("expected .DS_Store deleted, stat err=%v", err)
	}
	// It must NOT have been preserved in lost+found.
	lf := s.lostFoundRoot("ns", "proj")
	if entries, _ := os.ReadDir(lf); len(entries) != 0 {
		t.Fatalf("disposable file should be deleted, not moved to lost+found: %v", entries)
	}
	disposed := lostFoundEvents(c, "lostfound.disposed")
	if len(disposed) != 1 || disposed[0].Target != "ns/proj" || disposed[0].Path != ".DS_Store" {
		t.Fatalf("expected one lostfound.disposed for .DS_Store, got %+v", disposed)
	}
}

func TestSweepProject_NonDisposableMovedToLostFound(t *testing.T) {
	s, c := newWorkerStorage(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, "", "ns", "proj", "real.md", "content", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	projectPath := filepath.Join(s.baseDir, "ns", "proj")
	if err := os.WriteFile(filepath.Join(projectPath, "mystery.md"), []byte("unknown"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No disposable patterns: mystery.md is preserved, not deleted.

	s.sweepProject("ns", "proj")

	if _, err := os.Stat(filepath.Join(projectPath, "mystery.md")); !os.IsNotExist(err) {
		t.Fatalf("expected mystery.md removed from working tree, stat err=%v", err)
	}
	moved := lostFoundEvents(c, "lostfound.moved")
	if len(moved) != 1 || moved[0].Target != "ns/proj" || moved[0].Path != "mystery.md" {
		t.Fatalf("expected one lostfound.moved for mystery.md, got %+v", moved)
	}
}

func TestSweepProject_TrackedFileUntouched(t *testing.T) {
	s, c := newWorkerStorage(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, "", "ns", "proj", "real.md", "keepme", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	s.sweepProject("ns", "proj")

	got, err := os.ReadFile(filepath.Join(s.baseDir, "ns", "proj", "real.md"))
	if err != nil || string(got) != "keepme" {
		t.Fatalf("tracked file must be untouched: content=%q err=%v", got, err)
	}
	if n := len(lostFoundEvents(c, "lostfound.moved")) + len(lostFoundEvents(c, "lostfound.disposed")); n != 0 {
		t.Fatalf("expected no worker events for a tracked-only tree, got %d", n)
	}
}

func TestSweepProject_SkipsCorruptedProject(t *testing.T) {
	s := newWorkerStorageNoCenter(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, "", "ns", "proj", "real.md", "v1", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	projectPath := filepath.Join(s.baseDir, "ns", "proj")
	// Corrupt: hand-edit a tracked file (Modified drift -> StateCorrupted).
	if err := os.WriteFile(filepath.Join(projectPath, "real.md"), []byte("hand-edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Untracked junk also present.
	if err := os.WriteFile(filepath.Join(projectPath, "mystery.md"), []byte("unknown"), 0o644); err != nil {
		t.Fatal(err)
	}

	s.sweepProject("ns", "proj")

	// The worker must NOT act on a corrupted project: the untracked file stays.
	if _, err := os.Stat(filepath.Join(projectPath, "mystery.md")); err != nil {
		t.Fatalf("worker must skip corrupted project; mystery.md should remain, err=%v", err)
	}
}

func newWorkerStorageNoCenter(t *testing.T) *FSGitStorage {
	t.Helper()
	s, _ := newWorkerStorage(t)
	return s
}

func TestStartLostFoundSweep_CleanShutdown(t *testing.T) {
	s, _ := newWorkerStorage(t)
	ctx, cancel := context.WithCancel(context.Background())
	s.StartLostFoundSweep(ctx, 5*time.Millisecond)
	time.Sleep(30 * time.Millisecond) // let a few ticks run
	cancel()
	time.Sleep(20 * time.Millisecond) // goroutine observes ctx.Done and returns
}

func TestStartLostFoundSweep_ZeroIntervalDisabled(t *testing.T) {
	s, _ := newWorkerStorage(t)
	// interval <= 0 must be a no-op (no goroutine, returns immediately).
	s.StartLostFoundSweep(context.Background(), 0)
}
