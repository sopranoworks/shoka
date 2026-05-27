package storage

import (
	"errors"
	"os"
	"testing"
)

func newTestStorage(t *testing.T) *FSGitStorage {
	t.Helper()
	dir, err := os.MkdirTemp("", "shoka-version-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := NewFSGitStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestGetCurrentVersion_NoHistory(t *testing.T) {
	s := newTestStorage(t)
	v, err := s.GetCurrentVersion("ns", "proj", "missing.md")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "" {
		t.Fatalf("expected empty version for uncommitted file, got %q", v)
	}
}

func TestWriteFileVersioned_ReturnsHashAndTracksVersion(t *testing.T) {
	s := newTestStorage(t)
	h1, err := s.WriteFileVersioned("ns", "proj", "a.md", "v1", "")
	if err != nil {
		t.Fatalf("write v1: %v", err)
	}
	if h1 == "" {
		t.Fatal("expected non-empty commit hash")
	}

	cur, err := s.GetCurrentVersion("ns", "proj", "a.md")
	if err != nil {
		t.Fatal(err)
	}
	if cur != h1 {
		t.Fatalf("current version %q != written hash %q", cur, h1)
	}

	h2, err := s.WriteFileVersioned("ns", "proj", "a.md", "v2", "")
	if err != nil {
		t.Fatalf("write v2: %v", err)
	}
	if h2 == h1 {
		t.Fatal("second write should produce a new hash")
	}
}

func TestWriteFileVersioned_ConflictOnStaleExpected(t *testing.T) {
	s := newTestStorage(t)
	h1, _ := s.WriteFileVersioned("ns", "proj", "a.md", "v1", "")
	h2, _ := s.WriteFileVersioned("ns", "proj", "a.md", "v2", "") // current is now h2

	_, err := s.WriteFileVersioned("ns", "proj", "a.md", "v3", h1) // stale expected
	var conflict *VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected VersionConflictError, got %v", err)
	}
	if conflict.Current != h2 {
		t.Fatalf("conflict.Current = %q, want %q", conflict.Current, h2)
	}
	content, _ := s.ReadFile("ns", "proj", "a.md")
	if content != "v2" {
		t.Fatalf("content changed despite conflict: %q", content)
	}
}

func TestWriteFileVersioned_SucceedsWhenExpectedMatches(t *testing.T) {
	s := newTestStorage(t)
	h1, _ := s.WriteFileVersioned("ns", "proj", "a.md", "v1", "")
	h2, err := s.WriteFileVersioned("ns", "proj", "a.md", "v2", h1)
	if err != nil {
		t.Fatalf("expected success when expected matches, got %v", err)
	}
	if h2 == "" || h2 == h1 {
		t.Fatal("expected a new commit hash")
	}
}

func TestDeleteFileVersioned_ConflictOnStaleExpected(t *testing.T) {
	s := newTestStorage(t)
	h1, _ := s.WriteFileVersioned("ns", "proj", "a.md", "v1", "")
	s.WriteFileVersioned("ns", "proj", "a.md", "v2", "") // current is h2
	_, err := s.DeleteFileVersioned("ns", "proj", "a.md", h1)
	var conflict *VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected VersionConflictError on stale delete, got %v", err)
	}
	if _, err := s.ReadFile("ns", "proj", "a.md"); err != nil {
		t.Fatalf("file should still exist after conflicted delete: %v", err)
	}
}

func TestDeleteFileVersioned_SucceedsWhenExpectedMatches(t *testing.T) {
	s := newTestStorage(t)
	s.WriteFileVersioned("ns", "proj", "a.md", "v1", "")
	cur, _ := s.GetCurrentVersion("ns", "proj", "a.md")
	if _, err := s.DeleteFileVersioned("ns", "proj", "a.md", cur); err != nil {
		t.Fatalf("delete with matching version should succeed: %v", err)
	}
	if _, err := s.ReadFile("ns", "proj", "a.md"); err == nil {
		t.Fatal("file should be gone after delete")
	}
}
