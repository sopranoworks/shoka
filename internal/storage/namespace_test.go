package storage

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func newEmptyStorage(t *testing.T) *FSGitStorage {
	t.Helper()
	dir, err := os.MkdirTemp("", "shoka-ns-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := NewFSGitStorage(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestListAllProjects_CrossNamespaceSorted(t *testing.T) {
	s := newEmptyStorage(t)
	for _, p := range []struct{ ns, name string }{
		{"shoka", "maintenance"},
		{"rohrpost", "rohrpost-dev"},
		{"rohrpost", "another"},
	} {
		if err := s.CreateProject(p.ns, p.name); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.ListAllProjects()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"rohrpost/another", "rohrpost/rohrpost-dev", "shoka/maintenance"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListAllProjects() = %v, want %v", got, want)
	}
}

func TestListAllProjects_EmptyBaseDir(t *testing.T) {
	s := newEmptyStorage(t)
	got, err := s.ListAllProjects()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("ListAllProjects() = %v, want empty", got)
	}
}

func TestListNamespaces_IncludesEmpty(t *testing.T) {
	// B-28: a namespace is a first-class object — it is enumerated even with zero
	// projects (the inverse of the pre-B-28 "non-empty only" rule). A bare namespace
	// directory (here created directly; CreateNamespace does the same MkdirAll) is listed.
	s := newEmptyStorage(t)
	if err := s.CreateProject("shoka", "maintenance"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject("rohrpost", "rohrpost-dev"); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(s.baseDir, "emptyns"), 0755); err != nil {
		t.Fatal(err)
	}
	// A hidden Shoka-internal directory must still be excluded.
	if err := os.MkdirAll(filepath.Join(s.baseDir, ".shoka-internal"), 0755); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListNamespaces()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"emptyns", "rohrpost", "shoka"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListNamespaces() = %v, want %v (empty namespace included, hidden excluded)", got, want)
	}
}

func TestListNamespaces_EmptyBaseDir(t *testing.T) {
	s := newEmptyStorage(t)
	got, err := s.ListNamespaces()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("ListNamespaces() = %v, want empty", got)
	}
}
