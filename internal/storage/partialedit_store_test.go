package storage

import (
	"context"
	"errors"
	"testing"
)

// These exercise AppendToFile / PatchFile through the REAL write path
// (writeTransformed): per-file lock, if_match, atomic write, WAL "write" entry,
// catalog, file.write NOTIFY — then read back via ReadFileWithETag and verify the
// committed git state by draining the WAL.

func TestAppendToFile_EndAppendsAndReturnsNewETag(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	base, err := s.Write(ctx, "sess", "ns", "proj", "journal.md", "entry-1\n", nil)
	if err != nil {
		t.Fatalf("seed write: %v", err)
	}

	etag, err := s.AppendToFile(ctx, "sess", "ns", "proj", "journal.md", "entry-2\n", "end", "", nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if etag == base {
		t.Fatalf("etag did not change after append")
	}

	got, readEtag, err := s.ReadFileWithETag("ns", "proj", "journal.md")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != "entry-1\nentry-2\n" {
		t.Fatalf("got %q, want %q", got, "entry-1\nentry-2\n")
	}
	if readEtag != etag {
		t.Fatalf("returned etag %q != read-back etag %q", etag, readEtag)
	}
}

func TestAppendToFile_BeforeAnchorInsertsUnderLock(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, "sess", "ns", "proj", "backlog.md",
		"## Items\n### B-01\n## Cross-cutting (non-items)\ntail\n", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.AppendToFile(ctx, "sess", "ns", "proj", "backlog.md",
		"### B-02\n", "before", "## Cross-cutting (non-items)", nil); err != nil {
		t.Fatalf("append before: %v", err)
	}
	got, _, _ := s.ReadFileWithETag("ns", "proj", "backlog.md")
	want := "## Items\n### B-01\n### B-02\n## Cross-cutting (non-items)\ntail\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestAppendToFile_AmbiguousAnchorErrorsAndDoesNotWrite(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	orig := "x\nx\n"
	base, _ := s.Write(ctx, "sess", "ns", "proj", "f.md", orig, nil)

	_, err := s.AppendToFile(ctx, "sess", "ns", "proj", "f.md", "Y", "after", "x", nil)
	var me *MatchError
	if !errors.As(err, &me) || me.Count != 2 {
		t.Fatalf("want ambiguous(2), got %v", err)
	}
	// The file must be untouched (no partial write).
	got, etag, _ := s.ReadFileWithETag("ns", "proj", "f.md")
	if got != orig || etag != base {
		t.Fatalf("file changed despite error: content=%q etag=%q", got, etag)
	}
}

func TestPatchFile_UniqueReplaceCommits(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, "sess", "ns", "proj", "b.md", "**status:** draft\nbody\n", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := s.PatchFile(ctx, "sess", "ns", "proj", "b.md", "**status:** draft", "**status:** active", nil); err != nil {
		t.Fatalf("patch: %v", err)
	}
	got, _, _ := s.ReadFileWithETag("ns", "proj", "b.md")
	if got != "**status:** active\nbody\n" {
		t.Fatalf("got %q", got)
	}
	// The write is a real commit: drain the WAL and confirm history exists.
	drain(t, s)
	hist, err := s.GetHistory("ns", "proj", "b.md", 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) < 2 {
		t.Fatalf("expected >=2 commits (seed + patch), got %d", len(hist))
	}
}

func TestPatchFile_NotFoundErrorsAndDoesNotWrite(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	orig := "hello world\n"
	base, _ := s.Write(ctx, "sess", "ns", "proj", "c.md", orig, nil)

	_, err := s.PatchFile(ctx, "sess", "ns", "proj", "c.md", "MISSING", "x", nil)
	var me *MatchError
	if !errors.As(err, &me) || me.Count != 0 {
		t.Fatalf("want not-found(0), got %v", err)
	}
	got, etag, _ := s.ReadFileWithETag("ns", "proj", "c.md")
	if got != orig || etag != base {
		t.Fatalf("file changed despite not-found: content=%q", got)
	}
}

func TestPatchFile_IfMatchMismatchIsConflict(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	if _, err := s.Write(ctx, "sess", "ns", "proj", "d.md", "one TWO three", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	stale := contentSHA("something-else")
	_, err := s.PatchFile(ctx, "sess", "ns", "proj", "d.md", "TWO", "2", &stale)
	var conflict *VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("want VersionConflictError, got %v", err)
	}
}

func TestAppendToFile_IfMatchMatchSucceeds(t *testing.T) {
	s := newTestStorage(t)
	ctx := context.Background()
	etag, _ := s.Write(ctx, "sess", "ns", "proj", "e.md", "a\n", nil)
	if _, err := s.AppendToFile(ctx, "sess", "ns", "proj", "e.md", "b\n", "end", "", &etag); err != nil {
		t.Fatalf("append with matching if_match: %v", err)
	}
	got, _, _ := s.ReadFileWithETag("ns", "proj", "e.md")
	if got != "a\nb\n" {
		t.Fatalf("got %q", got)
	}
}
