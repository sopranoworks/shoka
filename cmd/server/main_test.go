package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/shoka/mcp-server/internal/auth"
	"github.com/shoka/mcp-server/internal/oauth"
)

func mcpProbe(t *testing.T, h http.Handler, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
}

// With OAuth discovery enabled, the discovery documents are reachable without a
// token while the MCP catch-all stays auth-enforced.
func TestMCPListenerHandlerOAuthEnabled(t *testing.T) {
	a := auth.New(auth.Config{Enabled: true, Tokens: []string{"secret"}})
	h := mcpListenerHandler(true, oauth.DiscoveryConfig{ExternalURL: "https://public.example"}, okHandler(), a)

	for _, p := range []string{
		"/.well-known/oauth-protected-resource/mcp",
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-authorization-server",
	} {
		if code := mcpProbe(t, h, p); code != http.StatusOK {
			t.Fatalf("%s must be reachable without auth, got %d", p, code)
		}
	}
	if code := mcpProbe(t, h, "/mcp"); code != http.StatusUnauthorized {
		t.Fatalf("/mcp must require auth, got %d", code)
	}
}

// With OAuth discovery disabled, the handler is exactly the auth-wrapped MCP
// handler: no discovery is mounted (the well-known paths hit the auth catch-all),
// and the MCP path stays enforced — behaviour unchanged from before this directive.
func TestMCPListenerHandlerOAuthDisabledUnchanged(t *testing.T) {
	a := auth.New(auth.Config{Enabled: true, Tokens: []string{"secret"}})
	h := mcpListenerHandler(false, oauth.DiscoveryConfig{}, okHandler(), a)

	if code := mcpProbe(t, h, "/.well-known/oauth-protected-resource/mcp"); code != http.StatusUnauthorized {
		t.Fatalf("discovery must NOT be mounted when disabled, got %d", code)
	}
	if code := mcpProbe(t, h, "/mcp"); code != http.StatusUnauthorized {
		t.Fatalf("/mcp must require auth, got %d", code)
	}
}
