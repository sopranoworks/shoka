package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// The 2026-06-05 M3 metrics directive: lost+found worker counters. These pin the
// three families' increment sites — the sweep-pass counter (once per pass, not per
// file), the disposed/moved action split, and the corrupted/dangerous skip split —
// and confirm there is no "quarantined" action (the sweep never quarantines).

func TestLostFoundMetrics_SweepCounterCountsPasses(t *testing.T) {
	s, _ := newWorkerStorage(t)
	if got := s.LostFoundSweeps(); got != 0 {
		t.Fatalf("expected 0 sweeps before any pass, got %d", got)
	}
	// Each sweepAllProjects pass increments once, regardless of whether it acts.
	s.sweepAllProjects()
	s.sweepAllProjects()
	if got := s.LostFoundSweeps(); got != 2 {
		t.Fatalf("expected 2 sweeps after two passes, got %d", got)
	}
}

func TestLostFoundMetrics_DisposedAndMovedCounted(t *testing.T) {
	s, _ := newWorkerStorage(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, "", "ns", "proj", "real.md", "content", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	projectPath := filepath.Join(s.baseDir, "ns", "proj")
	// One disposable (deleted) + one non-disposable (moved) + the tracked real.md.
	if err := os.WriteFile(filepath.Join(projectPath, ".DS_Store"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "mystery.md"), []byte("unknown"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDisposable(t, filepath.Join(s.baseDir, "shoka.disposable"), ".DS_Store")

	s.sweepProject("ns", "proj")

	disposed, moved := s.LostFoundActions()
	if disposed != 1 || moved != 1 {
		t.Fatalf("expected disposed=1 moved=1 (tracked file not counted), got disposed=%d moved=%d", disposed, moved)
	}
	// A healthy project is never a skip.
	if corrupted, dangerous := s.LostFoundProjectsSkipped(); corrupted != 0 || dangerous != 0 {
		t.Fatalf("healthy project must not be a skip, got corrupted=%d dangerous=%d", corrupted, dangerous)
	}
}

func TestLostFoundMetrics_SkipCorruptedCounted(t *testing.T) {
	s, _ := newWorkerStorage(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, "", "ns", "proj", "real.md", "v1", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	projectPath := filepath.Join(s.baseDir, "ns", "proj")
	// Hand-edit a tracked file -> Modified drift -> StateCorrupted.
	if err := os.WriteFile(filepath.Join(projectPath, "real.md"), []byte("hand-edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Untracked junk present too — it must NOT be acted on while skipped.
	if err := os.WriteFile(filepath.Join(projectPath, "mystery.md"), []byte("unknown"), 0o644); err != nil {
		t.Fatal(err)
	}

	s.sweepProject("ns", "proj")

	corrupted, dangerous := s.LostFoundProjectsSkipped()
	if corrupted != 1 || dangerous != 0 {
		t.Fatalf("expected corrupted=1 dangerous=0, got corrupted=%d dangerous=%d", corrupted, dangerous)
	}
	if disposed, moved := s.LostFoundActions(); disposed != 0 || moved != 0 {
		t.Fatalf("a skipped project must take no action, got disposed=%d moved=%d", disposed, moved)
	}
}

func TestLostFoundMetrics_SkipDangerousCounted(t *testing.T) {
	s, _ := newWorkerStorage(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, "", "ns", "proj", "real.md", "v1", nil); err != nil {
		t.Fatal(err)
	}
	drain(t, s)

	// Make the repo metadata unreadable by moving it out of the project entirely
	// (mirrors TestStore_DangerousStateAndRecovery) -> StateDangerous.
	repoMeta := filepath.Join(s.baseDir, "ns", "proj", ".git")
	if err := os.Rename(repoMeta, filepath.Join(s.baseDir, "repo-meta-moved-away")); err != nil {
		t.Fatal(err)
	}

	s.sweepProject("ns", "proj")

	corrupted, dangerous := s.LostFoundProjectsSkipped()
	if corrupted != 0 || dangerous != 1 {
		t.Fatalf("expected corrupted=0 dangerous=1, got corrupted=%d dangerous=%d", corrupted, dangerous)
	}
}
