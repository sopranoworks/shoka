package storage

import (
	"os"
	"path/filepath"
	"reflect"
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

	got, _ := s.discoverProjects()
	for _, p := range got {
		if p.name == ".shoka-lostfound" {
			t.Fatalf("discoverProjects must skip dot-prefixed project dirs, got %+v", got)
		}
	}
	if len(got) != 1 || got[0].namespace != "ns" || got[0].name != "proj" {
		t.Fatalf("expected exactly {ns, proj}, got %+v", got)
	}
}

// TestListProjects_ExcludesNonProjectEntries is the B-31 regression guard: the
// UI/MCP enumeration path (ListProjects / ListAllProjects) shares discoverProjects's
// single project-eligibility predicate, so a populated .shoka-lostfound area, the
// other dot-prefixed Shoka-internal dirs (.shoka/.drafts/.git), and a repo-less
// leftover dir are NEVER listed as projects. Before B-31, ListProjects returned every
// directory verbatim and surfaced .shoka-lostfound as a phantom "project" the load
// path then rejected ("invalid project name"). The ListProjects assertion below FAILS
// against the pre-fix code (which listed every entry.IsDir()).
func TestListProjects_ExcludesNonProjectEntries(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSGitStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// One real, git-backed project.
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatal(err)
	}
	// A populated per-project lost+found area, exactly as the worker lays it out.
	lf := filepath.Join(dir, "ns", ".shoka-lostfound", "proj", "20260614T120000Z")
	if err := os.MkdirAll(lf, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lf, "mystery.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The other dot-prefixed Shoka-internal dirs, plus a repo-less leftover dir
	// (a plain directory with no .git — the hasGitRepo half of the predicate).
	for _, d := range []string{".shoka", ".drafts", ".git", "orphan"} {
		if err := os.MkdirAll(filepath.Join(dir, "ns", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// ListProjects (the per-namespace path the UI/MCP use) must return ONLY the real
	// project — no dot-dirs, no repo-less leftover.
	projects, err := s.ListProjects("ns")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(projects, []string{"proj"}) {
		t.Fatalf("ListProjects(ns) = %v, want exactly [proj] (no .shoka-lostfound/.shoka/.drafts/.git/orphan)", projects)
	}

	// ListAllProjects (built on ListProjects) must agree.
	all, err := s.ListAllProjects()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(all, []string{"ns/proj"}) {
		t.Fatalf("ListAllProjects() = %v, want exactly [ns/proj]", all)
	}

	// discoverProjects (now sharing the same predicate) must agree.
	discovered, _ := s.discoverProjects()
	if len(discovered) != 1 || discovered[0].namespace != "ns" || discovered[0].name != "proj" {
		t.Fatalf("discoverProjects() = %+v, want exactly {ns, proj}", discovered)
	}
}
