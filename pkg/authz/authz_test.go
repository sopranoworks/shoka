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

func TestParseScope_ZonePrefix(t *testing.T) {
	cases := []struct {
		in   string
		want Grant
	}{
		{"git/namespace:foo:rw", Grant{Zone: "git", Namespace: "foo", Level: LevelWrite}},
		{"git/namespace:foo/proj:admin", Grant{Zone: "git", Namespace: "foo", Project: "proj", Level: LevelAdmin}},
		{"custom/namespace:bar:r", Grant{Zone: "custom", Namespace: "bar", Level: LevelRead}},
		{"git/*:admin", Grant{Zone: "git", Wildcard: true, Level: LevelAdmin}},
		{"git/*", Grant{Zone: "git", Wildcard: true, Level: LevelAdmin}},
	}
	for _, c := range cases {
		g := ParseScope(c.in)
		if len(g) != 1 || g[0] != c.want {
			t.Fatalf("ParseScope(%q) = %+v, want %+v", c.in, g, c.want)
		}
	}
}

func TestParseScope_MixedZones(t *testing.T) {
	g := ParseScope("namespace:foo:rw,git/namespace:foo:rw,custom/namespace:bar:r")
	if len(g) != 3 {
		t.Fatalf("expected 3 grants, got %d: %+v", len(g), g)
	}
	if g[0].Zone != "" || g[0].Namespace != "foo" {
		t.Fatalf("grant 0: want unzoned foo, got %+v", g[0])
	}
	if g[1].Zone != "git" || g[1].Namespace != "foo" {
		t.Fatalf("grant 1: want zone=git foo, got %+v", g[1])
	}
	if g[2].Zone != "custom" || g[2].Namespace != "bar" {
		t.Fatalf("grant 2: want zone=custom bar, got %+v", g[2])
	}
}

func TestAuthorize_ZonedScopesIgnored(t *testing.T) {
	// A zoned scope grants nothing from Shoka's perspective.
	if err := Authorize("git/namespace:foo:admin", "foo", "p", LevelRead); err == nil {
		t.Fatal("zoned scope must not satisfy Shoka authorization")
	}
	// A zoned wildcard is also ignored.
	if err := Authorize("git/*:admin", "foo", "p", LevelRead); err == nil {
		t.Fatal("zoned wildcard must not satisfy Shoka authorization")
	}
	// Mixed: unzoned grants still work alongside zoned ones.
	if err := Authorize("git/namespace:foo:admin,namespace:foo:r", "foo", "p", LevelRead); err != nil {
		t.Fatalf("unzoned grant in mixed scope should satisfy read: %v", err)
	}
}

func TestParseScope_ExistingProjectSlashUnchanged(t *testing.T) {
	// The "/" in namespace:foo/proj:rw must NOT be treated as a zone separator.
	g := ParseScope("namespace:foo/proj:rw")
	if len(g) != 1 {
		t.Fatalf("expected 1 grant, got %d", len(g))
	}
	if g[0].Zone != "" || g[0].Namespace != "foo" || g[0].Project != "proj" || g[0].Level != LevelWrite {
		t.Fatalf("existing project-scoped grant broken: %+v", g[0])
	}
}
