package tools

import (
	"context"
	"testing"

	"github.com/sopranoworks/shoka/pkg/auth"
)

// #4 (MCP, project ops = admin on the target namespace). create_project is RAISED to
// admin (a write-only principal can no longer create); delete_project is admin-on-ns.
// Both are enforced by the AuthzMiddleware via toolLevels, so the gate is the assertion.
func TestProjectOps_AuthzOnTargetNamespace(t *testing.T) {
	args := `{"namespace":"foo","project_name":"x"}`
	for _, tool := range []string{"create_project", "delete_project"} {
		// super-user → allowed.
		if reached, _ := runGate(t, "*", tool, args); !reached {
			t.Errorf("%s: super-user must be allowed", tool)
		}
		// admin on foo → allowed on foo.
		if reached, _ := runGate(t, "namespace:foo:admin", tool, args); !reached {
			t.Errorf("%s: namespace:foo:admin must be allowed on foo", tool)
		}
		// admin on bar → DENIED on foo.
		if reached, res := runGate(t, "namespace:bar:admin", tool, args); reached || !isError(res) {
			t.Errorf("%s: admin on bar must be DENIED on foo", tool)
		}
		// write on foo → DENIED (needs admin; this is the create_project tightening).
		if reached, res := runGate(t, "namespace:foo:rw", tool, args); reached || !isError(res) {
			t.Errorf("%s: write-only on foo must be DENIED (admin required)", tool)
		}
		// read on foo → DENIED.
		if reached, res := runGate(t, "namespace:foo:r", tool, args); reached || !isError(res) {
			t.Errorf("%s: read-only on foo must be DENIED", tool)
		}
	}
}

// fakeProjectMover records whether the underlying move ran.
type fakeProjectMover struct {
	moved bool
	args  [3]string
}

func (f *fakeProjectMover) MoveProject(_ context.Context, oldNs, proj, newNs string) error {
	f.moved = true
	f.args = [3]string{oldNs, proj, newNs}
	return nil
}

// #6 (MCP, project MOVE = SUPER-USER only in this first cut). The handler's requireSuperUser
// is authoritative: a namespace-admin — even on BOTH the source and target — is refused and
// the move never runs; a super-user succeeds.
func TestMoveProject_SuperUserOnly(t *testing.T) {
	scopedCtx := func(scope string) context.Context {
		return auth.WithPrincipal(context.Background(), auth.Principal{Scope: scope})
	}
	in := MoveProjectInput{Namespace: "src", ProjectName: "proj", NewNamespace: "dst"}

	// admin on both src and dst → still REFUSED (super-user only), op not run.
	f := &fakeProjectMover{}
	res, _, _ := MoveProjectHandler(f)(scopedCtx("namespace:src:admin,namespace:dst:admin"), nil, in)
	if res == nil || !res.IsError {
		t.Error("admin-on-both move_project must be refused (super-user only)")
	}
	if f.moved {
		t.Error("refused move_project must NOT run the op")
	}

	// super-user → allowed, op runs with the right args.
	f = &fakeProjectMover{}
	res, _, _ = MoveProjectHandler(f)(scopedCtx("*"), nil, in)
	if res != nil && res.IsError {
		t.Fatal("super-user move_project must succeed")
	}
	if !f.moved || f.args != [3]string{"src", "proj", "dst"} {
		t.Errorf("super-user move_project did not run with the right args: %+v", f)
	}
}

// #7 (MCP, project RENAME = admin on the namespace). rename_project is enforced by the
// AuthzMiddleware via toolLevels (admin-on-ns, NOT super-user — the project stays in its
// namespace), so the gate is the assertion. Mirrors create/delete_project.
func TestRenameProject_AuthzOnTargetNamespace(t *testing.T) {
	args := `{"namespace":"foo","project_name":"x","new_project_name":"y"}`
	// super-user → allowed.
	if reached, _ := runGate(t, "*", "rename_project", args); !reached {
		t.Error("rename_project: super-user must be allowed")
	}
	// admin on foo → allowed on foo.
	if reached, _ := runGate(t, "namespace:foo:admin", "rename_project", args); !reached {
		t.Error("rename_project: namespace:foo:admin must be allowed on foo")
	}
	// admin on bar → DENIED on foo.
	if reached, res := runGate(t, "namespace:bar:admin", "rename_project", args); reached || !isError(res) {
		t.Error("rename_project: admin on bar must be DENIED on foo")
	}
	// write on foo → DENIED (needs admin).
	if reached, res := runGate(t, "namespace:foo:rw", "rename_project", args); reached || !isError(res) {
		t.Error("rename_project: write-only on foo must be DENIED (admin required)")
	}
	// read on foo → DENIED.
	if reached, res := runGate(t, "namespace:foo:r", "rename_project", args); reached || !isError(res) {
		t.Error("rename_project: read-only on foo must be DENIED")
	}
}

