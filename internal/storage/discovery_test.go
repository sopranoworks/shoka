package storage

import (
	"testing"
	"time"
)

func findChange(changes []FileChange, path string) (FileChange, bool) {
	for _, c := range changes {
		if c.Path == path {
			return c, true
		}
	}
	return FileChange{}, false
}

func TestListFilesSince_TimestampIncludesNewWrites(t *testing.T) {
	s := newTestStorage(t)
	before := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if _, err := s.WriteFileVersioned("ns", "proj", "a.md", "hello", ""); err != nil {
		t.Fatal(err)
	}
	drain(t, s) // commits are asynchronous
	changes, err := s.ListFilesSince("ns", "proj", before)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := findChange(changes, "a.md")
	if !ok {
		t.Fatalf("expected a.md in changes since %s, got %+v", before, changes)
	}
	if c.Kind != "added" {
		t.Fatalf("kind = %q, want added", c.Kind)
	}
	if c.Hash == "" {
		t.Fatal("expected a commit hash")
	}
}

func TestListFilesSince_HashIsExclusive(t *testing.T) {
	s := newTestStorage(t)
	// The write API now returns a content etag, not a commit hash; fetch the real
	// commit hash for a.md from the (drained) history to use as the 'since' bound.
	if _, err := s.WriteFileVersioned("ns", "proj", "a.md", "v1", ""); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	histA, err := s.GetHistory("ns", "proj", "a.md", 1)
	if err != nil || len(histA) != 1 {
		t.Fatalf("history for a.md: %v (%d entries)", err, len(histA))
	}
	h1 := histA[0].Hash
	if _, err := s.WriteFileVersioned("ns", "proj", "b.md", "v1", ""); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	changes, err := s.ListFilesSince("ns", "proj", h1)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findChange(changes, "b.md"); !ok {
		t.Fatalf("expected b.md (committed after h1), got %+v", changes)
	}
	if _, ok := findChange(changes, "a.md"); ok {
		t.Fatalf("a.md committed at h1 must be excluded, got %+v", changes)
	}
}

func TestListFilesSince_ReportsDeletion(t *testing.T) {
	s := newTestStorage(t)
	before := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	s.WriteFileVersioned("ns", "proj", "a.md", "v1", "")
	drain(t, s)
	if _, err := s.DeleteFileVersioned("ns", "proj", "a.md", ""); err != nil {
		t.Fatal(err)
	}
	drain(t, s)
	changes, err := s.ListFilesSince("ns", "proj", before)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := findChange(changes, "a.md")
	if !ok {
		t.Fatalf("expected a.md in changes, got %+v", changes)
	}
	if c.Kind != "deleted" {
		t.Fatalf("kind = %q, want deleted", c.Kind)
	}
}

func TestSearchFiles_Content(t *testing.T) {
	s := newTestStorage(t)
	s.WriteFileVersioned("ns", "proj", "doc1.md", "hello world foo", "")
	s.WriteFileVersioned("ns", "proj", "notes.txt", "bar baz", "")

	matches, err := s.SearchFiles("ns", "proj", "world", "content")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 || matches[0].Path != "doc1.md" {
		t.Fatalf("expected one content match in doc1.md, got %+v", matches)
	}
	if matches[0].Snippet == "" {
		t.Fatal("expected a snippet for a content match")
	}
}

func TestSearchFiles_FilenameCaseInsensitive(t *testing.T) {
	s := newTestStorage(t)
	s.WriteFileVersioned("ns", "proj", "Notes.md", "anything", "")

	matches, err := s.SearchFiles("ns", "proj", "notes", "filename")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findMatch(matches, "Notes.md"); !ok {
		t.Fatalf("expected filename match for Notes.md, got %+v", matches)
	}
}

func TestSearchFiles_NoMatch(t *testing.T) {
	s := newTestStorage(t)
	s.WriteFileVersioned("ns", "proj", "a.md", "content", "")
	matches, err := s.SearchFiles("ns", "proj", "zzzzz", "both")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected no matches, got %+v", matches)
	}
}

func findMatch(matches []SearchMatch, path string) (SearchMatch, bool) {
	for _, m := range matches {
		if m.Path == path {
			return m, true
		}
	}
	return SearchMatch{}, false
}
