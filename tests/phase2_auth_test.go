package tests

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shoka/mcp-server/internal/auth"
	"github.com/shoka/mcp-server/internal/drafts"
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/ui"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAuth_DraftsEndpoint_RequiresTokenAndOrigin(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "shoka-auth-drafts-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	dm, err := drafts.NewManager(tempDir)
	require.NoError(t, err)

	a := auth.New(auth.Config{
		Enabled:        true,
		Tokens:         []string{"secret"},
		AllowedOrigins: []string{"http://allowed.example"},
	})
	dm.SetOriginChecker(a.OriginAllowed)

	server := httptest.NewServer(a.MiddlewareAllowQueryToken(dm))
	defer server.Close()

	base := "ws" + strings.TrimPrefix(server.URL, "http") + "/drafts/ns1/proj1?filepath=test.md"

	// 1. No token -> handshake rejected with 401 before upgrade.
	_, resp, err := websocket.DefaultDialer.Dial(base, nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// 2. Valid token but missing/disallowed origin -> rejected by CheckOrigin (403).
	tokenURL := base + "&token=secret"
	_, resp, err = websocket.DefaultDialer.Dial(tokenURL, nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode)

	// 3. Valid token + allowed origin -> upgrade succeeds.
	hdr := http.Header{}
	hdr.Set("Origin", "http://allowed.example")
	conn, resp, err := websocket.DefaultDialer.Dial(tokenURL, hdr)
	require.NoError(t, err)
	defer conn.Close()
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
}

func TestAuth_UIEndpoint_RequiresTokenAndOrigin(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "shoka-auth-ui-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	s, err := storage.NewFSGitStorage(tempDir)
	require.NoError(t, err)
	defer s.Close()
	dm, err := drafts.NewManager(tempDir)
	require.NoError(t, err)
	uim := ui.NewManager(s, dm, nil)

	a := auth.New(auth.Config{
		Enabled:        true,
		Tokens:         []string{"secret"},
		AllowedOrigins: []string{"http://allowed.example"},
	})
	uim.SetOriginChecker(a.OriginAllowed)

	server := httptest.NewServer(a.MiddlewareAllowQueryToken(uim))
	defer server.Close()

	base := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/ui"

	// No token -> 401.
	_, resp, err := websocket.DefaultDialer.Dial(base, nil)
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Valid token + allowed origin -> upgrade succeeds.
	hdr := http.Header{}
	hdr.Set("Origin", "http://allowed.example")
	conn, resp, err := websocket.DefaultDialer.Dial(base+"?token=secret", hdr)
	require.NoError(t, err)
	defer conn.Close()
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
}

func TestAuth_MCPEndpoint_RequiresToken(t *testing.T) {
	a := auth.New(auth.Config{Enabled: true, Tokens: []string{"secret"}})
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0.0.0"}, nil)
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server { return mcpSrv }, nil)

	server := httptest.NewServer(a.Middleware(h))
	defer server.Close()

	// No token -> 401.
	resp, err := http.Get(server.URL)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)

	// Valid token -> the request reaches the MCP handler (no longer 401). The
	// handler itself may answer with a 4xx other than 401 (e.g. a bare GET lacks
	// the Accept: text/event-stream the Streamable HTTP transport requires); the
	// point here is only that auth let it through.
	req, err := http.NewRequest(http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer secret")
	resp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp2.Body.Close()
	assert.NotEqual(t, http.StatusUnauthorized, resp2.StatusCode)
}

// Disabled auth must preserve current behaviour: no token, any origin works.
func TestAuth_Disabled_AllowsConnections(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "shoka-auth-off-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	dm, err := drafts.NewManager(tempDir)
	require.NoError(t, err)

	a := auth.New(auth.Config{Enabled: false})
	dm.SetOriginChecker(a.OriginAllowed)

	server := httptest.NewServer(a.MiddlewareAllowQueryToken(dm))
	defer server.Close()

	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/drafts/ns1/proj1?filepath=test.md"
	conn, resp, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	defer conn.Close()
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
}
