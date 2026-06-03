package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func newReq(authHeader, query, origin string) *http.Request {
	target := "/"
	if query != "" {
		target = "/?" + query
	}
	r := httptest.NewRequest(http.MethodGet, target, nil)
	if authHeader != "" {
		r.Header.Set("Authorization", authHeader)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	return r
}

// serveMiddleware runs r through a.Middleware wrapping a handler that returns 200,
// and reports the resulting status code (200 = passed through, 401 = rejected).
func serveMiddleware(a *Authenticator, r *http.Request) int {
	rec := httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, r)
	return rec.Code
}

// serveMiddlewareWS is like serveMiddleware but uses the query-token-allowing
// middleware that wraps the WebSocket endpoints.
func serveMiddlewareWS(a *Authenticator, r *http.Request) int {
	rec := httptest.NewRecorder()
	a.MiddlewareAllowQueryToken(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, r)
	return rec.Code
}

func TestDisabledAllowsMissingToken(t *testing.T) {
	a := New(Config{Enabled: false})
	if !a.Authenticate(newReq("", "", "")) {
		t.Fatal("disabled authenticator should allow requests without a token")
	}
	if code := serveMiddleware(a, newReq("", "", "")); code != http.StatusOK {
		t.Fatalf("disabled middleware should pass through, got %d", code)
	}
}

func TestEnabledMissingTokenRejected(t *testing.T) {
	a := New(Config{Enabled: true, Tokens: []string{"secret"}})
	if code := serveMiddleware(a, newReq("", "", "")); code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing token, got %d", code)
	}
}

func TestEnabledUnknownTokenRejected(t *testing.T) {
	a := New(Config{Enabled: true, Tokens: []string{"secret"}})
	if code := serveMiddleware(a, newReq("Bearer wrong", "", "")); code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unknown token, got %d", code)
	}
}

func TestEnabledValidTokenHeaderAccepted(t *testing.T) {
	a := New(Config{Enabled: true, Tokens: []string{"secret", "second"}})
	if code := serveMiddleware(a, newReq("Bearer second", "", "")); code != http.StatusOK {
		t.Fatalf("expected 200 for valid token, got %d", code)
	}
}

// Browsers cannot set an Authorization header on a WebSocket handshake, so the
// token is accepted via a `token` query parameter on the WS middleware only.
func TestEnabledValidTokenQueryParamAcceptedOnWS(t *testing.T) {
	a := New(Config{Enabled: true, Tokens: []string{"secret"}})
	if code := serveMiddlewareWS(a, newReq("", "token=secret", "")); code != http.StatusOK {
		t.Fatalf("expected 200 for valid token via query param on WS middleware, got %d", code)
	}
}

// F1: the header-only Middleware (MCP/SSE) must NOT honor a query-param token.
func TestQueryParamRejectedByHeaderOnlyMiddleware(t *testing.T) {
	a := New(Config{Enabled: true, Tokens: []string{"secret"}})
	if code := serveMiddleware(a, newReq("", "token=secret", "")); code != http.StatusUnauthorized {
		t.Fatalf("expected 401: query token must not authenticate on header-only middleware, got %d", code)
	}
	// And the same request DOES authenticate on the WS middleware.
	if code := serveMiddlewareWS(a, newReq("", "token=secret", "")); code != http.StatusOK {
		t.Fatalf("expected 200 for query token on WS middleware, got %d", code)
	}
}

func TestTokenOfDifferentLengthRejected(t *testing.T) {
	a := New(Config{Enabled: true, Tokens: []string{"secret"}})
	if a.Authenticate(newReq("Bearer secretlonger", "", "")) {
		t.Fatal("longer token must be rejected")
	}
	if a.Authenticate(newReq("Bearer sec", "", "")) {
		t.Fatal("shorter token must be rejected")
	}
}

// serveChallenge runs r through a.Middleware against a 200 handler and returns the
// status code and the WWW-Authenticate header value.
func serveChallenge(a *Authenticator, r *http.Request) (int, string) {
	rec := httptest.NewRecorder()
	a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, r)
	return rec.Code, rec.Header().Get("WWW-Authenticate")
}

// When OAuth discovery is wired, the 401 challenge carries the resource_metadata
// parameter so a client can discover the authorization server (RFC 9728 §5.1).
func TestChallengeCarriesResourceMetadataWhenConfigured(t *testing.T) {
	const prm = "https://public.example/.well-known/oauth-protected-resource/mcp"
	a := New(Config{
		Enabled:             true,
		Tokens:              []string{"secret"},
		ResourceMetadataURL: func(*http.Request) string { return prm },
	})
	code, hdr := serveChallenge(a, newReq("", "", ""))
	if code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", code)
	}
	want := `Bearer resource_metadata="` + prm + `"`
	if hdr != want {
		t.Fatalf("WWW-Authenticate\n got %q\nwant %q", hdr, want)
	}
}

// Without OAuth discovery the challenge stays the bare bearer it has always been —
// byte-for-byte unchanged.
func TestChallengeBareBearerWhenNotConfigured(t *testing.T) {
	a := New(Config{Enabled: true, Tokens: []string{"secret"}})
	_, hdr := serveChallenge(a, newReq("", "", ""))
	if hdr != "Bearer" {
		t.Fatalf("expected bare Bearer, got %q", hdr)
	}
}

func TestOriginAllowedWhenDisabled(t *testing.T) {
	a := New(Config{Enabled: false})
	if !a.OriginAllowed(newReq("", "", "")) {
		t.Fatal("disabled authenticator should allow any origin")
	}
}

func TestOriginRestrictedWhenEnabled(t *testing.T) {
	a := New(Config{
		Enabled:        true,
		Tokens:         []string{"x"},
		AllowedOrigins: []string{"https://app.example.com"},
	})
	if !a.OriginAllowed(newReq("", "", "https://app.example.com")) {
		t.Fatal("listed origin should be allowed")
	}
	if a.OriginAllowed(newReq("", "", "https://evil.example.com")) {
		t.Fatal("unlisted origin must be rejected")
	}
	if a.OriginAllowed(newReq("", "", "")) {
		t.Fatal("empty origin must be rejected when auth enabled")
	}
}
