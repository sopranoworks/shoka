package tools

import (
	"context"
	"testing"

	"github.com/sopranoworks/shoka/internal/auth"
	"github.com/sopranoworks/shoka/internal/storage"
)

// #6 (MCP gate) — namespace_health is admin-somewhere (any admin may read, filtered);
// namespace_recover project-level needs admin on the target namespace (the AuthzMiddleware).
func TestNamespaceHealth_Gate(t *testing.T) {
	// namespace_health: global admin target ⇒ "admin somewhere".
	if reached, _ := runGate(t, "*", "namespace_health", "{}"); !reached {
		t.Error("super-user must read namespace_health")
	}
	if reached, _ := runGate(t, "namespace:foo:admin", "namespace_health", "{}"); !reached {
		t.Error("a namespace-admin must reach namespace_health (filtered)")
	}
	if reached, res := runGate(t, "namespace:foo:rw", "namespace_health", "{}"); reached || !isError(res) {
		t.Error("a non-admin must be DENIED namespace_health")
	}

	// namespace_recover project-level: admin on the target namespace.
	pjArgs := `{"action":"drop_missing","namespace":"foo","project_name":"p"}`
	if reached, _ := runGate(t, "namespace:foo:admin", "namespace_recover", pjArgs); !reached {
		t.Error("namespace:foo:admin must reach namespace_recover on foo")
	}
	if reached, res := runGate(t, "namespace:bar:admin", "namespace_recover", pjArgs); reached || !isError(res) {
		t.Error("admin on bar must be DENIED namespace_recover on foo")
	}
	if reached, res := runGate(t, "namespace:foo:rw", "namespace_recover", pjArgs); reached || !isError(res) {
		t.Error("write-only on foo must be DENIED namespace_recover")
	}
}

type fakeNSRecoverer struct {
	droppedNamespace string
	droppedProject   string
	adoptedNamespace string
}

func (f *fakeNSRecoverer) DropMissingNamespace(ns string) error { f.droppedNamespace = ns; return nil }
func (f *fakeNSRecoverer) DropMissingProject(ns, proj string) error {
	f.droppedProject = ns + "/" + proj
	return nil
}
func (f *fakeNSRecoverer) CleanOrphanedSibling(ns, name string) error { return nil }
func (f *fakeNSRecoverer) AdoptForeign(ns, proj string) error {
	if proj == "" {
		f.adoptedNamespace = ns
	}
	return nil
}

// #6 (MCP handler) — whole-namespace recovery actions (empty project) are SUPER-USER only;
// the handler tightens beyond the middleware's namespace-targeted admin.
func TestNamespaceRecover_NamespaceLevelSuperUserOnly(t *testing.T) {
	ctxOf := func(scope string) context.Context {
		return auth.WithPrincipal(context.Background(), auth.Principal{Scope: scope})
	}
	nsLevelDrop := NamespaceRecoverInput{Action: "drop_missing", Namespace: "foo"}
	nsLevelAdopt := NamespaceRecoverInput{Action: "adopt", Namespace: "foo"}

	// namespace-admin → refused, op not run.
	f := &fakeNSRecoverer{}
	res, _, _ := NamespaceRecoverHandler(f)(ctxOf("namespace:foo:admin"), nil, nsLevelDrop)
	if res == nil || !res.IsError {
		t.Error("namespace-level drop_missing must be refused for a namespace-admin")
	}
	if f.droppedNamespace != "" {
		t.Errorf("refused drop_missing must NOT run the op (got %q)", f.droppedNamespace)
	}
	f = &fakeNSRecoverer{}
	res, _, _ = NamespaceRecoverHandler(f)(ctxOf("namespace:foo:admin"), nil, nsLevelAdopt)
	if res == nil || !res.IsError {
		t.Error("namespace-level adopt must be refused for a namespace-admin")
	}
	if f.adoptedNamespace != "" {
		t.Errorf("refused adopt must NOT run the op (got %q)", f.adoptedNamespace)
	}

	// super-user → allowed, op runs.
	f = &fakeNSRecoverer{}
	res, _, _ = NamespaceRecoverHandler(f)(ctxOf("*"), nil, nsLevelDrop)
	if res != nil && res.IsError {
		t.Fatal("super-user namespace-level drop_missing must succeed")
	}
	if f.droppedNamespace != "foo" {
		t.Errorf("super-user drop_missing did not run the op (got %q)", f.droppedNamespace)
	}
}

// compile-time checks the fakes satisfy the handler capabilities.
var _ namespaceRecoverer = (*fakeNSRecoverer)(nil)

type fakeNSHealth struct{}

func (fakeNSHealth) CheckAllHealth() storage.HealthReport { return storage.HealthReport{} }

var _ namespaceHealthReader = fakeNSHealth{}
