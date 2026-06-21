package storage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/scopeclean"
	"github.com/sopranoworks/shoka/pkg/authz"
	"github.com/sopranoworks/shoka/pkg/oauthstore"
	"github.com/sopranoworks/shoka/pkg/userstore"
)

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// #1 — namespace is a first-class object: empty-creatable, enumerated at zero projects,
// survives deleting its last project, removed only by DeleteNamespace.
func TestNamespaceFirstClass_Lifecycle(t *testing.T) {
	s := newEmptyStorage(t)
	ctx := context.Background()

	if err := s.CreateNamespace("foo"); err != nil {
		t.Fatalf("CreateNamespace: %v", err)
	}
	// Enumerated with zero projects.
	nss, err := s.ListNamespaces()
	if err != nil {
		t.Fatal(err)
	}
	if !contains(nss, "foo") {
		t.Fatalf("empty namespace foo not enumerated: %v", nss)
	}
	if ps, _ := s.ListProjects("foo"); len(ps) != 0 {
		t.Fatalf("new namespace must have zero projects, got %v", ps)
	}
	// Idempotent create.
	if err := s.CreateNamespace("foo"); err != nil {
		t.Fatalf("CreateNamespace (idempotent) must not error: %v", err)
	}

	// Add a project, then delete it: the namespace SURVIVES (does not auto-vanish).
	if err := s.CreateProject("foo", "p1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, "foo", "p1"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	nss, _ = s.ListNamespaces()
	if !contains(nss, "foo") {
		t.Fatalf("namespace foo must survive deletion of its last project: %v", nss)
	}

	// DeleteNamespace removes it.
	if err := s.DeleteNamespace(ctx, "foo"); err != nil {
		t.Fatalf("DeleteNamespace: %v", err)
	}
	nss, _ = s.ListNamespaces()
	if contains(nss, "foo") {
		t.Fatalf("namespace foo must be gone after DeleteNamespace: %v", nss)
	}
	if _, err := os.Stat(filepath.Join(s.baseDir, "foo")); !os.IsNotExist(err) {
		t.Fatalf("namespace dir must be removed, stat err = %v", err)
	}
}

// #2 — DeleteProject is atomic and sibling-safe (the c9f6827 substrate): it removes the
// dir + BOTH sibling DBs + evicts the in-memory handles, and a sibling project in the
// same namespace stays intact and healthy. RED if only the dir were removed (the .db
// siblings would be stranded and the sibling check below would still pass, but the
// stranded-sibling assertions would fail).
func TestDeleteProject_SiblingSafe(t *testing.T) {
	s := newEmptyStorage(t)
	ctx := context.Background()

	for _, p := range []string{"p1", "p2"} {
		if err := s.CreateProject("ns", p); err != nil {
			t.Fatal(err)
		}
		// A write materialises the catalog AND the index sibling DBs.
		if _, err := s.Write(ctx, "", "ns", p, "a.md", "hello", nil); err != nil {
			t.Fatal(err)
		}
	}
	s.WaitForWAL(10 * time.Second)

	p1dir := filepath.Join(s.baseDir, "ns", "p1")
	p1cat := s.catalogPath("ns", "p1")
	p1idx := s.indexPath("ns", "p1")
	for _, p := range []string{p1dir, p1cat, p1idx} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("precondition: %s should exist: %v", p, err)
		}
	}

	if err := s.DeleteProject(ctx, "ns", "p1"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	// All three on-disk artefacts gone — no stranded sibling DB.
	for _, p := range []string{p1dir, p1cat, p1idx} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("artefact %s must be removed, stat err = %v", p, err)
		}
	}
	// In-memory handles evicted.
	key := projectKey("ns", "p1")
	s.catMu.Lock()
	_, catThere := s.catalogs[key]
	s.catMu.Unlock()
	s.idxMu.Lock()
	_, idxThere := s.indexes[key]
	s.idxMu.Unlock()
	s.stateMu.Lock()
	_, stThere := s.states[key]
	s.stateMu.Unlock()
	if catThere || idxThere || stThere {
		t.Errorf("in-memory handles for p1 not evicted: cat=%v idx=%v state=%v", catThere, idxThere, stThere)
	}

	// Sibling p2 is intact, healthy, readable, and still writable.
	if st := s.State("ns", "p2"); st != StateHealthy {
		t.Errorf("sibling p2 state = %v, want healthy", st)
	}
	if got, err := s.ReadFile("ns", "p2", "a.md"); err != nil || got != "hello" {
		t.Errorf("sibling p2 read = %q, err=%v, want %q", got, err, "hello")
	}
	if _, err := os.Stat(s.catalogPath("ns", "p2")); err != nil {
		t.Errorf("sibling p2 catalog must remain: %v", err)
	}
	if _, err := s.Write(ctx, "", "ns", "p2", "b.md", "world", nil); err != nil {
		t.Errorf("sibling p2 must still be writable: %v", err)
	}
}