// fakeNamespaceRenamer records whether the underlying rename ran.
type fakeNamespaceRenamer struct {
	renamed bool
	args    [2]string
}

func (f *fakeNamespaceRenamer) RenameNamespace(_ context.Context, oldName, newName string) error {
	f.renamed = true
	f.args = [2]string{oldName, newName}
	return nil
}

// #7 (MCP, namespace RENAME = SUPER-USER only). The handler's requireSuperUser is
// authoritative: a namespace-admin is refused and the op never runs; a super-user succeeds.
func TestRenameNamespace_SuperUserOnly(t *testing.T) {
	scopedCtx := func(scope string) context.Context {
		return auth.WithPrincipal(context.Background(), auth.Principal{Scope: scope})
	}
	in := RenameNamespaceInput{Namespace: "src", NewNamespace: "dst"}

	// namespace-admin → REFUSED, op not run.
	f := &fakeNamespaceRenamer{}
	res, _, _ := RenameNamespaceHandler(f)(scopedCtx("namespace:src:admin"), nil, in)
	if res == nil || !res.IsError {
		t.Error("namespace-admin rename_namespace must be refused (super-user only)")
	}
	if f.renamed {
		t.Error("refused rename_namespace must NOT run the op")
	}

	// super-user → allowed, op runs with the right args.
	f = &fakeNamespaceRenamer{}
	res, _, _ = RenameNamespaceHandler(f)(scopedCtx("*"), nil, in)
	if res != nil && res.IsError {
		t.Fatal("super-user rename_namespace must succeed")
	}
	if !f.renamed || f.args != [2]string{"src", "dst"} {
		t.Errorf("super-user rename_namespace did not run with the right args: %+v", f)
	}
}

// fakeNamespaceManager records whether the underlying op ran.
type fakeNamespaceManager struct {
	createdNS string
	deletedNS string
}

func (f *fakeNamespaceManager) CreateNamespace(ns string) error { f.createdNS = ns; return nil }
func (f *fakeNamespaceManager) DeleteNamespace(_ context.Context, ns string) error {
	f.deletedNS = ns
	return nil
}

// #4 (MCP, namespace ops = SUPER-USER only). The handler's requireSuperUser is the
// authoritative gate (the middleware-level admin check would let a namespace-admin
// through for its own namespace, so the handler tightens it). A namespace-admin is
// REFUSED and the underlying op never runs; a super-user succeeds.
func TestNamespaceOps_SuperUserOnly(t *testing.T) {
	scopedCtx := func(scope string) context.Context {
		return auth.WithPrincipal(context.Background(), auth.Principal{Scope: scope})
	}

	// create_namespace — super-user allowed.
	f := &fakeNamespaceManager{}
	res, _, _ := CreateNamespaceHandler(f)(scopedCtx("*"), nil, CreateNamespaceInput{Namespace: "foo"})
	if res != nil && res.IsError {
		t.Fatalf("super-user create_namespace must succeed, got error result")
	}
	if f.createdNS != "foo" {
		t.Errorf("super-user create_namespace did not run the op (createdNS=%q)", f.createdNS)
	}

	// create_namespace — namespace-admin REFUSED, op not run.
	f = &fakeNamespaceManager{}
	res, _, _ = CreateNamespaceHandler(f)(scopedCtx("namespace:foo:admin"), nil, CreateNamespaceInput{Namespace: "foo"})
	if res == nil || !res.IsError {
		t.Errorf("namespace-admin create_namespace must be refused")
	}
	if f.createdNS != "" {
		t.Errorf("refused create_namespace must NOT run the op (createdNS=%q)", f.createdNS)
	}

	// delete_namespace — namespace-admin REFUSED, op not run.
	f = &fakeNamespaceManager{}
	res, _, _ = DeleteNamespaceHandler(f)(scopedCtx("namespace:foo:admin"), nil, DeleteNamespaceInput{Namespace: "foo"})
	if res == nil || !res.IsError {
		t.Errorf("namespace-admin delete_namespace must be refused")
	}
	if f.deletedNS != "" {
		t.Errorf("refused delete_namespace must NOT run the op (deletedNS=%q)", f.deletedNS)
	}

	// delete_namespace — super-user allowed.
	f = &fakeNamespaceManager{}
	res, _, _ = DeleteNamespaceHandler(f)(scopedCtx("*"), nil, DeleteNamespaceInput{Namespace: "foo"})
	if res != nil && res.IsError {
		t.Fatalf("super-user delete_namespace must succeed")
	}
	if f.deletedNS != "foo" {
		t.Errorf("super-user delete_namespace did not run the op (deletedNS=%q)", f.deletedNS)
	}
}
