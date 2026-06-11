package tools

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// orderTestIn/Out are trivial object schemas for stub tools — the ordering test
// only cares about tools/list order and tools/call dispatch, not tool behaviour.
type orderTestIn struct{}
type orderTestOut struct{}

// TestCoreFirstToolsMiddleware_listOrderAndDispatch registers tools in a
// deliberately non-core-first order, then verifies via a real in-memory
// tools/list round-trip that the eight core tools come first in the fixed order,
// no tool is dropped, the non-core tail keeps its (alphabetical) order, and both
// a core and a non-core tool still dispatch through tools/call. (B-49 fix-1.)
func TestCoreFirstToolsMiddleware_listOrderAndDispatch(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "shoka-test", Version: "0"}, nil)
	srv.AddReceivingMiddleware(CoreFirstToolsMiddleware())

	// Register in alphabetical order (the SDK's natural order) so the test proves
	// the middleware actively reorders rather than passively inheriting input order.
	// Mix of all eight core tools and several non-core tools.
	names := []string{
		"append_to_file",  // core
		"create_project",  // non-core
		"delete_file",     // non-core
		"get_history",     // core
		"get_server_info", // non-core
		"list_files",      // core
		"patch_file",      // core
		"read_file",       // core
		"read_summary",    // core
		"search_files",    // core
		"subscribe",       // non-core
		"write_file",      // core
	}
	called := map[string]bool{}
	for _, n := range names {
		n := n
		mcp.AddTool(srv, &mcp.Tool{Name: n, Description: "stub"},
			func(ctx context.Context, req *mcp.CallToolRequest, _ orderTestIn) (*mcp.CallToolResult, orderTestOut, error) {
				called[n] = true
				return &mcp.CallToolResult{}, orderTestOut{}, nil
			})
	}

	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}

	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if res.NextCursor != "" {
		t.Fatalf("expected single page (empty NextCursor), got %q", res.NextCursor)
	}

	got := make([]string, 0, len(res.Tools))
	for _, tl := range res.Tools {
		got = append(got, tl.Name)
	}

	// 1. The first eight must be the fixed core order.
	wantCore := []string{
		"read_file", "write_file", "list_files", "read_summary",
		"search_files", "get_history", "patch_file", "append_to_file",
	}
	if len(got) < len(wantCore) {
		t.Fatalf("too few tools returned: %v", got)
	}
	for i, want := range wantCore {
		if got[i] != want {
			t.Errorf("core position %d: got %q, want %q (full order: %v)", i, got[i], want, got)
		}
	}

	// 2. Completeness: every registered tool is still present (none dropped).
	if len(got) != len(names) {
		t.Errorf("tool count changed: got %d, want %d (%v)", len(got), len(names), got)
	}
	seen := map[string]bool{}
	for _, n := range got {
		seen[n] = true
	}
	for _, n := range names {
		if !seen[n] {
			t.Errorf("tool %q dropped from tools/list", n)
		}
	}

	// 3. The non-core tail keeps its existing (alphabetical) relative order.
	wantTail := []string{"create_project", "delete_file", "get_server_info", "subscribe"}
	gotTail := got[len(wantCore):]
	if len(gotTail) != len(wantTail) {
		t.Fatalf("tail length: got %v, want %v", gotTail, wantTail)
	}
	for i := range wantTail {
		if gotTail[i] != wantTail[i] {
			t.Errorf("tail position %d: got %q, want %q (tail: %v)", i, gotTail[i], wantTail[i], gotTail)
		}
	}

	// 4. Dispatch intact: a core and a non-core tool both still invoke.
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "read_file", Arguments: map[string]any{}}); err != nil {
		t.Errorf("CallTool read_file (core): %v", err)
	}
	if _, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "get_server_info", Arguments: map[string]any{}}); err != nil {
		t.Errorf("CallTool get_server_info (non-core): %v", err)
	}
	if !called["read_file"] {
		t.Error("core tool read_file handler was not invoked")
	}
	if !called["get_server_info"] {
		t.Error("non-core tool get_server_info handler was not invoked")
	}
}

// TestCoreFirstToolsMiddleware_nonListMethodUntouched ensures the middleware only
// reorders tools/list and passes every other method through unchanged.
func TestCoreFirstToolsMiddleware_nonListMethodUntouched(t *testing.T) {
	mw := CoreFirstToolsMiddleware()
	var sawMethod string
	next := func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		sawMethod = method
		return &mcp.CallToolResult{}, nil
	}
	h := mw(next)
	res, err := h(context.Background(), "tools/call", nil)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if sawMethod != "tools/call" {
		t.Fatalf("inner handler not called with original method, got %q", sawMethod)
	}
	if _, ok := res.(*mcp.CallToolResult); !ok {
		t.Fatalf("result altered for non-list method: %T", res)
	}
}
