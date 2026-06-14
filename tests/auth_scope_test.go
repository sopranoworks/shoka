package tests

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sopranoworks/shoka/internal/auth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// F1: the ?token= query fallback must be scoped to the WebSocket endpoints. On
// the MCP endpoint (wrapped by the header-only Middleware) a query token must NOT
// authenticate; only Authorization: Bearer is accepted.
func TestAuth_MCP_RejectsQueryToken(t *testing.T) {
	a := auth.New(auth.Config{Enabled: true, Tokens: []string{"secret"}})
	h := a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv := httptest.NewServer(h)
	defer srv.Close()

	// ?token= with no Authorization header -> 401 (query fallback not honored on MCP).
	resp, err := http.Get(srv.URL + "/mcp?token=secret")
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode, "query token must not authenticate on the MCP endpoint")

	// Authorization: Bearer -> 200.
	req, err := http.NewRequest(http.MethodGet, srv.URL+"/mcp", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer secret")
	resp2, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode, "Bearer header must authenticate on the MCP endpoint")
}
