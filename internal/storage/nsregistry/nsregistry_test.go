package nsregistry

import (
	"path/filepath"
	"reflect"
	"testing"
)

func open(t *testing.T) *Registry {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "namespaces.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func TestRegistry_NamespaceLifecycle(t *testing.T) {
	r := open(t)

	if empty, _ := r.IsEmpty(); !empty {
		t.Fatal("a fresh registry must be empty")
	}

	// EnsureNamespace registers; idempotent.
	if err := r.EnsureNamespace("foo"); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureNamespace("foo"); err != nil {
		t.Fatalf("EnsureNamespace must be idempotent: %v", err)
	}
	if empty, _ := r.IsEmpty(); empty {
		t.Fatal("registry must be non-empty after a register")
	}
	if got, _ := r.List(); !reflect.DeepEqual(got, []string{"foo"}) {
		t.Fatalf("List = %v, want [foo]", got)
	}

	// RemoveNamespace deregisters; idempotent.
	if err := r.RemoveNamespace("foo"); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveNamespace("foo"); err != nil {
		t.Fatalf("RemoveNamespace must be idempotent: %v", err)
	}
	if got, _ := r.List(); len(got) != 0 {
		t.Fatalf("List = %v, want empty after remove", got)
	}
}

func TestRegistry_ProjectLifecycle(t *testing.T) {
	r := open(t)

	// AddProject auto-registers the parent namespace and dedupes.
	if err := r.AddProject("foo", "p1"); err != nil {
		t.Fatal(err)
	}
	if err := r.AddProject("foo", "p1"); err != nil {
		t.Fatalf("AddProject must dedupe without error: %v", err)
	}
	if err := r.AddProject("foo", "p2"); err != nil {
		t.Fatal(err)
	}
	rec, found, _ := r.Get("foo")
	if !found || !reflect.DeepEqual(rec.Projects, []string{"p1", "p2"}) {
		t.Fatalf("Get(foo).Projects = %v (found=%v), want [p1 p2]", rec.Projects, found)
	}
	if has, _ := r.HasProject("foo", "p1"); !has {
		t.Error("HasProject(foo,p1) must be true")
	}
	if has, _ := r.HasProject("foo", "nope"); has {
		t.Error("HasProject(foo,nope) must be false")
	}

	// RemoveProject removes the project but the namespace record survives.
	if err := r.RemoveProject("foo", "p1"); err != nil {
		t.Fatal(err)
	}
	rec, found, _ = r.Get("foo")
	if !found {
		t.Fatal("namespace must survive removing a project")
	}
	if !reflect.DeepEqual(rec.Projects, []string{"p2"}) {
		t.Fatalf("Projects after remove = %v, want [p2]", rec.Projects)
	}
	// Removing the last project leaves the namespace registered with an empty project set.
	if err := r.RemoveProject("foo", "p2"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := r.Get("foo"); !found {
		t.Fatal("namespace must survive removing its LAST project")
	}
	if got, _ := r.List(); !reflect.DeepEqual(got, []string{"foo"}) {
		t.Fatalf("List = %v, want [foo] (namespace survives last-project removal)", got)
	}
}

// TestRegistry_MoveReadiness proves the move seam (decision 6): a project can be moved
// from one namespace record to another with no global immutable identity blocking it, and
// the bare-name-within-namespace keying gives the exact target-collision check a future
// MoveProject needs (GitHub-repository-transfer no-overwrite rule).
func TestRegistry_MoveReadiness(t *testing.T) {
	r := open(t)
	if err := r.AddProject("src", "proj"); err != nil {
		t.Fatal(err)
	}
	// The move: remove from src, add to dst — only if dst has no same-named project.
	if has, _ := r.HasProject("dst", "proj"); has {
		t.Fatal("precondition: dst must not already have proj")
	}
	if err := r.RemoveProject("src", "proj"); err != nil {
		t.Fatal(err)
	}
	if err := r.AddProject("dst", "proj"); err != nil {
		t.Fatal(err)
	}
	if has, _ := r.HasProject("src", "proj"); has {
		t.Error("proj must be gone from src after move")
	}
	if has, _ := r.HasProject("dst", "proj"); !has {
		t.Error("proj must be present in dst after move")
	}
	// Collision check: a same-named project already in the target is detectable (so a
	// future MoveProject can REFUSE), confirming uniqueness-within-namespace is checkable.
	if err := r.AddProject("dst2", "proj"); err != nil {
		t.Fatal(err)
	}
	if has, _ := r.HasProject("dst2", "proj"); !has {
		t.Error("HasProject must detect an existing same-named project in the target namespace")
	}
}
