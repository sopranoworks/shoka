// Package auth provides optional Bearer-token authentication and WebSocket
// origin policy for Shoka's network endpoints. When disabled (the default), all
// requests are allowed and all origins are accepted, preserving single-operator
// local behaviour.
package auth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
)

// Principal is the authenticated OAuth principal bound to a validated access
// token. It is attached to the MCP request context on a successful OAuth-enforced
// request so the write path can record it as the commit Committer (the B-39 §2.5
// decoupling). Name/Email come from the token's binding, not a config constant.
type Principal struct {
	Name  string
	Email string
}

type principalCtxKey struct{}

// WithPrincipal attaches the authenticated principal to ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalCtxKey{}, p)
}

// PrincipalFrom returns the authenticated principal carried on ctx, if any. The
// MCP tool handlers read it to set the commit Committer via identity.WithCommitter.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalCtxKey{}).(Principal)
	return p, ok
}

// Config configures the Authenticator. The zero value is a disabled
// authenticator that allows everything.
type Config struct {
	Enabled        bool
	Tokens         []string
	AllowedOrigins []string
	// ResourceMetadataURL, when set, returns the absolute URL of the OAuth
	// Protected Resource Metadata document for a request (RFC 9728). When it
	// returns a non-empty value the 401 WWW-Authenticate challenge carries the
	// resource_metadata parameter so a client can discover the authorization
	// server. Injected by the server when OAuth discovery is enabled; this keeps
	// the auth package free of any URL-composition dependency.
	ResourceMetadataURL func(*http.Request) string
	// ValidateToken, when set, enables OAuth token ENFORCEMENT on the MCP path
	// (the B-39 (b) directive). It validates a bearer access token against the
	// token store and returns the bound principal. When set it SUPERSEDES the
	// static-bearer check on the MCP middleware: only a valid OAuth access token
	// is accepted, and the bound principal is attached to the request context.
	// When nil, the MCP middleware behaves exactly as before (static-bearer or
	// disabled) — preserving the (a) discovery-only semantics.
	ValidateToken func(token string) (Principal, bool)
}

// Authenticator enforces token authentication and origin restrictions.
type Authenticator struct {
	enabled             bool
	tokens              []string
	allowedOrigins      []string
	resourceMetadataURL func(*http.Request) string
	validateToken       func(token string) (Principal, bool)
}

// New builds an Authenticator from c.
func New(c Config) *Authenticator {
	return &Authenticator{
		enabled:             c.Enabled,
		tokens:              c.Tokens,
		allowedOrigins:      c.AllowedOrigins,
		resourceMetadataURL: c.ResourceMetadataURL,
		validateToken:       c.ValidateToken,
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

// Middleware wraps next with authentication for the MCP endpoint. When an OAuth
// token validator is injected (enforcement on, B-39 (b)) it SUPERSEDES the
// static-bearer check: only a valid OAuth access token is accepted, a 401 with
// the resource_metadata challenge is returned otherwise, and the bound principal
// is attached to the request context so the write path can record it as the
// commit Committer. When no validator is injected the behaviour is exactly the
// prior static-bearer (or disabled) one.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	if a.validateToken == nil {
		return a.middleware(next, a.Authenticate)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.validateToken(bearerToken(r))
		if !ok {
			w.Header().Set("WWW-Authenticate", a.challenge(r))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// MiddlewareAllowQueryToken wraps next with authentication that also accepts the
// ?token= query parameter, for the WebSocket endpoints (/ws/ui and /drafts/).
func (a *Authenticator) MiddlewareAllowQueryToken(next http.Handler) http.Handler {
	return a.middleware(next, a.AuthenticateWebSocket)
}

func (a *Authenticator) middleware(next http.Handler, authed func(*http.Request) bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			w.Header().Set("WWW-Authenticate", a.challenge(r))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// challenge builds the WWW-Authenticate header value for a 401. It is the bare
// "Bearer" by default; when OAuth discovery is wired and a resource-metadata URL
// resolves for r, it carries the resource_metadata parameter (RFC 9728 §5.1) so a
// client can discover the authorization server.
func (a *Authenticator) challenge(r *http.Request) string {
	if a.resourceMetadataURL != nil {
		if u := a.resourceMetadataURL(r); u != "" {
			return `Bearer resource_metadata="` + u + `"`
		}
	}
	return "Bearer"
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
