package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sopranoworks/shoka/internal/auth"
	"github.com/sopranoworks/shoka/internal/config"
	"github.com/sopranoworks/shoka/internal/oauth"
	"github.com/sopranoworks/shoka/internal/storage/oauthstore"
)

func mcpProbe(t *testing.T, h http.Handler, path string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec.Code
}

// probeBearer probes h with an Authorization: Bearer <token> header.
func probeBearer(t *testing.T, h http.Handler, path, token string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(rec, req)
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

// B-50 phase 3: the Web/non-MCP routes (/drafts/, /ws/ui, /api/) authenticate
// against server.auth's static-bearer tokens + WS origin policy ONLY — never the
// OAuth ValidateToken closure (an OAuth access token is an MCP-client credential
// and must not gate a browser route). webAuthConfig must therefore strip all OAuth
// wiring regardless of what the rest of the config carries: no ValidateToken, no
// ResourceMetadataURL. Behaviourally that means the configured static bearer IS
// accepted on the Web path (it would be REJECTED if the OAuth closure had leaked
// through, since ValidateToken supersedes static-bearer), while a non-configured
// token (e.g. an OAuth access token) is not.
func TestWebAuthConfigHasNoOAuthClosure(t *testing.T) {
	wc := webAuthConfig(config.AuthConfig{
		Enabled:        true,
		Tokens:         []string{"web-secret"},
		AllowedOrigins: []string{"https://ui.example"},
	})
	if wc.ValidateToken != nil {
		t.Fatal("web auth must not carry the OAuth ValidateToken closure")
	}
	if wc.ResourceMetadataURL != nil {
		t.Fatal("web auth must not carry the OAuth ResourceMetadataURL composer")
	}

	gate := auth.New(wc).Middleware(okHandler())
	if code := probeBearer(t, gate, "/api/recover", "web-secret"); code != http.StatusOK {
		t.Fatalf("the configured static bearer must be accepted on the Web /api path, got %d", code)
	}
	if code := probeBearer(t, gate, "/api/recover", "an-oauth-access-token"); code != http.StatusUnauthorized {
		t.Fatalf("a non-server.auth token (e.g. an OAuth access token) must NOT authenticate the Web /api path, got %d", code)
	}
	if code := mcpProbe(t, gate, "/api/recover"); code != http.StatusUnauthorized {
		t.Fatalf("no token with server.auth enabled must be 401 on the Web /api path, got %d", code)
	}
}

// With server.auth disabled (the default single-operator local mode) the Web /api
// route is open — the pinned non-OAuth gate when server.auth is unset. This proves
// a browser with NO token can reach /api/ (the recovery dialog), the intended
// latent-bug fix: at HEAD, /api/ was OAuth-wrapped and locked the browser out
// whenever the OAuth transport was configured.
func TestWebAuthConfigDisabledIsOpen(t *testing.T) {
	gate := auth.New(webAuthConfig(config.AuthConfig{Enabled: false})).Middleware(okHandler())
	if code := mcpProbe(t, gate, "/api/recover"); code != http.StatusOK {
		t.Fatalf("with server.auth disabled the Web /api path must be open, got %d", code)
	}
}

// /drafts/ and /ws/ui already used the static-bearer/query-token path at HEAD;
// after decoupling they run on the same server.auth policy via the non-OAuth
// webAuth. Their gate is unchanged: the ?token= query fallback (browsers cannot
// set an Authorization header on a WS handshake) and the WS origin policy are
// carried through exactly as before — only the authenticator instance changed.
func TestWebAuthConfigCarriesWSPolicyUnchanged(t *testing.T) {
	a := auth.New(webAuthConfig(config.AuthConfig{
		Enabled:        true,
		Tokens:         []string{"web-secret"},
		AllowedOrigins: []string{"https://ui.example"},
	}))

	wsGate := a.MiddlewareAllowQueryToken(okHandler())
	if code := mcpProbe(t, wsGate, "/ws/ui?token=web-secret"); code != http.StatusOK {
		t.Fatalf("?token= must authenticate the WS routes, got %d", code)
	}
	if code := mcpProbe(t, wsGate, "/ws/ui?token=wrong"); code != http.StatusUnauthorized {
		t.Fatalf("a wrong ?token= must be rejected on the WS routes, got %d", code)
	}

	allow := httptest.NewRequest(http.MethodGet, "/ws/ui", nil)
	allow.Header.Set("Origin", "https://ui.example")
	if !a.OriginAllowed(allow) {
		t.Fatal("the configured allowed origin must pass")
	}
	deny := httptest.NewRequest(http.MethodGet, "/ws/ui", nil)
	deny.Header.Set("Origin", "https://evil.example")
	if a.OriginAllowed(deny) {
		t.Fatal("an unlisted origin must be rejected")
	}
}

// startupPostures by case (B-50 phase 4): each opened surface is summarised by
// presence + auth posture as a category — plain unauthenticated vs static-bearer
// per bearer_auth, oauth always OAuth-protected, web always OAuth-free (open vs
// static-bearer per server.auth.enabled). The MCP ports appear only when their
// listen is set; the Web UI always appears.
func TestDescribeStartupPostures(t *testing.T) {
	posture := func(ps []startupPosture, surface string) (startupPosture, bool) {
		for _, p := range ps {
			if p.Surface == surface {
				return p, true
			}
		}
		return startupPosture{}, false
	}

	t.Run("plain-only unauthenticated", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Server.MCP.Plain.Listen = "plain-listen-PLACEHOLDER"
		cfg.Server.MCP.Plain.BearerAuth = false
		ps := describeStartupPostures(cfg)
		if _, ok := posture(ps, "mcp-oauth"); ok {
			t.Fatal("mcp-oauth must NOT appear when oauth.listen is unset")
		}
		p, ok := posture(ps, "mcp-plain")
		if !ok || p.Auth != "unauthenticated" {
			t.Fatalf("plain posture = %+v, ok=%v; want auth=unauthenticated", p, ok)
		}
		w, ok := posture(ps, "web")
		if !ok || w.Auth != "oauth-free" || w.Policy != "open" {
			t.Fatalf("web posture = %+v; want oauth-free/open", w)
		}
	})

	t.Run("plain-only static-bearer", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Server.MCP.Plain.Listen = "plain-listen-PLACEHOLDER"
		cfg.Server.MCP.Plain.BearerAuth = true
		p, ok := posture(describeStartupPostures(cfg), "mcp-plain")
		if !ok || p.Auth != "static-bearer" {
			t.Fatalf("plain posture = %+v, ok=%v; want auth=static-bearer", p, ok)
		}
	})

	t.Run("oauth-only", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Server.MCP.OAuth.Listen = "oauth-listen-PLACEHOLDER"
		ps := describeStartupPostures(cfg)
		if _, ok := posture(ps, "mcp-plain"); ok {
			t.Fatal("mcp-plain must NOT appear when plain.listen is unset")
		}
		p, ok := posture(ps, "mcp-oauth")
		if !ok || p.Auth != "oauth-protected" {
			t.Fatalf("oauth posture = %+v, ok=%v; want auth=oauth-protected", p, ok)
		}
		// B-63 §0.1: the posture names the advertised registration mode (default cimd).
		if p.Policy != "cimd-registration" {
			t.Fatalf("oauth posture policy = %q; want cimd-registration (default)", p.Policy)
		}
	})

	t.Run("oauth dcr registration mode surfaced", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Server.MCP.OAuth.Listen = "oauth-listen-PLACEHOLDER"
		cfg.Server.MCP.OAuth.RegistrationMode = "dcr"
		p, ok := posture(describeStartupPostures(cfg), "mcp-oauth")
		if !ok || p.Policy != "dcr-registration" {
			t.Fatalf("oauth posture = %+v; want policy=dcr-registration", p)
		}
	})

	t.Run("both ports plus web static-bearer", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Server.MCP.Plain.Listen = "plain-listen-PLACEHOLDER"
		cfg.Server.MCP.Plain.BearerAuth = true
		cfg.Server.MCP.OAuth.Listen = "oauth-listen-PLACEHOLDER"
		cfg.Server.Auth.Enabled = true
		ps := describeStartupPostures(cfg)
		if _, ok := posture(ps, "mcp-plain"); !ok {
			t.Fatal("mcp-plain must appear")
		}
		if _, ok := posture(ps, "mcp-oauth"); !ok {
			t.Fatal("mcp-oauth must appear")
		}
		w, ok := posture(ps, "web")
		if !ok || w.Auth != "oauth-free" || w.Policy != "static-bearer" {
			t.Fatalf("web posture = %+v; want oauth-free/static-bearer", w)
		}
	})
}

