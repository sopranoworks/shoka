package storage

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// #2 — registry lifecycle through FSGitStorage: CreateProject auto-registers its parent
// namespace and records the project; DeleteProject removes the project but the namespace
// record survives; CreateNamespace registers an empty namespace; DeleteNamespace deregisters.
func TestRegistry_LifecycleThroughStorage(t *testing.T) {
	s := newEmptyStorage(t)
	ctx := context.Background()

	// CreateProject auto-registers the (never-explicitly-created) parent namespace.
	if err := s.CreateProject("foo", "p1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListNamespaces(); !contains(got, "foo") {
		t.Fatalf("CreateProject must auto-register parent namespace foo: %v", got)
	}
	if rec, found, _ := s.nsReg.Get("foo"); !found || !reflect.DeepEqual(rec.Projects, []string{"p1"}) {
		t.Fatalf("foo record projects = %v (found=%v), want [p1]", rec.Projects, found)
	}

	// A second project is added to the same namespace record.
	if err := s.CreateProject("foo", "p2"); err != nil {
		t.Fatal(err)
	}
	if rec, _, _ := s.nsReg.Get("foo"); !reflect.DeepEqual(rec.Projects, []string{"p1", "p2"}) {
		t.Fatalf("foo record projects = %v, want [p1 p2]", rec.Projects)
	}

	// DeleteProject removes the project from the record; the namespace survives.
	if err := s.DeleteProject(ctx, "foo", "p1"); err != nil {
		t.Fatal(err)
	}
	if rec, found, _ := s.nsReg.Get("foo"); !found || !reflect.DeepEqual(rec.Projects, []string{"p2"}) {
		t.Fatalf("after delete: foo projects = %v (found=%v), want [p2]", rec.Projects, found)
	}
	if got, _ := s.ListNamespaces(); !contains(got, "foo") {
		t.Fatalf("foo must survive a project delete: %v", got)
	}

	// CreateNamespace registers an empty managed namespace; DeleteNamespace deregisters.
	if err := s.CreateNamespace("bar"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListNamespaces(); !contains(got, "bar") {
		t.Fatalf("CreateNamespace must register bar: %v", got)
	}
	if err := s.DeleteNamespace(ctx, "bar"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListNamespaces(); contains(got, "bar") {
		t.Fatalf("DeleteNamespace must deregister bar: %v", got)
	}
}

// #3 — the default namespace is always managed (registered at StartupInit) and is
// delete-protected.
func TestRegistry_DefaultAlwaysManaged(t *testing.T) {
	s := newEmptyStorage(t)
	ctx := context.Background()
	s.StartupInit(ctx)

	if got, _ := s.ListNamespaces(); !contains(got, "default") {
		t.Fatalf("default must be managed after StartupInit: %v", got)
	}
	// Delete-protected: DeleteNamespace("default") is refused and default remains.
	if err := s.DeleteNamespace(ctx, "default"); err == nil {
		t.Fatal("DeleteNamespace(default) must be refused")
	}
	if got, _ := s.ListNamespaces(); !contains(got, "default") {
		t.Fatalf("default must remain managed after a refused delete: %v", got)
	}
}

// #4 (core) — the one-time rescue-adopt migration: a deployment with real .git projects
// but NO registry adopts them on the first StartupInit; default is ensured; repo-less /
// empty dirs are NOT adopted; re-running does not re-run the rescue or double-register.
func TestRegistry_RescueMigration(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	// First instance: create real projects under two namespaces (these register them).
	s1, err := NewFSGitStorageWithOptions(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct{ ns, name string }{{"alpha", "a1"}, {"alpha", "a2"}, {"beta", "b1"}} {
		if cerr := s1.CreateProject(p.ns, p.name); cerr != nil {
			t.Fatal(cerr)
		}
	}
	if cerr := s1.Close(); cerr != nil {
		t.Fatal(cerr)
	}

	// Simulate a pre-managed deployment: remove the registry so the on-disk projects exist
	// with NO managed info. Also drop in a repo-less leftover and an empty foreign dir that
	// must NOT be adopted.
	if rerr := os.Remove(filepath.Join(dir, "namespaces.db")); rerr != nil {
		t.Fatal(rerr)
	}
	if merr := os.MkdirAll(filepath.Join(dir, "alpha", "leftover"), 0o755); merr != nil { // repo-less
		t.Fatal(merr)
	}
	if merr := os.MkdirAll(filepath.Join(dir, "foreign"), 0o755); merr != nil { // empty, no projects
		t.Fatal(merr)
	}

	// Second instance: first StartupInit runs the rescue.
	s2, err := NewFSGitStorageWithOptions(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	s2.StartupInit(ctx)

	got, _ := s2.ListNamespaces()
	want := []string{"alpha", "beta", "default"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("after rescue ListNamespaces = %v, want %v (foreign not adopted, default present)", got, want)
	}
	if rec, _, _ := s2.nsReg.Get("alpha"); !reflect.DeepEqual(rec.Projects, []string{"a1", "a2"}) {
		t.Fatalf("alpha adopted projects = %v, want [a1 a2] (leftover must NOT be adopted)", rec.Projects)
	}
	if rec, _, _ := s2.nsReg.Get("beta"); !reflect.DeepEqual(rec.Projects, []string{"b1"}) {
		t.Fatalf("beta adopted projects = %v, want [b1]", rec.Projects)
	}

	// Idempotent: a second StartupInit does not re-run the rescue or change the set.
	s2.StartupInit(ctx)
	got2, _ := s2.ListNamespaces()
	if !reflect.DeepEqual(got2, want) {
		t.Fatalf("re-run rescue changed the managed set: %v, want %v", got2, want)
	}
	if rec, _, _ := s2.nsReg.Get("alpha"); !reflect.DeepEqual(rec.Projects, []string{"a1", "a2"}) {
		t.Fatalf("re-run double-registered alpha projects: %v", rec.Projects)
	}
}
