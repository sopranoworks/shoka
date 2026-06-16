package authz

import "testing"

func TestParseScope_SuperUserZeroValue(t *testing.T) {
	for _, s := range []string{"", "  ", "*"} {
		g := ParseScope(s)
		if len(g) != 1 || !g[0].Wildcard || g[0].Level != LevelAdmin {
			t.Fatalf("scope %q must parse to super-user (wildcard admin), got %+v", s, g)
		}
	}
}

func TestParseScope_Forms(t *testing.T) {
	cases := []struct {
		in   string
		want Grant
	}{
		{"namespace:foo:r", Grant{Namespace: "foo", Level: LevelRead}},
		{"namespace:foo:rw", Grant{Namespace: "foo", Level: LevelWrite}},
		{"namespace:foo:admin", Grant{Namespace: "foo", Level: LevelAdmin}},
		{"namespace:foo/proj:rw", Grant{Namespace: "foo", Project: "proj", Level: LevelWrite}},
		{"namespace:foo", Grant{Namespace: "foo", Level: LevelWrite}}, // legacy level-less ⇒ rw
		{"*:r", Grant{Wildcard: true, Level: LevelRead}},
		{"*:admin", Grant{Wildcard: true, Level: LevelAdmin}},
	}
	for _, c := range cases {
		g := ParseScope(c.in)
		if len(g) != 1 || g[0] != c.want {
			t.Fatalf("ParseScope(%q) = %+v, want %+v", c.in, g, c.want)
		}
	}
}

func TestParseScope_DuplicateMostPermissive(t *testing.T) {
	// Two grants for the same target: the effective level is the strongest (max).
	g := ParseScope("namespace:foo:r, namespace:foo:admin")
	if EffectiveLevel(g, "foo", "p") != LevelAdmin {
		t.Fatalf("duplicate same-target must resolve most-permissive (admin), got %v",
			EffectiveLevel(g, "foo", "p"))
	}
}

func TestAuthorize_SuperUserPassesAll(t *testing.T) {
	for _, s := range []string{"*", ""} {
		for _, req := range []Level{LevelRead, LevelWrite, LevelAdmin} {
			if err := Authorize(s, "anything", "any", req); err != nil {
				t.Fatalf("super-user %q must pass %v, got %v", s, req, err)
			}
			// Global op too.
			if err := Authorize(s, "", "", req); err != nil {
				t.Fatalf("super-user %q must pass global %v, got %v", s, req, err)
			}
		}
	}
}

func TestAuthorize_LevelOrdering(t *testing.T) {
	scope := "namespace:foo:rw"
	if err := Authorize(scope, "foo", "p", LevelRead); err != nil {
		t.Fatalf("rw should satisfy read: %v", err)
	}
	if err := Authorize(scope, "foo", "p", LevelWrite); err != nil {
		t.Fatalf("rw should satisfy write: %v", err)
	}
	if err := Authorize(scope, "foo", "p", LevelAdmin); err == nil {
		t.Fatal("rw must NOT satisfy admin")
	}
}

func TestAuthorize_NamespaceIsolation(t *testing.T) {
	scope := "namespace:foo:r"
	if err := Authorize(scope, "foo", "p", LevelRead); err != nil {
		t.Fatalf("read foo should pass: %v", err)
	}
	if err := Authorize(scope, "foo", "p", LevelWrite); err == nil {
		t.Fatal("read-only foo must be denied write")
	}
	if err := Authorize(scope, "bar", "p", LevelRead); err == nil {
		t.Fatal("foo-only must be denied any access to bar")
	}
}

func TestAuthorize_GlobalReadSatisfiedByAnyPositiveLevel(t *testing.T) {
	// A namespace-scoped reader can perform a global READ (max level anywhere ≥ read)
	// but not a global ADMIN op.
	scope := "namespace:foo:r"
	if err := Authorize(scope, "", "", LevelRead); err != nil {
		t.Fatalf("namespace reader should pass a global read, got %v", err)
	}
	if err := Authorize(scope, "", "", LevelAdmin); err == nil {
		t.Fatal("namespace reader must be denied a global admin op")
	}
}

func TestAuthorize_ProjectScope(t *testing.T) {
	scope := "namespace:foo/alpha:rw"
	if err := Authorize(scope, "foo", "alpha", LevelWrite); err != nil {
		t.Fatalf("foo/alpha rw should pass foo/alpha write: %v", err)
	}
	if err := Authorize(scope, "foo", "beta", LevelRead); err == nil {
		t.Fatal("foo/alpha grant must not reach foo/beta")
	}
}
