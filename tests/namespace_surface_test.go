package tests

import (
	"context"
	"os"
	"reflect"
	"testing"

	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/internal/tools"
)

// TestListProjects_CrossNamespace covers the namespace-surface directive
// (2026-05-29): list_projects with no namespace returns every project across
// all namespaces as "<ns>/<name>" strings (sorted); with an explicit namespace
// it returns only that namespace's projects, in the same string shape.
func TestListProjects_CrossNamespace(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "shoka-ns-surface-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	s, err := storage.NewFSGitStorage(tmpDir)
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}
	defer s.Close()

	if err := s.CreateProject("shoka", "maintenance"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject("rohrpost", "rohrpost-dev"); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	handler := tools.ListProjectsHandler(s)

	// No namespace argument → all namespaces, "<ns>/<name>", sorted.
	_, all, err := handler(ctx, nil, tools.ListProjectsInput{})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	wantAll := []string{"rohrpost/rohrpost-dev", "shoka/maintenance"}
	if !reflect.DeepEqual(all.Projects, wantAll) {
		t.Fatalf("list_projects() = %v, want %v", all.Projects, wantAll)
	}

	// Explicit namespace → only that namespace, same "<ns>/<name>" shape.
	_, scoped, err := handler(ctx, nil, tools.ListProjectsInput{Namespace: "rohrpost"})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	wantScoped := []string{"rohrpost/rohrpost-dev"}
	if !reflect.DeepEqual(scoped.Projects, wantScoped) {
		t.Fatalf("list_projects(namespace=rohrpost) = %v, want %v", scoped.Projects, wantScoped)
	}
}
