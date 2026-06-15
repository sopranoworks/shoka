package tests

// Live, wire-level coverage for the B-50 phase-2 two-MCP-transport wiring: the
// real Shoka binary is started with each config-presence case and driven over the
// network, asserting (1) which listeners bind for plain-only / oauth-only / both,
// (2) per-port auth — the OAuth port is pure OAuth (enforces tokens, serves
// discovery unauthenticated, rejects a static bearer) while the plain port is
// static-bearer-iff-bearer_auth or unauthenticated, and (3) a tool round-trip
// succeeds on each opened port. These extend the live harness in
// live_http_interop_test.go (TestMain, startLiveServer, freePort, runLifecycle,
// mcpEndpoint, waitMCPPort) and reuse bearerRT from logging_secret_test.go.

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/serverurl"
	"github.com/sopranoworks/shoka/internal/storage/oauthstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTwoTransportConfig writes a Shoka config opening the plain transport (when
// plainPort>0), the OAuth transport (when oauthPort>0), or both. authTokens
// populates server.auth.tokens (the plain port's static-bearer source); a
// non-empty list also sets server.auth.enabled. It returns the config path.
func writeTwoTransportConfig(t *testing.T, baseDir string, httpPort, plainPort int, plainBearer bool, oauthPort int, authTokens []string) string {
	t.Helper()

	mcpBlock := ""
	if plainPort > 0 {
		mcpBlock += fmt.Sprintf(`    plain:
      listen: "127.0.0.1:%d"
      external_url: "http://127.0.0.1:%d"
      bearer_auth: %t
`, plainPort, plainPort, plainBearer)
	}
	if oauthPort > 0 {
		mcpBlock += fmt.Sprintf(`    oauth:
      listen: "127.0.0.1:%d"
      external_url: "http://127.0.0.1:%d"
      consent_credential: "test-consent"
      trusted_client_metadata_domains: []
`, oauthPort, oauthPort)
	}

	tokensYAML := "[]"
	if len(authTokens) > 0 {
		tokensYAML = "["
		for i, tk := range authTokens {
			if i > 0 {
				tokensYAML += ", "
			}
			tokensYAML += fmt.Sprintf("%q", tk)
		}
		tokensYAML += "]"
	}

	cfg := fmt.Sprintf(`server:
  http:
    listen: "127.0.0.1:%d"
    external_url: "http://127.0.0.1:%d"
  mcp:
%s  auth:
    enabled: %t
    tokens: %s
    allowed_origins: []
  log:
    level: "debug"
    format: "text"
storage:
  base_dir: %q
services:
  google_cloud:
    project_id: ""
`, httpPort, httpPort, mcpBlock, len(authTokens) > 0, tokensYAML, baseDir)

	path := filepath.Join(t.TempDir(), "shoka.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatalf("writeTwoTransportConfig: %v", err)
	}
	return path
}

// seedOAuthToken opens the oauthstore the server will use (<baseDir>/oauth.db),
// mints one access token, and closes it BEFORE the server starts (bbolt is
// single-writer). The server then opens the same db and its OAuth-enforcing
// authenticator validates this token via Lookup. Returns the access token.
func seedOAuthToken(t *testing.T, baseDir string) string {
	t.Helper()
	st, err := oauthstore.Open(filepath.Join(baseDir, "oauth.db"))
	require.NoError(t, err)
	rec, err := st.NewSeries(
		"test-client",
		oauthstore.Principal{Name: "Test Operator", Email: "op@test.local"},
		"http://127.0.0.1/mcp",
		"*",
		time.Now(),
		time.Hour,
		24*time.Hour,
	)
	require.NoError(t, err)
	require.NoError(t, st.Close())
	return rec.AccessToken
}

// portBound reports whether a TCP listener accepts connections on the loopback
// port within a short window.
func portBound(port int, timeout time.Duration) bool {
	return waitMCPPort(port, timeout)
}