// The phase-4 KEY CHECK: no startup posture field may contain a listen address,
// external_url, token, or trusted-domain value. We seed the config with sentinel
// secrets/addresses and assert NONE of them appears in any posture field.
func TestDescribeStartupPostures_NoAddressOrSecret(t *testing.T) {
	const (
		plainAddr   = "10.1.2.3:7777"
		oauthAddr   = "0.0.0.0:8443"
		extURL      = "https://secret.example.test/mcp"
		token       = "super-secret-static-token"
		trustedHost = "trusted.client.example.test"
	)
	cfg := &config.Config{}
	cfg.Server.MCP.Plain.Listen = plainAddr
	cfg.Server.MCP.Plain.BearerAuth = true
	cfg.Server.MCP.Plain.ExternalURL = extURL
	cfg.Server.MCP.OAuth.Listen = oauthAddr
	cfg.Server.MCP.OAuth.ExternalURL = extURL
	cfg.Server.Auth.Enabled = true
	cfg.Server.Auth.Tokens = []string{token}
	cfg.Server.Auth.AllowedOrigins = []string{"https://" + trustedHost}

	forbidden := []string{plainAddr, oauthAddr, extURL, token, trustedHost, "10.1.2.3", "8443", "7777"}
	for _, p := range describeStartupPostures(cfg) {
		for _, field := range []string{p.Surface, p.Auth, p.Policy} {
			for _, bad := range forbidden {
				if field != "" && strings.Contains(field, bad) {
					t.Fatalf("posture field %q leaks confidential value %q (surface=%s)", field, bad, p.Surface)
				}
			}
		}
	}
}
