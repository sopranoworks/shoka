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

func TestRegistry_MoveProject(t *testing.T) {
	r := open(t)
	if err := r.AddProject("src", "proj"); err != nil {
		t.Fatal(err)
	}
	if err := r.EnsureNamespace("dst"); err != nil {
		t.Fatal(err)
	}

	if err := r.MoveProject("src", "proj", "dst"); err != nil {
		t.Fatalf("MoveProject: %v", err)
	}
	if has, _ := r.HasProject("src", "proj"); has {
		t.Error("proj must be gone from src after move")
	}
	if has, _ := r.HasProject("dst", "proj"); !has {
		t.Error("proj must be in dst after move")
	}

	// Idempotent recovery: re-running the (already-applied) move is a no-op success.
	if err := r.MoveProject("src", "proj", "dst"); err != nil {
		t.Fatalf("MoveProject (idempotent re-run) must not error: %v", err)
	}

	// No-overwrite: a genuine collision (both src and dst hold the name) is refused.
	if err := r.AddProject("a", "dup"); err != nil {
		t.Fatal(err)
	}
	if err := r.AddProject("b", "dup"); err != nil {
		t.Fatal(err)
	}
	if err := r.MoveProject("a", "dup", "b"); err == nil {
		t.Fatal("MoveProject must refuse when the target already has a project of that name")
	}
	if has, _ := r.HasProject("a", "dup"); !has {
		t.Error("a refused move must leave the source untouched")
	}
}

func TestRegistry_OpJournal(t *testing.T) {
	r := open(t)
	if _, found, _ := r.GetOpJournal(); found {
		t.Fatal("a fresh registry must have no op journal")
	}
	j := OpJournal{Op: "rename_namespace", OldNamespace: "src", NewNamespace: "dst", Phase: "dir_moved"}
	if err := r.SetOpJournal(j); err != nil {
		t.Fatal(err)
	}
	got, found, err := r.GetOpJournal()
	if err != nil || !found || got != j {
		t.Fatalf("GetOpJournal = (%+v, %v, %v), want %+v", got, found, err, j)
	}
	if err := r.ClearOpJournal(); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := r.GetOpJournal(); found {
		t.Fatal("ClearOpJournal must remove the entry")
	}
}

// TestRegistry_MoveReadiness proves the move seam (decision 6): a project can be moved
// from one namespace record to another with no global immutable identity blocking it, and
// the bare-name-within-namespace keying gives the exact target-collision check a future
// MoveProject needs (GitHub-repository-transfer no-overwrite rule).
func TestRegistry_RenameProject(t *testing.T) {
	r := open(t)
	if err := r.AddProject("ns", "old"); err != nil {
		t.Fatal(err)
	}
	if err := r.RenameProject("ns", "old", "new"); err != nil {
		t.Fatalf("RenameProject: %v", err)
	}
	if has, _ := r.HasProject("ns", "old"); has {
		t.Error("old name must be gone after rename")
	}
	if has, _ := r.HasProject("ns", "new"); !has {
		t.Error("new name must be present after rename")
	}
	// Idempotent recovery: re-running the applied rename is a no-op success.
	if err := r.RenameProject("ns", "old", "new"); err != nil {
		t.Fatalf("RenameProject (idempotent re-run) must not error: %v", err)
	}
	// No-overwrite: renaming onto an existing name is refused.
	if err := r.AddProject("ns", "taken"); err != nil {
		t.Fatal(err)
	}
	if err := r.RenameProject("ns", "new", "taken"); err == nil {
		t.Fatal("RenameProject must refuse a target-name collision")
	}
}

func TestRegistry_RenameNamespace(t *testing.T) {
	r := open(t)
	if err := r.AddProject("src", "p1"); err != nil {
		t.Fatal(err)
	}
	if err := r.AddProject("src", "p2"); err != nil {
		t.Fatal(err)
	}
	if has, _ := r.HasNamespace("src"); !has {
		t.Error("HasNamespace must report a managed namespace")
	}
	if err := r.RenameNamespace("src", "dst"); err != nil {
		t.Fatalf("RenameNamespace: %v", err)
	}
	if has, _ := r.HasNamespace("src"); has {
		t.Error("old namespace must be gone after rename")
	}
	if has, _ := r.HasNamespace("dst"); !has {
		t.Error("new namespace must be present after rename")
	}
	// The record carried its projects.
	for _, p := range []string{"p1", "p2"} {
		if has, _ := r.HasProject("dst", p); !has {
			t.Errorf("project %s must travel with the renamed namespace", p)
		}
	}
	rec, _, _ := r.Get("dst")
	if rec.Name != "dst" {
		t.Errorf("the re-keyed record's Name must be updated to dst, got %q", rec.Name)
	}
	// Idempotent recovery: re-running the applied rename is a no-op success.
	if err := r.RenameNamespace("src", "dst"); err != nil {
		t.Fatalf("RenameNamespace (idempotent re-run) must not error: %v", err)
	}
	// No-overwrite: renaming onto an existing namespace is refused.
	if err := r.EnsureNamespace("occupied"); err != nil {
		t.Fatal(err)
	}
	if err := r.RenameNamespace("dst", "occupied"); err == nil {
		t.Fatal("RenameNamespace must refuse a target-name collision")
	}
}

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
