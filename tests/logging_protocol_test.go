package tests

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shoka/mcp-server/internal/auth"
	"github.com/shoka/mcp-server/internal/httplog"
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/tools"
	"github.com/stretchr/testify/require"
)

// TestLogging_Protocol drives a real authenticated Streamable HTTP session at
// DEBUG and asserts the four protocol layers (§3.1 request, §3.2 response, §3.3
// session establishment, §3.4 SDK session state) are visible, with §4 redaction
// applied to write_file content and read_file result content. The secret
// invariant (content + token never logged) is also re-checked here.
func TestLogging_Protocol(t *testing.T) {
	const (
		token   = "PROTO-SECRET-BEARER-7e8f9a"
		content = "PROTOCOL-TEST-FILE-BODY-never-log-44d1c2"
	)

	sink := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))

	s, err := storage.NewFSGitStorage(t.TempDir())
	require.NoError(t, err)
	defer s.Close()
	s.SetLogger(logger)

	srv := mcp.NewServer(&mcp.Implementation{Name: "shoka-proto-test", Version: "0.0.0"}, &mcp.ServerOptions{Logger: logger})
	mcp.AddTool(srv, &mcp.Tool{Name: "create_project"}, tools.LoggedTool(logger, "create_project", tools.CreateProjectHandler(s)))
	mcp.AddTool(srv, &mcp.Tool{Name: "write_file"}, tools.LoggedTool(logger, "write_file", tools.WriteFileHandler(s)))
	mcp.AddTool(srv, &mcp.Tool{Name: "read_file"}, tools.LoggedTool(logger, "read_file", tools.ReadFileHandler(s)))

	// Production-shaped chain: logging outermost, then auth, then the Streamable
	// HTTP handler.
	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return srv }, nil)
	a := auth.New(auth.Config{Enabled: true, Tokens: []string{token}})
	handler := httplog.Middleware(logger)(a.Middleware(mcpHandler))

	httpSrv := httptest.NewServer(handler)
	defer httpSrv.Close()

	client := &http.Client{Transport: bearerRT{base: http.DefaultTransport, token: token}}
	cli := mcp.NewClient(&mcp.Implementation{Name: "proto-test-client", Version: "0.0.0"}, nil)
	sess, err := cli.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: httpSrv.URL, HTTPClient: client}, nil)
	require.NoError(t, err)
	defer sess.Close()

	r, err := sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_project", Arguments: map[string]any{"project_name": "p"}})
	require.NoError(t, err)
	require.False(t, r.IsError, wireText(r))

	r, err = sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "write_file", Arguments: map[string]any{"project_name": "p", "path": "a.md", "content": content}})
	require.NoError(t, err)
	require.False(t, r.IsError, wireText(r))

	r, err = sess.CallTool(context.Background(), &mcp.CallToolParams{Name: "read_file", Arguments: map[string]any{"project_name": "p", "path": "a.md"}})
	require.NoError(t, err)
	require.False(t, r.IsError, wireText(r))

	logs := sink.String()
	if os.Getenv("SHOKA_DUMP_LOGS") != "" {
		t.Log("\n--- BEGIN CAPTURED DEBUG LOG ---\n" + logs + "\n--- END CAPTURED DEBUG LOG ---")
	}

	// Invariants (must hold under the new logging — §4 / §7.2).
	require.NotContains(t, logs, content, "file content must never appear in logs")
	require.NotContains(t, logs, token, "bearer token must never appear in logs")

	// §3.1 request visibility + write_file content redaction.
	require.Contains(t, logs, "mcp message received", "§3.1 request line")
	require.Contains(t, logs, "rpc_method=tools/call")
	require.Contains(t, logs, "rpc_id=")
	require.Contains(t, logs, "<redacted ", "write_file content must be redacted with a byte-length placeholder")
	require.Contains(t, logs, "a.md", "non-secret path stays verbatim in params")

	// §3.2 response visibility + §3.3 session establishment. Streamable HTTP has no
	// SSE "endpoint" event; the session id is assigned on the initialize response's
	// Mcp-Session-Id header, which the middleware surfaces as "mcp session
	// established" with a session_id attribute.
	require.Contains(t, logs, "mcp response sent", "§3.2 response line")
	require.Contains(t, logs, "mcp session established", "§3.3 assigned session id must be logged")
	require.Contains(t, logs, "session_id=", "session id correlation value must be visible")

	// §3.4 session state (emitted by the SDK via the wired logger).
	require.Contains(t, logs, "server session connected")
	require.Contains(t, logs, "session initialized")
}
