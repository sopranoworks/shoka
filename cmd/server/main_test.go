package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/shoka/mcp-server/internal/auth"
	"github.com/shoka/mcp-server/internal/oauth"
	"github.com/shoka/mcp-server/internal/storage/oauthstore"
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

// The OAuth listener handler (B-50 phase 2): the discovery documents are reachable
// without a token while the MCP catch-all stays auth-enforced.
func TestOAuthListenerHandlerDiscoveryReachable(t *testing.T) {
	a := auth.New(auth.Config{Enabled: true, Tokens: []string{"secret"}})
	h := oauthListenerHandler(oauth.DiscoveryConfig{ExternalURL: "https://public.example"}, nil, okHandler(), a)

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

// With an AuthServer wired, /authorize and /token are mounted ahead of the
// auth-enforced catch-all: they are reachable without a bearer (they are how a
// token is obtained), so they never return the 401 the catch-all would.
func TestOAuthListenerHandlerMountsAuthServer(t *testing.T) {
	a := auth.New(auth.Config{Enabled: true, Tokens: []string{"secret"}})
	store, err := oauthstore.Open(filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	as := oauth.NewAuthServer(store, oauth.NewVerifier(nil), oauth.AuthServerConfig{ExternalURL: "https://public.example"})
	h := oauthListenerHandler(oauth.DiscoveryConfig{ExternalURL: "https://public.example"}, as, okHandler(), a)

	// GET /authorize with no params: the endpoint runs (on-page client error),
	// it does NOT fall through to the 401 catch-all.
	if code := mcpProbe(t, h, "/authorize"); code == http.StatusUnauthorized {
		t.Fatalf("/authorize must be mounted (reachable without a token), got 401")
	}
	// GET /token: method not allowed (it is POST-only) — again, not the 401.
	if code := mcpProbe(t, h, "/token"); code == http.StatusUnauthorized {
		t.Fatalf("/token must be mounted (reachable without a token), got 401")
	}
}

// The plain port (B-50 phase 2) wraps the bare MCP handler with its own
// static-bearer-or-none authenticator and mounts NO discovery/AS surface. With
// bearer_auth on (Enabled+Tokens) it enforces the static token and the well-known
// paths fall through to the auth gate; with bearer_auth off (the zero Config) it
// is unauthenticated.
func TestPlainPortHandlerAuthAndNoDiscovery(t *testing.T) {
	// bearer_auth: true — static-bearer enforced, no discovery served.
	withAuth := auth.New(auth.Config{Enabled: true, Tokens: []string{"secret"}}).Middleware(okHandler())
	if code := mcpProbe(t, withAuth, "/.well-known/oauth-protected-resource/mcp"); code != http.StatusUnauthorized {
		t.Fatalf("plain port must not mount discovery (well-known hits the auth gate), got %d", code)
	}
	if code := mcpProbe(t, withAuth, "/mcp"); code != http.StatusUnauthorized {
		t.Fatalf("plain port with bearer_auth must require a token, got %d", code)
	}

	// bearer_auth: false — unauthenticated; every path reaches the MCP handler.
	noAuth := auth.New(auth.Config{}).Middleware(okHandler())
	if code := mcpProbe(t, noAuth, "/mcp"); code != http.StatusOK {
		t.Fatalf("plain port without bearer_auth must allow the request through, got %d", code)
	}
}
