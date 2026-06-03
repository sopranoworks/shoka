package storage

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDiscoverProjects_SkipsDotPrefixedProjectDirs verifies that a dot-prefixed
// directory under a namespace (e.g. the .shoka-lostfound area) is NOT mistaken
// for a project. Without this guard the lost+found area would be discovered as a
// bogus project, fail git.PlainOpen, and be marked dangerous.
func TestDiscoverProjects_SkipsDotPrefixedProjectDirs(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSGitStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// A real, git-backed project and a Shoka-internal lost+found area, side by side.
	// proj must be a genuine project (CreateProject git-inits); a bare directory
	// without .git is leftover, not a project, and discovery rightly skips it (B-37).
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "ns", ".shoka-lostfound", "proj", "20260602T115550Z"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := s.discoverProjects()
	for _, p := range got {
		if p.name == ".shoka-lostfound" {
			t.Fatalf("discoverProjects must skip dot-prefixed project dirs, got %+v", got)
		}
	}
	if len(got) != 1 || got[0].namespace != "ns" || got[0].name != "proj" {
		t.Fatalf("expected exactly {ns, proj}, got %+v", got)
	}
}