// getStatus performs a bare GET against http://127.0.0.1:<port><path> with the
// optional bearer token and returns the status code.
func getStatus(t *testing.T, port int, path, bearer string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d%s", port, path), nil)
	require.NoError(t, err)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	// A dedicated client with keep-alives disabled, NOT the shared
	// http.DefaultClient. The default client pools idle keep-alive connections
	// keyed by host:port; across this suite's many short-lived servers on
	// ephemeral ports — which recycle under the whole-module gate — a pooled
	// connection to a now-stopped server can be reused for a fresh server that
	// happens to land on the same recycled port, surfacing as a spurious
	// "connection reset by peer". A fresh, non-pooling client dials anew every
	// call, so no stale connection can be reused.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestLiveMCP_PlainOnly_Presence: plain.listen set, oauth.listen absent → only the
// plain port binds; no OAuth discovery is served on it.
func TestLiveMCP_PlainOnly_Presence(t *testing.T) {
	baseDir := t.TempDir()
	var httpPort, plainPort int
	var logPath string
	cleanup := startLiveServer(t, func() liveLaunch {
		httpPort = freePort(t)
		plainPort = freePort(t)
		cfgPath := writeTwoTransportConfig(t, baseDir, httpPort, plainPort, false, 0, nil)
		logPath = filepath.Join(t.TempDir(), "server.log")
		return liveLaunch{cfgPath: cfgPath, logPath: logPath, readyPorts: []int{httpPort, plainPort}}
	})
	defer cleanup()

	assert.True(t, portBound(plainPort, 2*time.Second), "plain port must bind")
	// The OAuth transport is unconfigured: assert the server never STARTED an
	// oauth listener (its authoritative startup log), not that some unowned
	// ephemeral port stays free — the latter is the residual race (see
	// serverStartedListener). The plain listener IS started, proving the signal.
	assert.True(t, serverStartedListener(t, logPath, "MCP-plain"), "plain listener must be started")
	assert.False(t, serverStartedListener(t, logPath, "MCP-oauth"), "unconfigured oauth listener must NOT be started")

	// The plain port serves no OAuth discovery document.
	assert.NotEqual(t, http.StatusOK, getStatus(t, plainPort, serverurl.ProtectedResourceMetadataPath(), ""),
		"plain port must not serve OAuth discovery")

	// A tool round-trip succeeds over the plain (unauthenticated) port.
	runLifecycle(t, mcpEndpoint(plainPort), nil, "plain-only")
}

// TestLiveMCP_OAuthOnly_Presence: oauth.listen set, plain.listen absent → only the
// OAuth port binds; discovery + AS endpoints are reachable on it unauthenticated.
func TestLiveMCP_OAuthOnly_Presence(t *testing.T) {
	baseDir := t.TempDir()
	var httpPort, oauthPort int
	var logPath string
	cleanup := startLiveServer(t, func() liveLaunch {
		httpPort = freePort(t)
		oauthPort = freePort(t)
		cfgPath := writeTwoTransportConfig(t, baseDir, httpPort, 0, false, oauthPort, nil)
		logPath = filepath.Join(t.TempDir(), "server.log")
		return liveLaunch{cfgPath: cfgPath, logPath: logPath, readyPorts: []int{httpPort, oauthPort}}
	})
	defer cleanup()

	assert.True(t, portBound(oauthPort, 2*time.Second), "oauth port must bind")
	// The plain transport is unconfigured: assert via the server's startup log
	// that no plain listener was started (race-free), not that an unowned port
	// stays free (see serverStartedListener). The oauth listener IS started.
	assert.True(t, serverStartedListener(t, logPath, "MCP-oauth"), "oauth listener must be started")
	assert.False(t, serverStartedListener(t, logPath, "MCP-plain"), "unconfigured plain listener must NOT be started")

	// Discovery (RFC 9728) and AS metadata (RFC 8414) are reachable without a token.
	assert.Equal(t, http.StatusOK, getStatus(t, oauthPort, serverurl.ProtectedResourceMetadataPath(), ""),
		"PRM discovery must be reachable unauthenticated on the oauth port")
	assert.Equal(t, http.StatusOK, getStatus(t, oauthPort, serverurl.AuthorizationServerMetadataPath(), ""),
		"AS metadata must be reachable unauthenticated on the oauth port")
}

// TestLiveMCP_BothPorts_Presence: both listen addresses set → both ports bind.
func TestLiveMCP_BothPorts_Presence(t *testing.T) {
	baseDir := t.TempDir()
	var httpPort, plainPort, oauthPort int
	cleanup := startLiveServer(t, func() liveLaunch {
		httpPort = freePort(t)
		plainPort = freePort(t)
		oauthPort = freePort(t)
		cfgPath := writeTwoTransportConfig(t, baseDir, httpPort, plainPort, false, oauthPort, nil)
		logPath := filepath.Join(t.TempDir(), "server.log")
		return liveLaunch{cfgPath: cfgPath, logPath: logPath, readyPorts: []int{httpPort, plainPort, oauthPort}}
	})
	defer cleanup()

	assert.True(t, portBound(plainPort, 2*time.Second), "plain port must bind")
	assert.True(t, portBound(oauthPort, 2*time.Second), "oauth port must bind")

	// Discovery on the oauth port; none on the plain port.
	assert.Equal(t, http.StatusOK, getStatus(t, oauthPort, serverurl.ProtectedResourceMetadataPath(), ""),
		"oauth port serves discovery")
	assert.NotEqual(t, http.StatusOK, getStatus(t, plainPort, serverurl.ProtectedResourceMetadataPath(), ""),
		"plain port does not serve discovery")
}

// TestLiveMCP_PlainPort_BearerAuth: with bearer_auth:true the plain port rejects an
// absent/wrong bearer and accepts the configured static token.
func TestLiveMCP_PlainPort_BearerAuth(t *testing.T) {
	const token = "plain-static-token"
	baseDir := t.TempDir()
	var httpPort, plainPort int
	cleanup := startLiveServer(t, func() liveLaunch {
		httpPort = freePort(t)
		plainPort = freePort(t)
		cfgPath := writeTwoTransportConfig(t, baseDir, httpPort, plainPort, true, 0, []string{token})
		logPath := filepath.Join(t.TempDir(), "server.log")
		return liveLaunch{cfgPath: cfgPath, logPath: logPath, readyPorts: []int{httpPort, plainPort}}
	})
	defer cleanup()

	// Absent and wrong bearers are rejected with 401.
	assert.Equal(t, http.StatusUnauthorized, getStatus(t, plainPort, mcpPath, ""),
		"plain port with bearer_auth must reject an absent token")
	assert.Equal(t, http.StatusUnauthorized, getStatus(t, plainPort, mcpPath, "wrong-token"),
		"plain port with bearer_auth must reject a wrong token")

	// The correct token lets the full MCP round-trip through.
	httpClient := &http.Client{Transport: bearerRT{base: http.DefaultTransport, token: token}}
	runLifecycle(t, mcpEndpoint(plainPort), httpClient, "plain-bearer")
}

// TestLiveMCP_PlainPort_NoAuth: with bearer_auth:false the plain port accepts a
// request that carries no credential.
func TestLiveMCP_PlainPort_NoAuth(t *testing.T) {
	baseDir := t.TempDir()
	var httpPort, plainPort int
	cleanup := startLiveServer(t, func() liveLaunch {
		httpPort = freePort(t)
		plainPort = freePort(t)
		cfgPath := writeTwoTransportConfig(t, baseDir, httpPort, plainPort, false, 0, nil)
		logPath := filepath.Join(t.TempDir(), "server.log")
		return liveLaunch{cfgPath: cfgPath, logPath: logPath, readyPorts: []int{httpPort, plainPort}}
	})
	defer cleanup()

	// A bare GET reaches the MCP handler (not a 401) — auth let it through. The
	// handler itself answers 4xx for a non-stream GET; the point is "not 401".
	assert.NotEqual(t, http.StatusUnauthorized, getStatus(t, plainPort, mcpPath, ""),
		"plain port without bearer_auth must not require a token")
	runLifecycle(t, mcpEndpoint(plainPort), nil, "plain-noauth")
}

// TestLiveMCP_OAuthPort_Auth: the OAuth port enforces OAuth tokens — an absent
// token and a static bearer (server.auth.tokens) are both rejected, discovery is
// reachable unauthenticated, and a valid seeded OAuth access token passes.
func TestLiveMCP_OAuthPort_Auth(t *testing.T) {
	const staticToken = "a-static-bearer-not-oauth"
	baseDir := t.TempDir()

	// Seed a valid OAuth token into the store the server will open. baseDir is
	// stable across launch retries (only the ports are re-picked), so the seeded
	// token remains valid for whichever attempt finally binds.
	accessToken := seedOAuthToken(t, baseDir)

	var httpPort, oauthPort int
	cleanup := startLiveServer(t, func() liveLaunch {
		httpPort = freePort(t)
		oauthPort = freePort(t)
		// server.auth.tokens carries a static bearer that the OAuth port must NOT honor.
		cfgPath := writeTwoTransportConfig(t, baseDir, httpPort, 0, false, oauthPort, []string{staticToken})
		logPath := filepath.Join(t.TempDir(), "server.log")
		return liveLaunch{cfgPath: cfgPath, logPath: logPath, readyPorts: []int{httpPort, oauthPort}}
	})
	defer cleanup()

	// Discovery reachable unauthenticated.
	assert.Equal(t, http.StatusOK, getStatus(t, oauthPort, serverurl.ProtectedResourceMetadataPath(), ""),
		"discovery reachable unauthenticated on oauth port")

	// No token → 401.
	assert.Equal(t, http.StatusUnauthorized, getStatus(t, oauthPort, mcpPath, ""),
		"oauth port rejects a request with no token")

	// A static server.auth.tokens bearer is NOT an OAuth token → 401 (port purity).
	assert.Equal(t, http.StatusUnauthorized, getStatus(t, oauthPort, mcpPath, staticToken),
		"oauth port must reject a static server.auth.tokens bearer")

	// A valid OAuth access token passes the full MCP round-trip.
	httpClient := &http.Client{Transport: bearerRT{base: http.DefaultTransport, token: accessToken}}
	runLifecycle(t, mcpEndpoint(oauthPort), httpClient, "oauth-valid")
}
