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

func TestListNamespaces_NonEmptyOnly(t *testing.T) {
	s := newEmptyStorage(t)
	if err := s.CreateProject("shoka", "maintenance"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject("rohrpost", "rohrpost-dev"); err != nil {
		t.Fatal(err)
	}
	// A namespace directory with no project subdirectories must not be listed.
	if err := os.MkdirAll(filepath.Join(s.baseDir, "emptyns"), 0755); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListNamespaces()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"rohrpost", "shoka"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListNamespaces() = %v, want %v (emptyns must be excluded)", got, want)
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
