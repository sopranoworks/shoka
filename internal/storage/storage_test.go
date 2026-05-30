package storage

import (
	"testing"
)

func TestPathTraversalAndAbsoluteRejected(t *testing.T) {
	s := newTestStorage(t) // helper from version_test.go (same package)
	bad := []string{
		"../outside.md",
		"../../etc/passwd",
		"sub/../../escape.md",
		"/etc/passwd",
		"/abs.md",
	}
	for _, p := range bad {
		if _, err := s.WriteFileVersioned("ns", "proj", p, "x", ""); err == nil {
			t.Errorf("WriteFileVersioned(%q): expected error, got nil", p)
		}
		if _, err := s.ReadFile("ns", "proj", p); err == nil {
			t.Errorf("ReadFile(%q): expected error, got nil", p)
		}
		if err := s.DeleteFile("ns", "proj", p); err == nil {
			t.Errorf("DeleteFile(%q): expected error, got nil", p)
		}
		if _, err := s.GetCurrentVersion("ns", "proj", p); err == nil {
			t.Errorf("GetCurrentVersion(%q): expected error, got nil", p)
		}
	}
}

func TestPathNormalization(t *testing.T) {
	s := newTestStorage(t)
	if _, err := s.WriteFileVersioned("ns", "proj", "./sub/note.md", "hi", ""); err != nil {
		t.Fatalf("write with ./ prefix should be accepted: %v", err)
	}
	content, err := s.ReadFile("ns", "proj", "sub/note.md")
	if err != nil {
		t.Fatalf("read normalized path: %v", err)
	}
	if content != "hi" {
		t.Fatalf("content = %q, want hi", content)
	}
}

func TestFailedWriteLeavesNoCommit(t *testing.T) {
	s := newTestStorage(t)
	if _, err := s.WriteFileVersioned("ns", "proj", "a.md", "v1", ""); err != nil {
		t.Fatal(err)
	}
	// An invalid write must not create a commit.
	if _, err := s.WriteFileVersioned("ns", "proj", "../escape.md", "bad", ""); err == nil {
		t.Fatal("expected invalid write to fail")
	}
	drain(t, s) // commits are asynchronous; wait for the valid write to land
	hist, err := s.GetHistory("ns", "proj", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 {
		t.Fatalf("expected exactly 1 commit after one valid + one failed write, got %d", len(hist))
	}
}

func TestHistoryRetrievableByHash(t *testing.T) {
	s := newTestStorage(t)
	for _, v := range []string{"v1", "v2", "v3"} {
		if _, err := s.WriteFileVersioned("ns", "proj", "h.md", v, ""); err != nil {
			t.Fatal(err)
		}
	}
	drain(t, s) // commits are asynchronous; wait for all three to land
	hist, err := s.GetHistory("ns", "proj", "h.md", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 3 {
		t.Fatalf("expected 3 commits, got %d", len(hist))
	}
	seen := map[string]bool{}
	for _, c := range hist {
		if seen[c.Hash] {
			t.Fatalf("duplicate commit hash %s", c.Hash)
		}
		seen[c.Hash] = true
	}
	// Oldest commit holds v1.
	oldest := hist[len(hist)-1].Hash
	content, err := s.ReadFileAtVersion("ns", "proj", "h.md", oldest)
	if err != nil {
		t.Fatalf("read at version %s: %v", oldest, err)
	}
	if content != "v1" {
		t.Fatalf("content at oldest = %q, want v1", content)
	}
}
