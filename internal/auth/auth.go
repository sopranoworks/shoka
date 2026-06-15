// Package auth provides optional Bearer-token authentication and WebSocket
// origin policy for Shoka's network endpoints. When disabled (the default), all
// requests are allowed and all origins are accepted, preserving single-operator
// local behaviour.
package auth

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sopranoworks/shoka/internal/reqtrace"
	"github.com/sopranoworks/shoka/internal/tokenfp"
)

// mcpSessionIDHeader is the Streamable HTTP session identifier header (MCP spec).
// It is absent on the initialize handshake (the server assigns it on the response)
// and echoed on every subsequent request — so an empty value on a POST marks the
// connect's first request, used to log the principal binding once per connect.
const mcpSessionIDHeader = "Mcp-Session-Id"

// Principal is the authenticated OAuth principal bound to a validated access
// token. It is attached to the MCP request context on a successful OAuth-enforced
// request so the write path can record it as the commit Committer (the B-39 §2.5
// decoupling). Name/Email come from the token's binding, not a config constant.
// ClientID is the CIMD client_id (metadata URL) the token was issued to — a
// non-secret identifier carried for diagnostic logging only (B-52); it does NOT
// participate in the commit identity.
type Principal struct {
	Name     string
	Email    string
	ClientID string
	// Scope is the authorization grant the token carries (the 2026-06-15 authz
	// foundation). "*" — or empty, which is read AS "*" everywhere — means
	// all-access: every DCR-issued token today. A future non-DCR pre-issued token
	// carries a namespace grant (e.g. "namespace:foo"); the tools/call authz gate
	// (internal/tools.AuthzMiddleware) enforces it. Empty is treated as "*" for
	// backward compatibility with tokens minted before the field existed.
	Scope string
}

// RejectReason is a discrete OAuth token-rejection category (B-53 §2.4), returned
// by ValidateToken so the auth stage can name WHY a 401 was issued instead of the
// prior reasonless rejection. The reason already exists at the token store
// (oauthstore.Lookup distinguishes not-found from expired) — widening the validator
// to carry it is observability-only: the allow/deny decision and every wire
// response are unchanged. Empty ("") on success.
type RejectReason string

const (
	// ReasonMissingBearer: no bearer token was presented at all.
	ReasonMissingBearer RejectReason = "missing-bearer"
	// ReasonInvalidToken: a token was presented but is unknown/revoked/rotated away.
	ReasonInvalidToken RejectReason = "invalid-token"
	// ReasonExpired: the token exists but is past its expiry.
	ReasonExpired RejectReason = "expired"
	// ReasonPrincipalUnresolved: the token validated but no principal could be
	// resolved for it (defensive — a malformed/empty binding).
	ReasonPrincipalUnresolved RejectReason = "principal-unresolved"
)

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
	//
	// The RejectReason return (B-53 §2.4) is logging-only: it lets the auth stage
	// name a discrete rejection category. It is ignored for the allow/deny decision
	// (the bool alone decides), so the wire behaviour is identical to the prior
	// (Principal, bool) signature.
	ValidateToken func(token string) (Principal, RejectReason, bool)
	// Logger records the auth stage (B-53 §2.4): every request that reaches an
	// authenticator emits its result — authenticated (authenticator + principal/
	// client_id) at INFO, or rejected with a discrete reason at WARN — all carrying
	// the shared request_id. Nil → slog.Default(). The access token is never logged.
	Logger *slog.Logger
}

// Authenticator enforces token authentication and origin restrictions.
type Authenticator struct {
	enabled             bool
	tokens              []string
	allowedOrigins      []string
	resourceMetadataURL func(*http.Request) string
	validateToken       func(token string) (Principal, RejectReason, bool)
	logger              *slog.Logger
}

