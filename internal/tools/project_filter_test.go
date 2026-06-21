package tools

import (
	"context"
	"testing"

	"github.com/sopranoworks/shoka/pkg/auth"
)

// B-28 stage 3: list_projects results are filtered to the namespaces the principal
// has at least read on (the deferred stage-2 global-read item). A super-user sees all.
func TestFilterProjectsByReadScope(t *testing.T) {
	all := []string{"foo/a", "foo/b", "bar/c", "baz/d"}

	// Super-user (no principal ⇒ scope "") keeps everything.
	if got := filterProjectsByReadScope(context.Background(), all); len(got) != 4 {
		t.Fatalf("super-user must see all, got %v", got)
	}

	// A namespace:foo:r principal sees only foo/*.
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Scope: "namespace:foo:r"})
	got := filterProjectsByReadScope(ctx, all)
	if len(got) != 2 || got[0] != "foo/a" || got[1] != "foo/b" {
		t.Fatalf("foo:r principal must see only foo/*, got %v", got)
	}

	// A two-namespace principal sees both granted namespaces, not the third.
	ctx2 := auth.WithPrincipal(context.Background(), auth.Principal{Scope: "namespace:foo:r,namespace:bar:rw"})
	got2 := filterProjectsByReadScope(ctx2, all)
	if len(got2) != 3 {
		t.Fatalf("foo+bar principal must see 3, got %v", got2)
	}
	for _, p := range got2 {
		if p == "baz/d" {
			t.Fatal("baz must be filtered out")
		}
	}
}
