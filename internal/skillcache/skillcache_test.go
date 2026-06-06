package skillcache

import (
	"os"
	"path/filepath"
	"testing"
)

func mkfile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestListIdentifiesSkills: a top-level dir with SKILL.md is a skill; a plain
// file (README.md) and a dir without SKILL.md are not.
func TestListIdentifiesSkills(t *testing.T) {
	dir := t.TempDir()
	mkfile(t, filepath.Join(dir, "alpha", "SKILL.md"), "a")
	mkfile(t, filepath.Join(dir, "beta", "SKILL.md"), "b")
	mkfile(t, filepath.Join(dir, "notaskill", "other.txt"), "x")
	mkfile(t, filepath.Join(dir, "README.md"), "readme")

	names, err := List(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "alpha" || names[1] != "beta" {
		t.Fatalf("List = %v, want [alpha beta]", names)
	}
	if !Has(dir, "alpha") || Has(dir, "notaskill") || Has(dir, "README.md") {
		t.Fatal("Has misidentified a skill")
	}
}

// TestListMissingDir: a missing dir is empty, not an error.
func TestListMissingDir(t *testing.T) {
	names, err := List(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil || names != nil {
		t.Fatalf("List(missing) = %v, %v; want nil, nil", names, err)
	}
}

// TestDirHashDetectsChange: the hash is deterministic for identical content and
// changes when any file content, a file addition, or a file removal occurs —
// i.e. it hashes the whole directory, not just SKILL.md.
func TestDirHashDetectsChange(t *testing.T) {
	a := t.TempDir()
	mkfile(t, filepath.Join(a, "SKILL.md"), "content\n")
	mkfile(t, filepath.Join(a, "sub", "helper.txt"), "help\n")

	h1, err := DirHash(a)
	if err != nil {
		t.Fatal(err)
	}

	// Identical content in a different dir => identical hash (path-relative).
	b := t.TempDir()
	mkfile(t, filepath.Join(b, "SKILL.md"), "content\n")
	mkfile(t, filepath.Join(b, "sub", "helper.txt"), "help\n")
	h2, err := DirHash(b)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("identical content hashed differently: %s vs %s", h1, h2)
	}

	// Edit a supporting file => hash changes.
	mkfile(t, filepath.Join(b, "sub", "helper.txt"), "help CHANGED\n")
	h3, err := DirHash(b)
	if err != nil {
		t.Fatal(err)
	}
	if h3 == h1 {
		t.Fatal("editing a supporting file did not change the hash")
	}

	// Add a new file => hash changes.
	mkfile(t, filepath.Join(b, "extra.txt"), "new\n")
	h4, err := DirHash(b)
	if err != nil {
		t.Fatal(err)
	}
	if h4 == h3 {
		t.Fatal("adding a file did not change the hash")
	}
}

// TestCopySkillCleanReplace: copying replaces the destination wholesale — a file
// present in an older install but absent from the source must not survive.
func TestCopySkillCleanReplace(t *testing.T) {
	src := t.TempDir()
	skillSrc := filepath.Join(src, "demo")
	mkfile(t, filepath.Join(skillSrc, "SKILL.md"), "v2\n")
	mkfile(t, filepath.Join(skillSrc, "keep.txt"), "keep\n")

	destParent := t.TempDir()
	// Pre-existing stale install with a file the new source does not have.
	mkfile(t, filepath.Join(destParent, "demo", "stale.txt"), "stale\n")
	mkfile(t, filepath.Join(destParent, "demo", "SKILL.md"), "v1\n")

	dst, err := CopySkill(skillSrc, destParent)
	if err != nil {
		t.Fatal(err)
	}
	if dst != filepath.Join(destParent, "demo") {
		t.Fatalf("dst = %s", dst)
	}
	if _, err := os.Stat(filepath.Join(dst, "stale.txt")); !os.IsNotExist(err) {
		t.Fatal("stale file survived a clean replace")
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "SKILL.md")); string(b) != "v2\n" {
		t.Fatalf("SKILL.md not replaced: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "keep.txt")); string(b) != "keep\n" {
		t.Fatal("keep.txt not copied")
	}

	// The copied dir hashes equal to the source.
	hs, _ := DirHash(skillSrc)
	hd, _ := DirHash(dst)
	if hs != hd {
		t.Fatal("copied skill hash differs from source")
	}
}

// TestSyncRequiresRepo: Sync with no repo errors before touching anything.
func TestSyncRequiresRepo(t *testing.T) {
	if _, err := Sync(t.TempDir(), "", ""); err == nil {
		t.Fatal("Sync with empty repo must error")
	}
}
