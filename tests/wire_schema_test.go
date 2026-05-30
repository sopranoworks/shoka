package tests

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/tools"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wireStart boots an in-process MCP server over the real Streamable HTTP
// transport (auth disabled) with the file/project/discovery tools registered, and
// connects a real MCP client. This exercises the actual wire-level
// argument-schema validation — the layer the handler-direct unit tests bypass,
// which is how F2 (optional fields wrongly required) shipped undetected.
func wireStart(t *testing.T) (*mcp.ClientSession, func()) {
	t.Helper()
	s, err := storage.NewFSGitStorage(t.TempDir())
	require.NoError(t, err)

	srv := mcp.NewServer(&mcp.Implementation{Name: "shoka-wire-test", Version: "0.0.0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "create_project"}, tools.CreateProjectHandler(s))
	mcp.AddTool(srv, &mcp.Tool{Name: "read_file"}, tools.ReadFileHandler(s))
	mcp.AddTool(srv, &mcp.Tool{Name: "write_file"}, tools.WriteFileHandler(s))
	mcp.AddTool(srv, &mcp.Tool{Name: "delete_file"}, tools.DeleteFileHandler(s))
	mcp.AddTool(srv, &mcp.Tool{Name: "list_files"}, tools.ListFilesHandler(s))
	mcp.AddTool(srv, &mcp.Tool{Name: "get_history"}, tools.GetHistoryHandler(s))
	mcp.AddTool(srv, &mcp.Tool{Name: "read_summary"}, tools.ReadSummaryHandler(s))
	mcp.AddTool(srv, &mcp.Tool{Name: "list_files_since"}, tools.ListFilesSinceHandler(s))
	mcp.AddTool(srv, &mcp.Tool{Name: "search_files"}, tools.SearchFilesHandler(s))

	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return srv }, nil)
	httpSrv := httptest.NewServer(h)

	cli := mcp.NewClient(&mcp.Implementation{Name: "wire-test-client", Version: "0.0.0"}, nil)
	sess, err := cli.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: httpSrv.URL}, nil)
	require.NoError(t, err)

	return sess, func() { sess.Close(); httpSrv.Close(); s.Close() }
}

func wireCall(t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err, "transport-level call %s", name)
	return res
}

func wireText(res *mcp.CallToolResult) string {
	out := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out += tc.Text
		}
	}
	return out
}

func wireStructString(res *mcp.CallToolResult, key string) string {
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// TestWireSchema_OptionalFieldsMayBeOmitted is the F2 regression fixture: every
// field documented as optional must be omittable over the wire. If any silently
// becomes required again, the corresponding sub-assertion fails.
func TestWireSchema_OptionalFieldsMayBeOmitted(t *testing.T) {
	sess, cleanup := wireStart(t)
	defer cleanup()

	// create_project without namespace -> default namespace.
	r := wireCall(t, sess, "create_project", map[string]any{"project_name": "p"})
	assert.False(t, r.IsError, "create_project (omit namespace): %s", wireText(r))

	// write_file without namespace AND without expected_version.
	r = wireCall(t, sess, "write_file", map[string]any{"project_name": "p", "path": "a.md", "content": "hello world"})
	assert.False(t, r.IsError, "write_file (omit namespace + expected_version): %s", wireText(r))

	// read_file without namespace; capture version for the locking checks.
	r = wireCall(t, sess, "read_file", map[string]any{"project_name": "p", "path": "a.md"})
	assert.False(t, r.IsError, "read_file (omit namespace): %s", wireText(r))
	ver := wireStructString(r, "version")
	require.NotEmpty(t, ver, "read_file should report a version")

	// write_file WITH a correct expected_version still works (present-field path).
	r = wireCall(t, sess, "write_file", map[string]any{"project_name": "p", "path": "a.md", "content": "v2", "expected_version": ver})
	assert.False(t, r.IsError, "write_file (correct expected_version): %s", wireText(r))

	// write_file WITH a stale expected_version still conflicts (locking preserved).
	r = wireCall(t, sess, "write_file", map[string]any{"project_name": "p", "path": "a.md", "content": "v3", "expected_version": "1111111111111111111111111111111111111111"})
	assert.True(t, r.IsError, "write_file (stale expected_version) must still conflict")

	// delete_file without expected_version.
	r = wireCall(t, sess, "write_file", map[string]any{"project_name": "p", "path": "del.md", "content": "x"})
	require.False(t, r.IsError, "setup write for delete: %s", wireText(r))
	r = wireCall(t, sess, "delete_file", map[string]any{"project_name": "p", "path": "del.md"})
	assert.False(t, r.IsError, "delete_file (omit expected_version): %s", wireText(r))

	// list_files without include_versions / include_summaries.
	r = wireCall(t, sess, "list_files", map[string]any{"project_name": "p"})
	assert.False(t, r.IsError, "list_files (omit includes): %s", wireText(r))
	// list_files WITH include_versions=true still works (present-field path).
	r = wireCall(t, sess, "list_files", map[string]any{"project_name": "p", "include_versions": true})
	assert.False(t, r.IsError, "list_files (include_versions=true): %s", wireText(r))

	// get_history without since.
	r = wireCall(t, sess, "get_history", map[string]any{"project_name": "p"})
	assert.False(t, r.IsError, "get_history (omit since): %s", wireText(r))

	// read_summary without namespace.
	r = wireCall(t, sess, "read_summary", map[string]any{"project_name": "p", "path": "a.md"})
	assert.False(t, r.IsError, "read_summary (omit namespace): %s", wireText(r))

	// search_files without namespace AND without search_in.
	r = wireCall(t, sess, "search_files", map[string]any{"project_name": "p", "query": "hello"})
	assert.False(t, r.IsError, "search_files (omit namespace + search_in): %s", wireText(r))

	// list_files_since without namespace (since is genuinely required, so provide it).
	r = wireCall(t, sess, "list_files_since", map[string]any{"project_name": "p", "since": "1970-01-01T00:00:00Z"})
	assert.False(t, r.IsError, "list_files_since (omit namespace): %s", wireText(r))
}
