package tests

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sopranoworks/shoka/internal/httplog"
	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/internal/tools"
	"github.com/sopranoworks/shoka/pkg/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncBuffer is a goroutine-safe log sink (the transport serves on background goroutines).
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// bearerRT injects an Authorization header on every request (the SDK's
// StreamableClientTransport has no header field, only a custom http.Client).
type bearerRT struct {
	base  http.RoundTripper
	token string
}

func (rt bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	// Per the http.RoundTripper contract, do not mutate the incoming request;
	// clone it before adding the Authorization header.
	r2 := r.Clone(r.Context())
	r2.Header.Set("Authorization", "Bearer "+rt.token)
	return rt.base.RoundTrip(r2)
}

// TestLogging_NeverLeaksContentOrToken drives a real authenticated Streamable
// HTTP session through the full logging stack (transport middleware + SDK session
// logger + tool wrapper) at DEBUG level and asserts neither secret appears in any
// log.
func TestLogging_NeverLeaksContentOrToken(t *testing.T) {
	const (
		token   = "SUPER-SECRET-BEARER-TOKEN-a1b2c3"
		content = "TOP-SECRET-DOCUMENT-BODY-do-not-log-9f3a7c"
	)

	sink := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug}))

	s, err := storage.NewFSGitStorage(t.TempDir())
	require.NoError(t, err)
	defer s.Close()
	s.SetLogger(logger)

	srv := mcp.NewServer(&mcp.Implementation{Name: "shoka-secret-test", Version: "0.0.0"}, &mcp.ServerOptions{Logger: logger})
	mcp.AddTool(srv, &mcp.Tool{Name: "create_project"}, tools.LoggedTool(logger, "create_project", tools.CreateProjectHandler(s)))
	mcp.AddTool(srv, &mcp.Tool{Name: "write_file"}, tools.LoggedTool(logger, "write_file", tools.WriteFileHandler(s)))

	// Production-shaped chain: logging outermost, then auth, then the Streamable
	// HTTP handler.
	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return srv }, nil)
	a := auth.New(auth.Config{Enabled: true, Tokens: []string{token}})
	handler := httplog.Middleware(logger)(a.Middleware(mcpHandler))

	httpSrv := httptest.NewServer(handler)
	defer httpSrv.Close()

	client := &http.Client{Transport: bearerRT{base: http.DefaultTransport, token: token}}
	cli := mcp.NewClient(&mcp.Implementation{Name: "secret-test-client", Version: "0.0.0"}, nil)
	sess, err := cli.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: httpSrv.URL, HTTPClient: client}, nil)
	require.NoError(t, err, "authenticated Streamable HTTP connect must succeed (proves auth+flusher survive logging)")
	defer sess.Close()

	r, err := sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "create_project", Arguments: map[string]any{"project_name": "p"},
	})
	require.NoError(t, err)
	require.False(t, r.IsError, wireText(r))

	r, err = sess.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "write_file", Arguments: map[string]any{"project_name": "p", "path": "a.md", "content": content},
	})
	require.NoError(t, err)
	require.False(t, r.IsError, wireText(r))

	// Let the background commit finish so its log line is included in the
	// leak check (the commit logs the hash+path, never the content).
	s.WaitForWAL(10 * time.Second)

	logs := sink.String()
	require.NotEmpty(t, logs, "expected DEBUG logs to be produced")
	// Sanity: the stack actually logged tool activity.
	assert.Contains(t, logs, "tool call received")
	assert.Contains(t, logs, "write_file")
	// The two assertions that matter:
	assert.NotContains(t, logs, content, "file content must never appear in logs")
	assert.NotContains(t, logs, token, "bearer token must never appear in logs")
}
