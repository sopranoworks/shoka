package authz

import (
	"reflect"
	"testing"
)

func TestIsSuperUser(t *testing.T) {
	cases := []struct {
		scope string
		want  bool
	}{
		{"", true},                     // migration-free zero value
		{"*", true},                    // bare wildcard
		{"*:admin", true},              // explicit wildcard admin
		{"*:rw", false},                // wildcard write is NOT super-user
		{"*:r", false},                 // wildcard read is NOT super-user
		{"namespace:foo:admin", false}, // admin on ONE namespace is NOT super-user
		{"namespace:foo:rw", false},
		{"namespace:foo:admin,namespace:bar:admin", false}, // admin on two is still not global
		{"none", false},
	}
	for _, c := range cases {
		if got := IsSuperUser(c.scope); got != c.want {
			t.Errorf("IsSuperUser(%q) = %v, want %v", c.scope, got, c.want)
		}
	}
}

func TestAdminNamespaces(t *testing.T) {
	// Super-user → nil set, superUser true.
	if ns, su := AdminNamespaces("*:admin"); ns != nil || !su {
		t.Errorf("super-user: got (%v,%v), want (nil,true)", ns, su)
	}
	if ns, su := AdminNamespaces(""); ns != nil || !su {
		t.Errorf("empty scope (super-user): got (%v,%v), want (nil,true)", ns, su)
	}
	// Scoped: only namespaces with namespace-wide admin, sorted + deduped. A project-
	// scoped admin (foo/p) and a non-admin namespace (qux:rw) do not count.
	ns, su := AdminNamespaces("namespace:foo:admin,namespace:bar:admin,namespace:qux:rw,namespace:zed/p:admin,namespace:foo:admin")
	if su {
		t.Fatalf("scoped principal reported super-user")
	}
	if want := []string{"bar", "foo"}; !reflect.DeepEqual(ns, want) {
		t.Errorf("AdminNamespaces = %v, want %v", ns, want)
	}
}

func TestPruneNamespaceGrants(t *testing.T) {
	cases := []struct {
		name        string
		scope, ns   string
		wantScope   string
		wantRemoved int
	}{
		{"drops namespace-wide and project grants, keeps others verbatim",
			"namespace:foo:admin,namespace:foo/p1:rw,namespace:bar:rw", "foo",
			"namespace:bar:rw", 2},
		{"keeps wildcard untouched",
			"*:admin", "foo", "*:admin", 0},
		{"super-user empty scope untouched",
			"", "foo", "", 0},
		{"emptying a scope yields the no-access sentinel, never empty (super-user footgun)",
			"namespace:foo:admin", "foo", NoAccessScope, 1},
		{"legacy level-less namespace grant is dropped",
			"namespace:foo,namespace:bar", "foo", "namespace:bar", 1},
		{"unrelated namespace is a no-op",
			"namespace:bar:admin", "foo", "namespace:bar:admin", 0},
	}
	for _, c := range cases {
		got, removed := PruneNamespaceGrants(c.scope, c.ns)
		if got != c.wantScope || removed != c.wantRemoved {
			t.Errorf("%s: PruneNamespaceGrants(%q,%q) = (%q,%d), want (%q,%d)",
				c.name, c.scope, c.ns, got, removed, c.wantScope, c.wantRemoved)
		}
	}
}

func TestPruneProjectGrants(t *testing.T) {
	cases := []struct {
		name            string
		scope, ns, proj string
		wantScope       string
		wantRemoved     int
	}{
		{"drops only the specific project, keeps namespace-wide + wildcard",
			"*:admin,namespace:foo:admin,namespace:foo/p1:rw,namespace:foo/p2:r", "foo", "p1",
			"*:admin,namespace:foo:admin,namespace:foo/p2:r", 1},
		{"namespace-wide grant is NOT dropped by a project delete",
			"namespace:foo:rw", "foo", "p1", "namespace:foo:rw", 0},
		{"emptying yields the no-access sentinel",
			"namespace:foo/p1:admin", "foo", "p1", NoAccessScope, 1},
	}
	for _, c := range cases {
		got, removed := PruneProjectGrants(c.scope, c.ns, c.proj)
		if got != c.wantScope || removed != c.wantRemoved {
			t.Errorf("%s: PruneProjectGrants(%q,%q,%q) = (%q,%d), want (%q,%d)",
				c.name, c.scope, c.ns, c.proj, got, removed, c.wantScope, c.wantRemoved)
		}
	}
}

