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

// Authenticate reports whether r carries a valid credential. When auth is
// disabled it always returns true.
func (a *Authenticator) Authenticate(r *http.Request) bool {
	if !a.enabled {
		return true
	}
	return a.validToken(tokenFromRequest(r))
}

// Middleware wraps next, returning 401 when auth is enabled and the request
// carries no valid token. When disabled it passes every request through.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.Authenticate(r) {
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

// tokenFromRequest extracts the bearer token from the Authorization header, or
// falls back to the `token` query parameter (browsers cannot set an
// Authorization header on a WebSocket handshake).
func tokenFromRequest(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) >= 7 && strings.EqualFold(h[:7], "Bearer ") {
		return h[7:]
	}
	return r.URL.Query().Get("token")
}
