package storage

import (
	"context"
	"errors"
	"testing"
)

// B-68 — write_file overwriting an existing path. The premise that write_file
// "fails on an existing path" was a mistaken assumption: Write (the MCP tool's
// entry point, internal/tools/file.go -> s.Write) overwrites an existing file,
// with if_match as the OPTIONAL optimistic-concurrency guard. git is the
// backstop — every overwrite is a commit whose parent holds the prior bytes, so
// a replacement is recoverable. This test pins all four cases the directive's
// verification names, through the exact Write(...,*string) signature the handler
// calls.
func TestWriteFile_OverwritesExistingPath_B68(t *testing.T) {
	s := newTestStorage(t) // git-inits ns/proj
	ctx := context.Background()
	const path = "doc.md"

	// (1) Create, then overwrite WITHOUT if_match — overwrite proceeds.
	etag1, err := s.Write(ctx, "sess", "ns", "proj", path, "v1\n", nil)
	if err != nil {
		t.Fatalf("initial write: %v", err)
	}
	etag2, err := s.Write(ctx, "sess", "ns", "proj", path, "v2\n", nil)
	if err != nil {
		t.Fatalf("overwrite without if_match should succeed, got: %v", err)
	}
	if etag2 == etag1 {
		t.Fatalf("etag did not change after overwrite")
	}
	if got, _, _ := s.ReadFileWithETag("ns", "proj", path); got != "v2\n" {
		t.Fatalf("after overwrite got %q, want %q", got, "v2\n")
	}

	// (2) Overwrite WITH a matching if_match — overwrite proceeds.
	etag3, err := s.Write(ctx, "sess", "ns", "proj", path, "v3\n", &etag2)
	if err != nil {
		t.Fatalf("overwrite with matching if_match should succeed, got: %v", err)
	}
	if got, _, _ := s.ReadFileWithETag("ns", "proj", path); got != "v3\n" {
		t.Fatalf("after matched overwrite got %q, want %q", got, "v3\n")
	}

	// (3) Overwrite with a STALE if_match — rejected; the file is untouched.
	_, err = s.Write(ctx, "sess", "ns", "proj", path, "v4\n", &etag1)
	var conflict *VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("stale if_match should be a *VersionConflictError, got: %v", err)
	}
	got, etagNow, _ := s.ReadFileWithETag("ns", "proj", path)
	if got != "v3\n" || etagNow != etag3 {
		t.Fatalf("stale-if_match write must not change the file: content=%q etag=%q", got, etagNow)
	}

	// (4) A write to a project that was never created still fails (B-37 guard
	// not regressed) — overwrite is enabled for real projects only.
	if _, err := s.Write(ctx, "sess", "ns", "nope", path, "x\n", nil); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("write to non-existent project should be ErrProjectNotFound, got: %v", err)
	}

	// (5) History-recoverable: each overwrite is its own commit, and the commit
	// before the latest holds the prior content — so a replacement is never lost.
	drain(t, s)
	hist, err := s.GetHistory("ns", "proj", path, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) < 3 {
		t.Fatalf("expected >=3 commits (v1, v2, v3), got %d", len(hist))
	}
	prior, err := s.ReadFileAtVersion("ns", "proj", path, hist[1].Hash)
	if err != nil {
		t.Fatalf("read prior version: %v", err)
	}
	if prior != "v2\n" {
		t.Fatalf("the commit before HEAD should hold the prior content %q, got %q", "v2\n", prior)
	}
}
