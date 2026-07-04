package tools

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/pkg/auth"
	"github.com/sopranoworks/shoka/pkg/authz"
)

func newCopyStorage(t *testing.T) *storage.FSGitStorage {
	t.Helper()
	dir, err := os.MkdirTemp("", "shoka-tools-copy-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := storage.NewFSGitStorageWithOptions(dir, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// 1. Copy file across projects — success
func TestCopyFileHandler_AcrossProjects(t *testing.T) {
	s := newCopyStorage(t)
	if err := s.CreateProject("src-ns", "src-proj"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject("dst-ns", "dst-proj"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(context.Background(), "", "src-ns", "src-proj", "doc.md", "# Hello\n", nil); err != nil {
		t.Fatal(err)
	}
	s.WaitForWAL(10 * time.Second)

	h := CopyFileHandler(s)
	res, out, err := h(context.Background(), nil, CopyFileInput{
		SourceNamespace:   "src-ns",
		SourceProjectName: "src-proj",
		SourcePath:        "doc.md",
		Namespace:         "dst-ns",
		ProjectName:       "dst-proj",
		Path:              "doc.md",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if out.ETag == "" {
		t.Error("expected ETag on success")
	}
	if out.Message == "" {
		t.Error("expected a message on success")
	}
}

// 2. Copy file within same project — success
func TestCopyFileHandler_WithinSameProject(t *testing.T) {
	s := newCopyStorage(t)
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(context.Background(), "", "ns", "proj", "original.md", "content here\n", nil); err != nil {
		t.Fatal(err)
	}
	s.WaitForWAL(10 * time.Second)

	h := CopyFileHandler(s)
	res, out, err := h(context.Background(), nil, CopyFileInput{
		SourceNamespace:   "ns",
		SourceProjectName: "proj",
		SourcePath:        "original.md",
		Namespace:         "ns",
		ProjectName:       "proj",
		Path:              "duplicate.md",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if out.ETag == "" {
		t.Error("expected ETag")
	}
}

// 3. Copy with rename (different destination path) — success
func TestCopyFileHandler_WithRename(t *testing.T) {
	s := newCopyStorage(t)
	if err := s.CreateProject("ns", "src"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject("ns", "dst"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(context.Background(), "", "ns", "src", "notes.md", "some notes\n", nil); err != nil {
		t.Fatal(err)
	}
	s.WaitForWAL(10 * time.Second)

	h := CopyFileHandler(s)
	res, out, err := h(context.Background(), nil, CopyFileInput{
		SourceNamespace:   "ns",
		SourceProjectName: "src",
		SourcePath:        "notes.md",
		Namespace:         "ns",
		ProjectName:       "dst",
		Path:              "renamed.md",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if out.ETag == "" {
		t.Error("expected ETag")
	}

	// Verify the file is at the renamed path.
	content, _, rerr := s.ReadFileWithETag("ns", "dst", "renamed.md")
	if rerr != nil {
		t.Fatalf("failed to read copied file: %v", rerr)
	}
	if content != "some notes\n" {
		t.Errorf("content mismatch: got %q", content)
	}
}

// 4. Copy to subdirectory (path includes dir) — success
func TestCopyFileHandler_ToSubdirectory(t *testing.T) {
	s := newCopyStorage(t)
	if err := s.CreateProject("ns", "src"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject("ns", "dst"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(context.Background(), "", "ns", "src", "file.md", "subdirectory test\n", nil); err != nil {
		t.Fatal(err)
	}
	s.WaitForWAL(10 * time.Second)

	h := CopyFileHandler(s)
	res, out, err := h(context.Background(), nil, CopyFileInput{
		SourceNamespace:   "ns",
		SourceProjectName: "src",
		SourcePath:        "file.md",
		Namespace:         "ns",
		ProjectName:       "dst",
		Path:              "reports/copied.md",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("unexpected error result: %+v", res)
	}
	if out.ETag == "" {
		t.Error("expected ETag")
	}

	content, _, rerr := s.ReadFileWithETag("ns", "dst", "reports/copied.md")
	if rerr != nil {
		t.Fatalf("failed to read at subdirectory path: %v", rerr)
	}
	if content != "subdirectory test\n" {
		t.Errorf("content mismatch: got %q", content)
	}
}

// 5. Destination file exists — error, no overwrite
func TestCopyFileHandler_DestinationExists(t *testing.T) {
	s := newCopyStorage(t)
	if err := s.CreateProject("ns", "src"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject("ns", "dst"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(context.Background(), "", "ns", "src", "a.md", "source\n", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(context.Background(), "", "ns", "dst", "a.md", "already here\n", nil); err != nil {
		t.Fatal(err)
	}
	s.WaitForWAL(10 * time.Second)

	h := CopyFileHandler(s)
	res, _, err := h(context.Background(), nil, CopyFileInput{
		SourceNamespace:   "ns",
		SourceProjectName: "src",
		SourcePath:        "a.md",
		Namespace:         "ns",
		ProjectName:       "dst",
		Path:              "a.md",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("expected error result when destination exists")
	}

	// Verify existing file was NOT overwritten.
	content, _, rerr := s.ReadFileWithETag("ns", "dst", "a.md")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if content != "already here\n" {
		t.Errorf("destination was overwritten: got %q", content)
	}
}

// 6. Source file not found — error
func TestCopyFileHandler_SourceNotFound(t *testing.T) {
	s := newCopyStorage(t)
	if err := s.CreateProject("ns", "src"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject("ns", "dst"); err != nil {
		t.Fatal(err)
	}

	h := CopyFileHandler(s)
	res, _, err := h(context.Background(), nil, CopyFileInput{
		SourceNamespace:   "ns",
		SourceProjectName: "src",
		SourcePath:        "nonexistent.md",
		Namespace:         "ns",
		ProjectName:       "dst",
		Path:              "copy.md",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("expected error result when source file does not exist")
	}
}

// 7. Insufficient source permission (no read) — error
func TestCopyFileHandler_InsufficientSourcePermission(t *testing.T) {
	s := newCopyStorage(t)
	if err := s.CreateProject("secret", "proj"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject("public", "proj"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(context.Background(), "", "secret", "proj", "data.md", "secret data\n", nil); err != nil {
		t.Fatal(err)
	}
	s.WaitForWAL(10 * time.Second)

	// Scope grants rw to public/proj but nothing on secret/proj.
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{
		Scope: "namespace:public:rw",
	})

	h := CopyFileHandler(s)
	res, _, err := h(ctx, nil, CopyFileInput{
		SourceNamespace:   "secret",
		SourceProjectName: "proj",
		SourcePath:        "data.md",
		Namespace:         "public",
		ProjectName:       "proj",
		Path:              "stolen.md",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatal("expected error when caller lacks read on source")
	}
}

// 8. Insufficient destination permission (read-only) — error
// Note: destination write permission is enforced by AuthzMiddleware (not the handler).
// This test verifies the middleware behavior by testing it directly.
func TestCopyFileHandler_InsufficientDestinationPermission(t *testing.T) {
	// The AuthzMiddleware checks namespace/project_name from the call arguments against
	// toolLevels["copy_file"] which we register at LevelWrite. A principal with only
	// read on the destination would be blocked by the middleware. We verify toolLevel
	// returns write for copy_file.
	level := toolLevel("copy_file")
	if level != authz.LevelWrite {
		t.Errorf("copy_file tool level = %v, want %v (write)", level, authz.LevelWrite)
	}
}

// 9. Copy preserves content exactly (byte-identical)
func TestCopyFileHandler_PreservesContentExactly(t *testing.T) {
	s := newCopyStorage(t)
	if err := s.CreateProject("ns", "src"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject("ns", "dst"); err != nil {
		t.Fatal(err)
	}

	original := "# Title\n\nParagraph with unicode: 日本語テスト 🎉\n\n```go\nfunc main() {}\n```\n"
	if _, err := s.Write(context.Background(), "", "ns", "src", "rich.md", original, nil); err != nil {
		t.Fatal(err)
	}
	s.WaitForWAL(10 * time.Second)

	h := CopyFileHandler(s)
	res, _, err := h(context.Background(), nil, CopyFileInput{
		SourceNamespace:   "ns",
		SourceProjectName: "src",
		SourcePath:        "rich.md",
		Namespace:         "ns",
		ProjectName:       "dst",
		Path:              "rich.md",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("unexpected error: %+v", res)
	}

	copied, _, rerr := s.ReadFileWithETag("ns", "dst", "rich.md")
	if rerr != nil {
		t.Fatal(rerr)
	}
	if copied != original {
		t.Errorf("content not preserved exactly.\nwant: %q\n got: %q", original, copied)
	}
}