func TestRewriteProjectGrants(t *testing.T) {
	cases := []struct {
		name              string
		scope, ns, p, dst string
		wantScope         string
		wantRewritten     int
	}{
		{"re-homes the project grant, perm preserved, others verbatim",
			"namespace:foo/p1:rw,namespace:bar:rw", "foo", "p1", "baz",
			"namespace:baz/p1:rw,namespace:bar:rw", 1},
		{"namespace-wide grant is NOT re-homed by a project move",
			"namespace:foo:admin", "foo", "p1", "baz", "namespace:foo:admin", 0},
		{"wildcard untouched",
			"*:admin", "foo", "p1", "baz", "*:admin", 0},
		{"a different project under the source is untouched",
			"namespace:foo/p2:rw", "foo", "p1", "baz", "namespace:foo/p2:rw", 0},
		{"legacy level-less project grant keeps its form",
			"namespace:foo/p1", "foo", "p1", "baz", "namespace:baz/p1", 1},
		{"admin perm preserved",
			"namespace:foo/p1:admin", "foo", "p1", "baz", "namespace:baz/p1:admin", 1},
	}
	for _, c := range cases {
		// A move keeps the project name (oldProj == newProj == c.p); only the namespace changes.
		got, n := RewriteProjectGrants(c.scope, c.ns, c.p, c.dst, c.p)
		if got != c.wantScope || n != c.wantRewritten {
			t.Errorf("%s: RewriteProjectGrants(%q,%q,%q,%q,%q) = (%q,%d), want (%q,%d)",
				c.name, c.scope, c.ns, c.p, c.dst, c.p, got, n, c.wantScope, c.wantRewritten)
		}
	}
}

// TestRewriteProjectGrants_RenameCase covers the generalised helper's OTHER caller: a project
// RENAME keeps the namespace and changes the project segment.
func TestRewriteProjectGrants_RenameCase(t *testing.T) {
	// namespace:ns/old:rw → namespace:ns/new:rw (ns fixed, proj changes); ns-wide untouched.
	got, n := RewriteProjectGrants("namespace:ns/old:rw,namespace:ns:admin", "ns", "old", "ns", "new")
	if want := "namespace:ns/new:rw,namespace:ns:admin"; got != want || n != 1 {
		t.Errorf("rename rewrite = (%q,%d), want (%q,1)", got, n, want)
	}
}

// TestRewriteNamespaceGrants covers the namespace-rename DUAL rewrite: BOTH the namespace-wide
// grant AND every project-specific grant follow the new name; wildcards and other namespaces
// are untouched; perms (and the legacy level-less form) are preserved.
func TestRewriteNamespaceGrants(t *testing.T) {
	cases := []struct {
		name, scope, old, new string
		wantScope             string
		wantRewritten         int
	}{
		{"namespace-wide AND project-specific both follow, perms preserved",
			"namespace:src:rw,namespace:src/p1:admin,namespace:other:rw", "src", "dst",
			"namespace:dst:rw,namespace:dst/p1:admin,namespace:other:rw", 2},
		{"wildcard untouched",
			"*:admin", "src", "dst", "*:admin", 0},
		{"legacy level-less forms preserved",
			"namespace:src,namespace:src/p1", "src", "dst", "namespace:dst,namespace:dst/p1", 2},
		{"a different namespace is untouched",
			"namespace:other/p1:rw", "src", "dst", "namespace:other/p1:rw", 0},
	}
	for _, c := range cases {
		got, n := RewriteNamespaceGrants(c.scope, c.old, c.new)
		if got != c.wantScope || n != c.wantRewritten {
			t.Errorf("%s: RewriteNamespaceGrants(%q,%q,%q) = (%q,%d), want (%q,%d)",
				c.name, c.scope, c.old, c.new, got, n, c.wantScope, c.wantRewritten)
		}
	}
}

func TestIsSuperUser_ZonedWildcardIsNot(t *testing.T) {
	if IsSuperUser("git/*:admin") {
		t.Fatal("zoned wildcard admin must not be super-user")
	}
	if IsSuperUser("git/*:admin,namespace:foo:rw") {
		t.Fatal("zoned wildcard + unzoned non-admin must not be super-user")
	}
}

func TestAdminNamespaces_IgnoresZoned(t *testing.T) {
	ns, su := AdminNamespaces("git/namespace:foo:admin,namespace:bar:admin")
	if su {
		t.Fatal("must not be super-user")
	}
	if want := []string{"bar"}; !reflect.DeepEqual(ns, want) {
		t.Errorf("AdminNamespaces = %v, want %v (zoned foo should be ignored)", ns, want)
	}
}

