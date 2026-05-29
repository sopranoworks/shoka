package tests

// Shared helpers for the live Streamable-HTTP session-behavior coverage tests
// (Scenarios A, B, C, mirroring the SSE suite they replace). These extend — never
// duplicate — the infrastructure in live_http_interop_test.go (TestMain,
// startLiveServer, freePort, writeLiveConfig, mcpEndpoint, hasNoRequiredArgs).
// Like the rest of the live suite they encode NO prior knowledge of Shoka's tool
// catalog: tool names are discovered via tools/list and selected by schema
// introspection, never written as literals.

import (
	"context"
	"net/http"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectClient opens one real Streamable HTTP client session against a live
// Shoka MCP endpoint (Connect performs the initialize handshake) and returns the
// session. httpClient may be nil. The caller is responsible for Close.
func connectClient(ctx context.Context, t *testing.T, mcpURL string, httpClient *http.Client, name string) *mcp.ClientSession {
	t.Helper()
	transport := &mcp.StreamableClientTransport{Endpoint: mcpURL, HTTPClient: httpClient}
	client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "0.0.1"}, nil)
	sess, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("Connect (initialize handshake) failed: %v", err)
	}
	return sess
}

// discoverNoArgTools lists the tools on a session and returns the names of every
// tool whose input schema declares no required fields (i.e. callable with empty
// arguments), in the order tools/list reports them. hasNoRequiredArgs is defined
// in live_http_interop_test.go (same package).
func discoverNoArgTools(ctx context.Context, t *testing.T, sess *mcp.ClientSession) []string {
	t.Helper()
	lt, err := sess.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	var names []string
	for _, tool := range lt.Tools {
		if hasNoRequiredArgs(tool.InputSchema) {
			names = append(names, tool.Name)
		}
	}
	return names
}