// New builds an Authenticator from c.
func New(c Config) *Authenticator {
	logger := c.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Authenticator{
		enabled:             c.Enabled,
		tokens:              c.Tokens,
		allowedOrigins:      c.AllowedOrigins,
		resourceMetadataURL: c.ResourceMetadataURL,
		validateToken:       c.ValidateToken,
		logger:              logger,
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
		return a.middleware(next, a.Authenticate, bearerToken)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bearer := bearerToken(r)
		p, reason, ok := a.validateToken(bearer)
		if !ok {
			// Close the reasonless-401 (B-53 §2.4): the OAuth auth stage names the
			// discrete category and the shared request_id so the rejection correlates
			// with this request's entry/response lines. The access token is never
			// logged; a one-way fingerprint (B-54) correlates the PRESENTED token with
			// the "oauth token issued" line — proving whether the same value reached
			// Lookup. The wire response is unchanged (401 + challenge).
			a.logRejected(r, "oauth", reason, tokenfp.Fingerprint(bearer))
			w.Header().Set("WWW-Authenticate", a.challenge(r))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		// Auth stage, success (B-53 §2.4 — always, not gated): which client/principal
		// the token bound to, under the shared request_id. is_handshake marks the
		// initialize POST (no Mcp-Session-Id yet) — answering B-52's "which client got
		// bound to the new session" without a separate gated line. token_fingerprint
		// (B-54) is the one-way fingerprint of the accepted token, so a SUCCESSFUL
		// validation also correlates to its issuance. The token is NEVER logged.
		a.logger.LogAttrs(r.Context(), slog.LevelInfo, "auth ok",
			slog.String("request_id", reqtrace.ID(r.Context())),
			slog.String("authenticator", "oauth"),
			slog.String("client_id", p.ClientID),
			slog.String("principal", p.Name),
			slog.String("token_fingerprint", tokenfp.Fingerprint(bearer)),
			slog.Bool("is_handshake", r.Method == http.MethodPost && r.Header.Get(mcpSessionIDHeader) == ""),
			slog.String("remote", r.RemoteAddr))
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// MiddlewareAllowQueryToken wraps next with authentication that also accepts the
// ?token= query parameter, for the WebSocket endpoints (/ws/ui and /drafts/).
func (a *Authenticator) MiddlewareAllowQueryToken(next http.Handler) http.Handler {
	return a.middleware(next, a.AuthenticateWebSocket, tokenFromRequest)
}

// middleware is the static-bearer / disabled auth path (plain MCP and all Web
// routes). tokenOf extracts the credential the way authed consults it (header-only
// vs header-or-?token=), so a rejection can be classified as missing vs invalid.
func (a *Authenticator) middleware(next http.Handler, authed func(*http.Request) bool, tokenOf func(*http.Request) string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Disabled authenticator: allow-all (single-operator/loopback). It makes no
		// auth decision, so the auth stage is "disabled" — the request is still fully
		// traced by reqtrace's entry/response lines. Logged at INFO with the id.
		if !a.enabled {
			a.logger.LogAttrs(r.Context(), slog.LevelInfo, "auth ok",
				slog.String("request_id", reqtrace.ID(r.Context())),
				slog.String("authenticator", "disabled"),
				slog.String("remote", r.RemoteAddr))
			next.ServeHTTP(w, r)
			return
		}
		if !authed(r) {
			tok := tokenOf(r)
			reason := ReasonInvalidToken
			if tok == "" {
				reason = ReasonMissingBearer
			}
			a.logRejected(r, "static-bearer", reason, tokenfp.Fingerprint(tok))
			w.Header().Set("WWW-Authenticate", a.challenge(r))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		a.logger.LogAttrs(r.Context(), slog.LevelInfo, "auth ok",
			slog.String("request_id", reqtrace.ID(r.Context())),
			slog.String("authenticator", "static-bearer"),
			slog.String("remote", r.RemoteAddr))
		next.ServeHTTP(w, r)
	})
}

// logRejected emits the auth-stage rejection line (B-53 §2.4) at WARN: the
// authenticator kind, the discrete reason category, the shared request_id, and the
// one-way fingerprint of the PRESENTED token (B-54 — "" when none) — so no 401 is
// ever silent and the rejected token correlates to its issuance line. No credential
// value is logged.
func (a *Authenticator) logRejected(r *http.Request, authenticator string, reason RejectReason, fingerprint string) {
	a.logger.LogAttrs(r.Context(), slog.LevelWarn, "auth rejected",
		slog.String("request_id", reqtrace.ID(r.Context())),
		slog.String("authenticator", authenticator),
		slog.String("reason", string(reason)),
		slog.String("token_fingerprint", fingerprint),
		slog.String("remote", r.RemoteAddr))
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