func TestPruneNamespaceGrants_IncludesZoned(t *testing.T) {
	scope := "namespace:foo:admin,git/namespace:foo:rw"
	got, removed := PruneNamespaceGrants(scope, "foo")
	if removed != 2 {
		t.Errorf("should remove 2 (both zoned and unzoned), removed %d", removed)
	}
	if got != NoAccessScope {
		t.Errorf("all grants pruned should yield NoAccessScope: got %q", got)
	}
}

func TestPruneNamespaceGrants_ZonedOtherNamespaceUntouched(t *testing.T) {
	scope := "namespace:foo:admin,git/namespace:bar:rw"
	got, removed := PruneNamespaceGrants(scope, "foo")
	if removed != 1 {
		t.Errorf("should remove 1 (only unzoned foo), removed %d", removed)
	}
	if got != "git/namespace:bar:rw" {
		t.Errorf("zoned grant for other namespace should survive: got %q", got)
	}
}

func TestPruneProjectGrants_IncludesZoned(t *testing.T) {
	scope := "namespace:foo/p1:rw,git/namespace:foo/p1:admin"
	got, removed := PruneProjectGrants(scope, "foo", "p1")
	if removed != 2 {
		t.Errorf("should remove 2 (both zoned and unzoned), removed %d", removed)
	}
	if got != NoAccessScope {
		t.Errorf("all grants pruned should yield NoAccessScope: got %q", got)
	}
}

func TestRewriteProjectGrants_IncludesZoned(t *testing.T) {
	scope := "namespace:foo/p1:rw,git/namespace:foo/p1:admin"
	got, n := RewriteProjectGrants(scope, "foo", "p1", "bar", "p1")
	if n != 2 {
		t.Errorf("should rewrite 2 (both zoned and unzoned), got %d", n)
	}
	if want := "namespace:bar/p1:rw,git/namespace:bar/p1:admin"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRewriteProjectGrants_ZonedRenameCase(t *testing.T) {
	scope := "git/namespace:ns/old:rw,namespace:ns/old:admin"
	got, n := RewriteProjectGrants(scope, "ns", "old", "ns", "new")
	if n != 2 {
		t.Errorf("should rewrite 2, got %d", n)
	}
	if want := "git/namespace:ns/new:rw,namespace:ns/new:admin"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRewriteNamespaceGrants_IncludesZoned(t *testing.T) {
	scope := "namespace:src:rw,git/namespace:src:admin"
	got, n := RewriteNamespaceGrants(scope, "src", "dst")
	if n != 2 {
		t.Errorf("should rewrite 2 (both zoned and unzoned), got %d", n)
	}
	if want := "namespace:dst:rw,git/namespace:dst:admin"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRewriteNamespaceGrants_ZonedWithProject(t *testing.T) {
	scope := "git/namespace:src/p1:rw,namespace:src/p1:admin,namespace:other:rw"
	got, n := RewriteNamespaceGrants(scope, "src", "dst")
	if n != 2 {
		t.Errorf("should rewrite 2, got %d", n)
	}
	if want := "git/namespace:dst/p1:rw,namespace:dst/p1:admin,namespace:other:rw"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRewriteNamespaceGrants_MixedScopes(t *testing.T) {
	scope := "namespace:src:r,git/namespace:src:rw,namespace:src/p1:admin,git/namespace:src/p2:r"
	got, n := RewriteNamespaceGrants(scope, "src", "dst")
	if n != 4 {
		t.Errorf("should rewrite 4, got %d", n)
	}
	if want := "namespace:dst:r,git/namespace:dst:rw,namespace:dst/p1:admin,git/namespace:dst/p2:r"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestNoAccessScope_DeniesEverything proves the cascade-cleanup substitute really grants
// nothing — the whole point of using it instead of "" (which would read as super-user).
func TestNoAccessScope_DeniesEverything(t *testing.T) {
	if IsSuperUser(NoAccessScope) {
		t.Fatal("NoAccessScope must not be super-user")
	}
	if err := Authorize(NoAccessScope, "foo", "", LevelRead); err == nil {
		t.Error("NoAccessScope must deny even read")
	}
	if err := Authorize(NoAccessScope, "", "", LevelRead); err == nil {
		t.Error("NoAccessScope must deny a global read")
	}
}
