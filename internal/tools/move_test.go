package tools

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/storage"
)

func newMoveStorage(t *testing.T) (*storage.FSGitStorage, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "shoka-tools-move-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := storage.NewFSGitStorageWithOptions(dir, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func TestMoveFileHandler_Success(t *testing.T) {
	s, _ := newMoveStorage(t)
	if _, err := s.Write(context.Background(), "", "ns", "proj", "old.md", "# Old\n", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(context.Background(), "", "ns", "proj", "ref.md", "[x](old.md)\n", nil); err != nil {
		t.Fatal(err)
	}
	s.WaitForWAL(10 * time.Second)

	h := MoveFileHandler(s)
	res, out, err := h(context.Background(), nil, MoveFileInput{Namespace: "ns",
		ProjectName: "proj", SourcePath: "old.md", TargetPath: "new.md",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if out.NewETag == "" {
		t.Error("expected NewETag")
	}
	// Link auto-update on move is disabled (B-33): the ref.md referrer set up above
	// is deliberately NOT rewritten, so the count is always 0.
	if out.LinksRewritten != 0 {
		t.Errorf("LinksRewritten = %d, want 0 (link rewrite on move is disabled)", out.LinksRewritten)
	}
	// MCP move is agent-authored; that git-author guarantee is pinned inside the
	// storage submodule (storage.TestMove_MCPIsAgentAuthored) to keep go-git out of
	// this package (archlint Anchor 1). Here we assert the handler's success shape.
}

func TestMoveFileHandler_TargetExistsConflict(t *testing.T) {
	s, _ := newMoveStorage(t)
	if _, err := s.Write(context.Background(), "", "ns", "proj", "a.md", "AAA", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(context.Background(), "", "ns", "proj", "b.md", "BBB", nil); err != nil {
		t.Fatal(err)
	}
	s.WaitForWAL(10 * time.Second)

	h := MoveFileHandler(s)
	res, out, err := h(context.Background(), nil, MoveFileInput{Namespace: "ns",
		ProjectName: "proj", SourcePath: "a.md", TargetPath: "b.md",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("expected an error result for target-exists with no if_match")
	}
	if !out.Conflict || out.CurrentETag == "" {
		t.Errorf("want Conflict=true with CurrentETag, got %+v", out)
	}
}

func TestMoveFileHandler_Validation(t *testing.T) {
	s, _ := newMoveStorage(t)
	h := MoveFileHandler(s)
	res, _, err := h(context.Background(), nil, MoveFileInput{Namespace: "ns", ProjectName: "proj", SourcePath: "a.md"})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("missing target_path must be an error result")
	}
}
