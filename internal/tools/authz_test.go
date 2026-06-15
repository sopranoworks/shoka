package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/auth"
)

// The 2026-06-15 authz foundation: the gate is *-pass today (scope "*" or an
// absent principal allow everything — behaviour unchanged) but its else-branch is
// REAL and tested, so a future pre-issued scoped token is enforced automatically.

func TestAuthorize_StarAndEmptyAllowEverything(t *testing.T) {
	for _, scope := range []string{"*", ""} {
		p := auth.Principal{Name: "Op", Scope: scope}
		if err := authorize(p, "anything", "any-proj", "write"); err != nil {
			t.Fatalf("scope %q should allow all access, got %v", scope, err)
		}
	}
	// The no-principal case (zero Principal, Scope "") — plain transport / OAuth off.
	if err := authorize(auth.Principal{}, "ns", "proj", "read"); err != nil {
		t.Fatalf("absent principal should be allowed (single-operator local), got %v", err)
	}
}

// TestAuthorize_DormantEnforcement is the key proof the gate is real, not a no-op:
// a synthetic scoped principal (no such token is minted today) is enforced — the
// granted namespace is allowed and any other is denied.
func TestAuthorize_DormantEnforcement(t *testing.T) {
	p := auth.Principal{Name: "Scoped", Scope: "namespace:foo"}

	if err := authorize(p, "foo", "proj", "read"); err != nil {
		t.Fatalf("namespace:foo principal should reach namespace foo, got %v", err)
	}
	if err := authorize(p, "bar", "proj", "read"); err == nil {
		t.Fatalf("namespace:foo principal must be DENIED namespace bar, got allow")
	}
	// A non-* scope with no namespace target is conservatively denied.
	if err := authorize(p, "", "", "read"); err == nil {
		t.Fatalf("scoped principal with empty namespace must be denied, got allow")
	}
}

func TestAuthorize_ProjectScopedGrant(t *testing.T) {
	p := auth.Principal{Scope: "namespace:foo/alpha"}
	if err := authorize(p, "foo", "alpha", "read"); err != nil {
		t.Fatalf("project grant foo/alpha should allow foo/alpha, got %v", err)
	}
	if err := authorize(p, "foo", "beta", "read"); err == nil {
		t.Fatalf("project grant foo/alpha must deny foo/beta, got allow")
	}
}

func TestAuthorize_MultiGrant(t *testing.T) {
	p := auth.Principal{Scope: "namespace:foo, namespace:baz"}
	for _, ns := range []string{"foo", "baz"} {
		if err := authorize(p, ns, "p", "read"); err != nil {
			t.Fatalf("multi-grant should allow %q, got %v", ns, err)
		}
	}
	if err := authorize(p, "bar", "p", "read"); err == nil {
		t.Fatalf("multi-grant should deny bar, got allow")
	}
}

func TestActionFor(t *testing.T) {
	if actionFor("write_file") != "write" || actionFor("move_file") != "write" {
		t.Fatal("mutating tools should map to write")
	}
	if actionFor("read_file") != "read" || actionFor("list_files") != "read" {
		t.Fatal("non-mutating tools should map to read")
	}
}

// callToolReq builds a tools/call request with the given tool name and raw args.
func callToolReq(name, argsJSON string) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: name, Arguments: json.RawMessage(argsJSON)},
	}
}

// TestAuthzMiddleware_GatesToolsCall asserts the installed middleware intercepts
// tools/call: a *-scope (or absent) principal passes through to next, while a
// scoped principal targeting a foreign namespace is denied WITHOUT reaching next.
func TestAuthzMiddleware_GatesToolsCall(t *testing.T) {
	var nextCalled bool
	next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		nextCalled = true
		return &mcp.CallToolResult{}, nil
	}
	gated := AuthzMiddleware()(next)

	// Allowed: * principal reaches next.
	nextCalled = false
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Scope: "*"})
	res, err := gated(ctx, "tools/call", callToolReq("read_file", `{"namespace":"bar","project_name":"x"}`))
	if err != nil || !nextCalled {
		t.Fatalf("star principal should pass through: nextCalled=%v err=%v", nextCalled, err)
	}
	if ctr, ok := res.(*mcp.CallToolResult); ok && ctr.IsError {
		t.Fatalf("star principal should not be an error result")
	}

	// Denied: scoped principal to a foreign namespace is blocked before next.
	nextCalled = false
	ctx = auth.WithPrincipal(context.Background(), auth.Principal{Scope: "namespace:foo"})
	res, err = gated(ctx, "tools/call", callToolReq("read_file", `{"namespace":"bar","project_name":"x"}`))
	if err != nil {
		t.Fatalf("denial should be an IsError result, not a transport error: %v", err)
	}
	if nextCalled {
		t.Fatal("denied call must NOT reach next")
	}
	ctr, ok := res.(*mcp.CallToolResult)
	if !ok || !ctr.IsError {
		t.Fatalf("denied call should return an IsError CallToolResult, got %T", res)
	}
}

// TestAuthzMiddleware_NonToolsCallPassthrough asserts the gate no-ops for other
// methods (e.g. tools/list), exactly like CoreFirstToolsMiddleware's method guard.
func TestAuthzMiddleware_NonToolsCallPassthrough(t *testing.T) {
	var nextCalled bool
	next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		nextCalled = true
		return &mcp.ListToolsResult{}, nil
	}
	gated := AuthzMiddleware()(next)

	// Even a scoped principal does not gate a non-tools/call method.
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Scope: "namespace:foo"})
	if _, err := gated(ctx, "tools/list", &mcp.ListToolsRequest{}); err != nil {
		t.Fatalf("tools/list should pass through: %v", err)
	}
	if !nextCalled {
		t.Fatal("non-tools/call method must reach next unconditionally")
	}
}
