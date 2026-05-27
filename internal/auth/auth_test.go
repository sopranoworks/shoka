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
// token is also accepted via a `token` query parameter for WS endpoints.
func TestEnabledValidTokenQueryParamAccepted(t *testing.T) {
	a := New(Config{Enabled: true, Tokens: []string{"secret"}})
	if code := serveMiddleware(a, newReq("", "token=secret", "")); code != http.StatusOK {
		t.Fatalf("expected 200 for valid token via query param, got %d", code)
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
