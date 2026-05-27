// Package auth provides optional Bearer-token authentication and WebSocket
// origin policy for Shoka's network endpoints. When disabled (the default), all
// requests are allowed and all origins are accepted, preserving single-operator
// local behaviour.
package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// Config configures the Authenticator. The zero value is a disabled
// authenticator that allows everything.
type Config struct {
	Enabled        bool
	Tokens         []string
	AllowedOrigins []string
}

// Authenticator enforces token authentication and origin restrictions.
type Authenticator struct {
	enabled        bool
	tokens         []string
	allowedOrigins []string
}

// New builds an Authenticator from c.
func New(c Config) *Authenticator {
	return &Authenticator{
		enabled:        c.Enabled,
		tokens:         c.Tokens,
		allowedOrigins: c.AllowedOrigins,
	}
}

// Authenticate reports whether r carries a valid credential via the
// Authorization: Bearer header. When auth is disabled it always returns true.
// The ?token= query fallback is intentionally NOT consulted here — it is scoped
// to the WebSocket endpoints (AuthenticateWebSocket / MiddlewareAllowQueryToken)
// so that bearer tokens never need to appear in URLs on the MCP/SSE endpoint.
func (a *Authenticator) Authenticate(r *http.Request) bool {
	if !a.enabled {
		return true
	}
	return a.validToken(bearerToken(r))
}

// AuthenticateWebSocket is like Authenticate but additionally accepts the token
// via the ?token= query parameter, because browsers cannot set an Authorization
// header on a WebSocket handshake.
func (a *Authenticator) AuthenticateWebSocket(r *http.Request) bool {
	if !a.enabled {
		return true
	}
	return a.validToken(tokenFromRequest(r))
}

// Middleware wraps next with header-only Bearer authentication (used for the
// MCP/SSE endpoint). Returns 401 when auth is enabled and no valid Bearer token
// is present. When disabled it passes every request through.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return a.middleware(next, a.Authenticate)
}

// MiddlewareAllowQueryToken wraps next with authentication that also accepts the
// ?token= query parameter, for the WebSocket endpoints (/ws/ui and /drafts/).
func (a *Authenticator) MiddlewareAllowQueryToken(next http.Handler) http.Handler {
	return a.middleware(next, a.AuthenticateWebSocket)
}

func (a *Authenticator) middleware(next http.Handler, authed func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// OriginAllowed implements the WebSocket CheckOrigin policy. When auth is
// disabled it allows any origin; when enabled it allows only origins listed in
// AllowedOrigins (an empty Origin header is rejected).
func (a *Authenticator) OriginAllowed(r *http.Request) bool {
	if !a.enabled {
		return true
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	for _, o := range a.allowedOrigins {
		if o == origin {
			return true
		}
	}
	return false
}

// validToken compares provided against every configured token in constant time.
// All tokens are checked (no early return) to avoid leaking which token matched.
func (a *Authenticator) validToken(provided string) bool {
	pb := []byte(provided)
	var matched int
	for _, t := range a.tokens {
		if subtle.ConstantTimeCompare(pb, []byte(t)) == 1 {
			matched = 1
		}
	}
	return matched == 1
}

// bearerToken extracts the token from an "Authorization: Bearer <token>" header,
// or returns "" if absent.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
		return h[7:]
	}
	return ""
}

// tokenFromRequest returns the Bearer header token, or falls back to the `token`
// query parameter. The query fallback is used only on WebSocket paths, where
// browsers cannot set an Authorization header on the handshake.
func tokenFromRequest(r *http.Request) string {
	if t := bearerToken(r); t != "" {
		return t
	}
	return r.URL.Query().Get("token")
}