// #1 (part 2, RED→GREEN core) — DeleteNamespace is EMPTY-ONLY: it REFUSES a non-empty
// namespace (no fan-out; projects untouched), succeeds once the projects are deleted one at
// a time, and keeps `default` delete-protected. RED on part 1's fan-out behaviour.
func TestDeleteNamespace_EmptyOnly(t *testing.T) {
	s := newEmptyStorage(t)
	ctx := context.Background()
	for _, p := range []string{"p1", "p2", "p3"} {
		if err := s.CreateProject("ns", p); err != nil {
			t.Fatal(err)
		}
	}

	// Non-empty → refused; the projects are NOT deleted.
	if err := s.DeleteNamespace(ctx, "ns"); err == nil {
		t.Fatal("DeleteNamespace must REFUSE a non-empty namespace (no fan-out)")
	}
	if _, err := os.Stat(filepath.Join(s.baseDir, "ns", "p1")); err != nil {
		t.Fatalf("a refused namespace delete must leave projects untouched: %v", err)
	}
	if nss, _ := s.ListNamespaces(); !contains(nss, "ns") {
		t.Fatalf("a refused namespace delete must keep the namespace managed: %v", nss)
	}

	// Delete the projects one at a time, then the (now-empty) namespace succeeds.
	for _, p := range []string{"p1", "p2", "p3"} {
		if err := s.DeleteProject(ctx, "ns", p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteNamespace(ctx, "ns"); err != nil {
		t.Fatalf("DeleteNamespace on an empty namespace must succeed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.baseDir, "ns")); !os.IsNotExist(err) {
		t.Fatalf("empty namespace dir must be gone after delete: %v", err)
	}
	if nss, _ := s.ListNamespaces(); contains(nss, "ns") {
		t.Errorf("namespace ns must not be enumerated after delete: %v", nss)
	}

	// `default` stays delete-protected regardless of emptiness.
	if err := s.DeleteNamespace(ctx, "default"); err == nil {
		t.Error("DeleteNamespace(default) must be refused")
	}
}

// storageWithStores wires a real cascade cleaner (over a userstore + oauthstore) into a
// fresh storage instance — the production path that DeleteProject/DeleteNamespace drive.
func storageWithStores(t *testing.T) (*FSGitStorage, *userstore.Store, *oauthstore.Store) {
	t.Helper()
	s := newEmptyStorage(t)
	dir := t.TempDir()
	var key [32]byte
	for i := range key {
		key[i] = byte(i + 1)
	}
	us, err := userstore.Open(filepath.Join(dir, "users.db"), key[:])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = us.Close() })
	os_, err := oauthstore.Open(filepath.Join(dir, "oauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os_.Close() })
	s.SetScopeCleaner(scopeclean.New(us, os_))
	return s, us, os_
}

func userScope(t *testing.T, us *userstore.Store, email string) string {
	t.Helper()
	u, err := us.GetUser(email)
	if err != nil {
		t.Fatalf("GetUser %s: %v", email, err)
	}
	return u.Scope
}

// #5a — cascade cleanup on namespace delete: every grant referencing foo (or foo/*) is
// purged from users + invites + token series; a `*` super-user grant is untouched; and
// re-creating foo gives a previously-foo:admin principal NO access.
func TestCascadeCleanup_NamespaceDelete(t *testing.T) {
	s, us, oas := storageWithStores(t)
	ctx := context.Background()
	now := time.Now()

	// Seed users.
	if err := us.CreateUser(&userstore.UserRecord{Email: "super@x", Scope: "*:admin"}); err != nil {
		t.Fatal(err)
	}
	if err := us.CreateUser(&userstore.UserRecord{Email: "mix@x", Scope: "namespace:foo:admin,namespace:bar:rw"}); err != nil {
		t.Fatal(err)
	}
	if err := us.CreateUser(&userstore.UserRecord{Email: "foonly@x", Scope: "namespace:foo:admin"}); err != nil {
		t.Fatal(err)
	}
	// Seed an invite + token series.
	if _, _, err := us.CreateInvite("invitee@x", "namespace:foo:rw", "super@x", now, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := oas.NewSeries("c1", oauthstore.Principal{Name: "n"}, "res", "namespace:foo:rw", now, time.Hour, 24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := oas.NewSeries("c2", oauthstore.Principal{Name: "n"}, "res", "*", now, time.Hour, 24*time.Hour); err != nil {
		t.Fatal(err)
	}

	if err := s.CreateNamespace("foo"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject("foo", "p1"); err != nil {
		t.Fatal(err)
	}
	// Empty-only delete (B-28 part 2): remove the project first, then the namespace. The
	// per-project DeleteProject cascade-cleans foo/p1 grants; DeleteNamespace cleans the
	// namespace-wide foo grants.
	if err := s.DeleteProject(ctx, "foo", "p1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNamespace(ctx, "foo"); err != nil {
		t.Fatalf("DeleteNamespace: %v", err)
	}

	// Super-user grant untouched.
	if got := userScope(t, us, "super@x"); got != "*:admin" {
		t.Errorf("super-user scope changed: %q", got)
	}
	// Mixed user keeps bar, loses foo.
	if got := userScope(t, us, "mix@x"); got != "namespace:bar:rw" {
		t.Errorf("mix scope = %q, want namespace:bar:rw", got)
	}
	// Foo-only user drops to no-access (NOT empty, which would read as super-user).
	if got := userScope(t, us, "foonly@x"); got != authz.NoAccessScope {
		t.Errorf("foonly scope = %q, want %q", got, authz.NoAccessScope)
	}
	if authz.IsSuperUser(userScope(t, us, "foonly@x")) {
		t.Error("foonly must not have escalated to super-user")
	}
	// Invite scope purged.
	invites, _ := us.ListInvites()
	for _, inv := range invites {
		if inv.Scope != authz.NoAccessScope {
			t.Errorf("invite scope = %q, want purged to %q", inv.Scope, authz.NoAccessScope)
		}
	}
	// Token series: foo purged, * untouched.
	series, _ := oas.List()
	var sawStar, sawPurged bool
	for _, se := range series {
		switch se.ClientID {
		case "c1":
			if se.Scope != authz.NoAccessScope {
				t.Errorf("c1 series scope = %q, want purged", se.Scope)
			}
			sawPurged = true
		case "c2":
			if se.Scope != "*" {
				t.Errorf("c2 series scope = %q, want * untouched", se.Scope)
			}
			sawStar = true
		}
	}
	if !sawStar || !sawPurged {
		t.Fatalf("expected both token series present (star=%v purged=%v)", sawStar, sawPurged)
	}

	// The invariant: re-create foo, and a previously-foo:admin user has NO access to it.
	if err := s.CreateNamespace("foo"); err != nil {
		t.Fatal(err)
	}
	if err := authz.Authorize(userScope(t, us, "foonly@x"), "foo", "", authz.LevelRead); err == nil {
		t.Error("re-creating foo must NOT resurrect foonly's access")
	}
	if err := authz.Authorize(userScope(t, us, "mix@x"), "foo", "", authz.LevelRead); err == nil {
		t.Error("re-creating foo must NOT resurrect mix's foo access")
	}
}

// #5b — cascade cleanup on a single project delete: only namespace:foo/p1 grants are
// purged; the namespace-wide namespace:foo grant survives.
func TestCascadeCleanup_ProjectDelete(t *testing.T) {
	s, us, _ := storageWithStores(t)
	ctx := context.Background()

	if err := us.CreateUser(&userstore.UserRecord{Email: "nswide@x", Scope: "namespace:foo:rw"}); err != nil {
		t.Fatal(err)
	}
	if err := us.CreateUser(&userstore.UserRecord{Email: "projonly@x", Scope: "namespace:foo/p1:rw"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject("foo", "p1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, "foo", "p1"); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	// Namespace-wide grant survives a project delete.
	if got := userScope(t, us, "nswide@x"); got != "namespace:foo:rw" {
		t.Errorf("namespace-wide scope = %q, want namespace:foo:rw (project delete must not touch it)", got)
	}
	// Project-specific grant purged to no-access.
	if got := userScope(t, us, "projonly@x"); got != authz.NoAccessScope {
		t.Errorf("project-only scope = %q, want %q", got, authz.NoAccessScope)
	}
}
