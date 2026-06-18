package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/auth"
	"github.com/sopranoworks/shoka/internal/authz"
)

// B-28 stage-2 flip: the MCP middleware looks up each tool's required level from the
// registry and enforces it via the shared authz.Authorize (the same function the
// /ws/ui gate calls). The decision logic itself is tested in internal/authz; here we
// test the registry + the middleware behaviour.

func TestToolLevel_Registry(t *testing.T) {
	cases := map[string]authz.Level{
		"read_file":       authz.LevelRead,
		"list_projects":   authz.LevelRead,
		"write_file":      authz.LevelWrite,
		"move_file":       authz.LevelWrite,
		"recover_project": authz.LevelAdmin, // the formerly-unclassified gap
		// B-28 ns/proj management part 1: project create/delete = admin on the target
		// namespace (create RAISED from write); namespace create/delete carry admin in the
		// registry and their handlers tighten to super-user.
		"create_project":   authz.LevelAdmin,
		"delete_project":   authz.LevelAdmin,
		"create_namespace": authz.LevelAdmin,
		"delete_namespace": authz.LevelAdmin,
	}
	for tool, want := range cases {
		if got := toolLevel(tool); got != want {
			t.Errorf("toolLevel(%q) = %v, want %v", tool, got, want)
		}
	}
	// Fail-closed: an unregistered tool requires admin.
	if got := toolLevel("some_future_tool"); got != authz.LevelAdmin {
		t.Errorf("unregistered tool must fail closed to admin, got %v", got)
	}
	// translate_file was RETIRED (B-28): it is no longer in the level registry, so
	// it now falls through to the fail-closed admin default rather than its former
	// write level — a positive check that the retired tool is gone from the surface.
	if got := toolLevel("translate_file"); got != authz.LevelAdmin {
		t.Errorf("retired translate_file must fall through to fail-closed admin, got %v", got)
	}
}

func callToolReq(name, argsJSON string) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: name, Arguments: json.RawMessage(argsJSON)},
	}
}

// runGate drives the middleware and reports whether the call reached next (allowed)
// and the result.
func runGate(t *testing.T, scope, tool, argsJSON string) (reached bool, res mcp.Result) {
	t.Helper()
	next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		reached = true
		return &mcp.CallToolResult{}, nil
	}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Scope: scope})
	r, err := AuthzMiddleware()(next)(ctx, "tools/call", callToolReq(tool, argsJSON))
	if err != nil {
		t.Fatalf("denial must be an IsError result, not a transport error: %v", err)
	}
	return reached, r
}

func isError(res mcp.Result) bool {
	ctr, ok := res.(*mcp.CallToolResult)
	return ok && ctr.IsError
}

// TestAuthzMiddleware_NoRegression: a super-user ("*") passes every tool.
func TestAuthzMiddleware_NoRegression(t *testing.T) {
	for _, tool := range []string{"read_file", "write_file", "delete_file", "recover_project", "list_projects"} {
		reached, res := runGate(t, "*", tool, `{"namespace":"bar","project_name":"x"}`)
		if !reached || isError(res) {
			t.Fatalf("super-user must pass %q (reached=%v isErr=%v)", tool, reached, isError(res))
		}
	}
}

// TestAuthzMiddleware_ScopedFlip is the core RED→GREEN flip: a namespace:foo:r token
// reads foo, is DENIED write_file on foo and ALL ops on bar, and recover_project needs
// admin. Before the flip the dormant gate *-passed everything / ignored the level.
func TestAuthzMiddleware_ScopedFlip(t *testing.T) {
	const scope = "namespace:foo:r"

	// read foo → allowed
	if reached, res := runGate(t, scope, "read_file", `{"namespace":"foo","project_name":"x"}`); !reached || isError(res) {
		t.Fatal("read-only foo must permit read_file on foo")
	}
	// write_file foo → denied (read-only)
	if reached, res := runGate(t, scope, "write_file", `{"namespace":"foo","project_name":"x"}`); reached || !isError(res) {
		t.Fatal("read-only foo must DENY write_file on foo")
	}
	// delete_file foo → denied
	if reached, res := runGate(t, scope, "delete_file", `{"namespace":"foo","project_name":"x"}`); reached || !isError(res) {
		t.Fatal("read-only foo must DENY delete_file on foo")
	}
	// read_file bar → denied (foreign namespace)
	if reached, res := runGate(t, scope, "read_file", `{"namespace":"bar","project_name":"x"}`); reached || !isError(res) {
		t.Fatal("foo-only must DENY any access to bar")
	}
	// recover_project foo → denied (needs admin)
	if reached, res := runGate(t, scope, "recover_project", `{"namespace":"foo","project_name":"x"}`); reached || !isError(res) {
		t.Fatal("recover_project must require admin")
	}
}

// TestAuthzMiddleware_RWCannotAdmin: a read-write token may write but not recover.
func TestAuthzMiddleware_RWCannotAdmin(t *testing.T) {
	const scope = "namespace:foo:rw"
	if reached, _ := runGate(t, scope, "write_file", `{"namespace":"foo","project_name":"x"}`); !reached {
		t.Fatal("rw foo must permit write_file on foo")
	}
	if reached, res := runGate(t, scope, "recover_project", `{"namespace":"foo","project_name":"x"}`); reached || !isError(res) {
		t.Fatal("rw foo must DENY recover_project (admin)")
	}
}

// TestAuthzMiddleware_NonToolsCallPassthrough: the gate no-ops for other methods.
func TestAuthzMiddleware_NonToolsCallPassthrough(t *testing.T) {
	var nextCalled bool
	next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		nextCalled = true
		return &mcp.ListToolsResult{}, nil
	}
	ctx := auth.WithPrincipal(context.Background(), auth.Principal{Scope: "namespace:foo:r"})
	if _, err := AuthzMiddleware()(next)(ctx, "tools/list", &mcp.ListToolsRequest{}); err != nil {
		t.Fatalf("tools/list should pass through: %v", err)
	}
	if !nextCalled {
		t.Fatal("non-tools/call must reach next unconditionally")
	}
}

// TestAuthzMiddleware_DeletedLogAdminOnly: list_deleted/revive_file are admin on
// the target namespace (B-28 deleted-log). A read-write principal is denied; the
// namespace admin and the super-user pass.
func TestAuthzMiddleware_DeletedLogAdminOnly(t *testing.T) {
	if got := toolLevel("list_deleted"); got != authz.LevelAdmin {
		t.Errorf("list_deleted level = %v, want admin", got)
	}
	if got := toolLevel("revive_file"); got != authz.LevelAdmin {
		t.Errorf("revive_file level = %v, want admin", got)
	}
	for _, tool := range []string{"list_deleted", "revive_file"} {
		args := `{"namespace":"foo","project_name":"x","path":"a.md"}`
		// read-write on foo → denied (needs admin)
		if reached, res := runGate(t, "namespace:foo:rw", tool, args); reached || !isError(res) {
			t.Fatalf("%s must require admin (rw denied)", tool)
		}
		// admin on a DIFFERENT namespace → denied
		if reached, res := runGate(t, "namespace:bar:admin", tool, args); reached || !isError(res) {
			t.Fatalf("%s on foo must be denied to a bar-admin", tool)
		}
		// admin on foo → allowed
		if reached, res := runGate(t, "namespace:foo:admin", tool, args); !reached || isError(res) {
			t.Fatalf("%s on foo must be allowed to a foo-admin", tool)
		}
		// super-user → allowed
		if reached, res := runGate(t, "*", tool, args); !reached || isError(res) {
			t.Fatalf("%s must be allowed to a super-user", tool)
		}
	}
}
