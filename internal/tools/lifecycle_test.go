package tools

import (
	"context"
	"testing"

	"github.com/sopranoworks/shoka/internal/auth"
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
