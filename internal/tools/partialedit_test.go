package tools

import (
	"context"
	"os"
	"testing"

	"github.com/sopranoworks/shoka/internal/storage"
)

func newPartialEditStorage(t *testing.T) *storage.FSGitStorage {
	t.Helper()
	dir, err := os.MkdirTemp("", "shoka-tools-partial-*")
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
	return s
}

// --- append_to_file ---

func TestAppendToFileHandler_EndSuccess(t *testing.T) {
	s := newPartialEditStorage(t)
	if _, err := s.Write(context.Background(), "", "ns", "proj", "j.md", "a\n", nil); err != nil {
		t.Fatal(err)
	}
	h := AppendToFileHandler(s)
	res, out, err := h(context.Background(), nil, AppendToFileInput{
		Namespace: "ns", ProjectName: "proj", Path: "j.md", Content: "b\n",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if out.ETag == "" {
		t.Error("expected new etag")
	}
	got, _, _ := s.ReadFileWithETag("ns", "proj", "j.md")
	if got != "a\nb\n" {
		t.Fatalf("got %q, want %q", got, "a\nb\n")
	}
}

func TestAppendToFileHandler_AmbiguousAnchorIsError(t *testing.T) {
	s := newPartialEditStorage(t)
	if _, err := s.Write(context.Background(), "", "ns", "proj", "j.md", "x\nx\n", nil); err != nil {
		t.Fatal(err)
	}
	h := AppendToFileHandler(s)
	res, _, err := h(context.Background(), nil, AppendToFileInput{
		Namespace: "ns", ProjectName: "proj", Path: "j.md", Content: "Y", Position: "after", Anchor: "x",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("ambiguous anchor must be an error result")
	}
}

func TestAppendToFileHandler_MissingAnchorForBeforeIsError(t *testing.T) {
	s := newPartialEditStorage(t)
	if _, err := s.Write(context.Background(), "", "ns", "proj", "j.md", "abc", nil); err != nil {
		t.Fatal(err)
	}
	h := AppendToFileHandler(s)
	res, _, err := h(context.Background(), nil, AppendToFileInput{
		Namespace: "ns", ProjectName: "proj", Path: "j.md", Content: "x", Position: "before",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("position:before with no anchor must be an error result")
	}
}

func TestAppendToFileHandler_Validation(t *testing.T) {
	s := newPartialEditStorage(t)
	h := AppendToFileHandler(s)
	res, _, err := h(context.Background(), nil, AppendToFileInput{Namespace: "ns", ProjectName: "proj"})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("missing path must be an error result")
	}
}

// --- patch_file ---

func TestPatchFileHandler_Success(t *testing.T) {
	s := newPartialEditStorage(t)
	if _, err := s.Write(context.Background(), "", "ns", "proj", "b.md", "**status:** draft\n", nil); err != nil {
		t.Fatal(err)
	}
	h := PatchFileHandler(s)
	res, out, err := h(context.Background(), nil, PatchFileInput{
		Namespace: "ns", ProjectName: "proj", Path: "b.md",
		OldString: "**status:** draft", NewString: "**status:** active",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if out.ETag == "" {
		t.Error("expected new etag")
	}
	got, _, _ := s.ReadFileWithETag("ns", "proj", "b.md")
	if got != "**status:** active\n" {
		t.Fatalf("got %q", got)
	}
}

func TestPatchFileHandler_NotFoundIsError(t *testing.T) {
	s := newPartialEditStorage(t)
	if _, err := s.Write(context.Background(), "", "ns", "proj", "b.md", "hello\n", nil); err != nil {
		t.Fatal(err)
	}
	h := PatchFileHandler(s)
	res, _, err := h(context.Background(), nil, PatchFileInput{
		Namespace: "ns", ProjectName: "proj", Path: "b.md", OldString: "MISSING", NewString: "x",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("not-found old_string must be an error result")
	}
}

func TestPatchFileHandler_AmbiguousIsError(t *testing.T) {
	s := newPartialEditStorage(t)
	if _, err := s.Write(context.Background(), "", "ns", "proj", "b.md", "a a a\n", nil); err != nil {
		t.Fatal(err)
	}
	h := PatchFileHandler(s)
	res, _, err := h(context.Background(), nil, PatchFileInput{
		Namespace: "ns", ProjectName: "proj", Path: "b.md", OldString: "a", NewString: "Z",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("ambiguous old_string must be an error result")
	}
}

func TestPatchFileHandler_EmptyOldStringIsError(t *testing.T) {
	s := newPartialEditStorage(t)
	if _, err := s.Write(context.Background(), "", "ns", "proj", "b.md", "hi\n", nil); err != nil {
		t.Fatal(err)
	}
	h := PatchFileHandler(s)
	res, _, err := h(context.Background(), nil, PatchFileInput{
		Namespace: "ns", ProjectName: "proj", Path: "b.md", OldString: "", NewString: "x",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("empty old_string must be an error result")
	}
}

func TestPatchFileHandler_StaleIfMatchIsConflict(t *testing.T) {
	s := newPartialEditStorage(t)
	if _, err := s.Write(context.Background(), "", "ns", "proj", "b.md", "one TWO three", nil); err != nil {
		t.Fatal(err)
	}
	stale := "deadbeef"
	h := PatchFileHandler(s)
	res, out, err := h(context.Background(), nil, PatchFileInput{
		Namespace: "ns", ProjectName: "proj", Path: "b.md", OldString: "TWO", NewString: "2", IfMatch: &stale,
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("stale if_match must be an error result")
	}
	if !out.Conflict || out.CurrentETag == "" {
		t.Errorf("want Conflict=true with CurrentETag, got %+v", out)
	}
}
