package tools

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/storage"
)

// wireToolText extracts the concatenated text of a tool result's content (for
// failure messages).
func wireToolText(res *mcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	var s string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			s += tc.Text
		}
	}
	return s
}

func newDiffStorage(t *testing.T) *storage.FSGitStorage {
	t.Helper()
	dir, err := os.MkdirTemp("", "shoka-tools-diff-*")
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

func writeDrain(t *testing.T, s *storage.FSGitStorage, path, content string) {
	t.Helper()
	if _, err := s.Write(context.Background(), "", "ns", "proj", path, content, nil); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatalf("WAL did not drain after writing %s", path)
	}
}

func deleteDrain(t *testing.T, s *storage.FSGitStorage, path string) {
	t.Helper()
	if err := s.Delete(context.Background(), "", "ns", "proj", path, nil); err != nil {
		t.Fatalf("delete %s: %v", path, err)
	}
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatalf("WAL did not drain after deleting %s", path)
	}
}

// commitHashes returns the commit hashes for path (newest first), via the
// storage history API only — the tools package must not import go-git (Anchor 1).
func commitHashes(t *testing.T, s *storage.FSGitStorage, path string) []string {
	t.Helper()
	hist, err := s.GetHistory("ns", "proj", path, 0)
	if err != nil {
		t.Fatalf("GetHistory(%q): %v", path, err)
	}
	out := make([]string, len(hist))
	for i, c := range hist {
		out[i] = c.Hash
	}
	return out
}

// TestGetDiffHandler_Modified — happy path: a file committed twice, diffed
// oldest→newest, returns status="modified" with the expected hunk lines.
func TestGetDiffHandler_Modified(t *testing.T) {
	s := newDiffStorage(t)
	writeDrain(t, s, "doc.md", "line1\nline2\n")
	writeDrain(t, s, "doc.md", "line1\nCHANGED\n")
	h := commitHashes(t, s, "doc.md") // [newest, oldest]
	if len(h) != 2 {
		t.Fatalf("expected 2 commits, got %d", len(h))
	}

	res, out, err := GetDiffHandler(s)(context.Background(), nil, GetDiffInput{
		Namespace: "ns", ProjectName: "proj", Path: "doc.md", FromHash: h[1], ToHash: h[0],
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res != nil && res.IsError {
		t.Fatalf("unexpected error result: %s", wireToolText(res))
	}
	if out.Diff.Status != "modified" {
		t.Fatalf("status = %q, want modified", out.Diff.Status)
	}
	if out.Diff.Suppressed != "" {
		t.Fatalf("unexpectedly suppressed: %q", out.Diff.Suppressed)
	}
	var sawDel, sawAdd bool
	for _, hk := range out.Diff.Hunks {
		for _, l := range hk.Lines {
			if l.Op == "delete" && l.Text == "line2" {
				sawDel = true
			}
			if l.Op == "add" && l.Text == "CHANGED" {
				sawAdd = true
			}
		}
	}
	if !sawDel || !sawAdd {
		t.Fatalf("hunks missing expected change (delete line2 / add CHANGED): %+v", out.Diff.Hunks)
	}
}

// TestGetDiffHandler_Added — file present only on the to side ⇒ status "added".
func TestGetDiffHandler_Added(t *testing.T) {
	s := newDiffStorage(t)
	writeDrain(t, s, "other.md", "x\n") // commit where doc.md is absent
	writeDrain(t, s, "doc.md", "n1\nn2\n")
	all := commitHashes(t, s, "") // [doc.md commit, other.md commit]
	if len(all) != 2 {
		t.Fatalf("expected 2 commits, got %d", len(all))
	}

	_, out, err := GetDiffHandler(s)(context.Background(), nil, GetDiffInput{
		Namespace: "ns", ProjectName: "proj", Path: "doc.md", FromHash: all[1], ToHash: all[0],
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if out.Diff.Status != "added" {
		t.Fatalf("status = %q, want added", out.Diff.Status)
	}
}

// TestGetDiffHandler_Deleted — file present only on the from side ⇒ "deleted".
func TestGetDiffHandler_Deleted(t *testing.T) {
	s := newDiffStorage(t)
	writeDrain(t, s, "keep.md", "k\n") // leaves a non-empty tree after delete
	writeDrain(t, s, "doc.md", "d1\nd2\n")
	deleteDrain(t, s, "doc.md")
	all := commitHashes(t, s, "") // [delete-doc, write-doc, write-keep]
	if len(all) != 3 {
		t.Fatalf("expected 3 commits, got %d", len(all))
	}

	_, out, err := GetDiffHandler(s)(context.Background(), nil, GetDiffInput{
		Namespace: "ns", ProjectName: "proj", Path: "doc.md", FromHash: all[1], ToHash: all[0],
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if out.Diff.Status != "deleted" {
		t.Fatalf("status = %q, want deleted", out.Diff.Status)
	}
}

// TestGetDiffHandler_SuppressedBinary — a binary version ⇒ the tool conveys
// binary=true / suppressed="binary" (not an empty diff).
func TestGetDiffHandler_SuppressedBinary(t *testing.T) {
	s := newDiffStorage(t)
	writeDrain(t, s, "b.md", "before text\n")
	writeDrain(t, s, "b.md", "with a \x00 nul byte\n")
	h := commitHashes(t, s, "b.md")

	_, out, err := GetDiffHandler(s)(context.Background(), nil, GetDiffInput{
		Namespace: "ns", ProjectName: "proj", Path: "b.md", FromHash: h[1], ToHash: h[0],
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if !out.Diff.Binary || out.Diff.Suppressed != "binary" {
		t.Fatalf("binary suppression not conveyed: binary=%v suppressed=%q", out.Diff.Binary, out.Diff.Suppressed)
	}
	if len(out.Diff.Hunks) != 0 {
		t.Fatalf("binary diff should carry no hunks, got %d", len(out.Diff.Hunks))
	}
}

// TestGetDiffHandler_BadInput — a bad hash and a missing path each yield a clean
// tool error (IsError), no panic.
func TestGetDiffHandler_BadInput(t *testing.T) {
	s := newDiffStorage(t)
	writeDrain(t, s, "doc.md", "v1\n")
	h := commitHashes(t, s, "doc.md")
	const zero = "0000000000000000000000000000000000000000"

	// Bad 'from' hash → DiffVersions errors → IsError.
	res, _, err := GetDiffHandler(s)(context.Background(), nil, GetDiffInput{
		Namespace: "ns", ProjectName: "proj", Path: "doc.md", FromHash: zero, ToHash: h[0],
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("bad hash should yield an error tool result")
	}

	// Missing path → required-arg error.
	res2, _, err := GetDiffHandler(s)(context.Background(), nil, GetDiffInput{
		Namespace: "ns", ProjectName: "proj", Path: "", FromHash: h[0], ToHash: h[0],
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if res2 == nil || !res2.IsError {
		t.Fatalf("missing path should yield an error tool result")
	}
}
