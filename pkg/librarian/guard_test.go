package librarian

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRelWithin_Rejects(t *testing.T) {
	root := "/srv/corpus"
	reject := []string{
		"../etc/passwd",      // parent traversal
		"../../etc/passwd",   // deeper traversal
		"a/../../etc/passwd", // traversal after a real segment
		"/etc/passwd",        // absolute
		"sub/../../escape",   // net escape
	}
	for _, p := range reject {
		if _, _, err := relWithin(root, p); err == nil {
			t.Errorf("relWithin(%q) accepted an escaping path; want rejection", p)
		}
	}
}

func TestRelWithin_Accepts(t *testing.T) {
	root := "/srv/corpus"
	accept := map[string]string{
		"file.md":     "file.md",
		"a/b/c.txt":   "a/b/c.txt",
		"a/./b.txt":   "a/b.txt", // cleaned, still within
		".":           ".",
		"sub/../keep": "keep", // traversal that nets to within-root
	}
	for in, wantRel := range accept {
		_, rel, err := relWithin(root, in)
		if err != nil {
			t.Errorf("relWithin(%q) rejected an in-root path: %v", in, err)
			continue
		}
		if rel != wantRel {
			t.Errorf("relWithin(%q) rel = %q, want %q", in, rel, wantRel)
		}
	}
}

// TestGuard_Resolve covers the full kernel: in-root accepted, escape rejected,
// ignored rejected, and a symlink entry SKIPPED (rejected) — never resolved.
func TestGuard_Resolve(t *testing.T) {
	root := t.TempDir()
	// Lay out a small corpus.
	writeFile(t, filepath.Join(root, "doc.md"), "hello")
	mkdir(t, filepath.Join(root, "sub"))
	writeFile(t, filepath.Join(root, "sub", "nested.md"), "nested")
	mkdir(t, filepath.Join(root, ".git"))
	writeFile(t, filepath.Join(root, ".git", "config"), "[core]")
	writeFile(t, filepath.Join(root, ".shoka.disposable"), "patterns")

	g := NewGuard(root, []string{".shoka*"})

	// Accepted: real, in-root, non-ignored, non-symlink.
	if _, rel, err := g.Resolve("doc.md", false); err != nil || rel != "doc.md" {
		t.Errorf("Resolve(doc.md) = (%q, %v), want (doc.md, nil)", rel, err)
	}
	if _, rel, err := g.Resolve("sub/nested.md", false); err != nil || rel != "sub/nested.md" {
		t.Errorf("Resolve(sub/nested.md) = (%q, %v), want (sub/nested.md, nil)", rel, err)
	}
	if _, _, err := g.Resolve("sub", true); err != nil {
		t.Errorf("Resolve(sub, dir) rejected an in-root dir: %v", err)
	}

	// Rejected: escape.
	if _, _, err := g.Resolve("../outside", false); err == nil {
		t.Errorf("Resolve(../outside) accepted an escaping path")
	}
	// Rejected: ignored (.git and injected .shoka*).
	if _, _, err := g.Resolve(".git/config", false); err == nil {
		t.Errorf("Resolve(.git/config) accepted an ignored path")
	}
	if _, _, err := g.Resolve(".shoka.disposable", false); err == nil {
		t.Errorf("Resolve(.shoka.disposable) accepted an ignored path")
	}
}

func TestGuard_SymlinkSkipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.txt"), "TOP SECRET")

	// A symlink leaf pointing outside root.
	linkLeaf := filepath.Join(root, "leak.txt")
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), linkLeaf); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// A symlinked directory pointing outside root, with a file "under" it.
	linkDir := filepath.Join(root, "escape")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Fatalf("symlink dir: %v", err)
	}

	g := NewGuard(root, nil)

	if _, _, err := g.Resolve("leak.txt", false); err == nil {
		t.Errorf("Resolve(leak.txt) followed a symlink leaf; want skip/reject")
	}
	// Reading THROUGH a symlinked intermediate dir must also be refused.
	if _, _, err := g.Resolve("escape/secret.txt", false); err == nil {
		t.Errorf("Resolve(escape/secret.txt) followed a symlinked dir; want skip/reject")
	}
	// Sanity: isSymlink reports the link, not its target.
	sym, err := isSymlink(linkLeaf)
	if err != nil || !sym {
		t.Errorf("isSymlink(link) = (%v, %v), want (true, nil)", sym, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}
