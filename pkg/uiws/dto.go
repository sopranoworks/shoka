package uiws

import "time"

// OAuthConnectionInfo is one live OAuth/MCP connection in the OAUTH_LIST
// response — the no-secret view of an oauthstore.SeriesInfo. It carries the
// connecting client's identity (its CIMD metadata URL, the Claude side — shown
// to tell connections apart), the bound principal, the issued/expiry times, and
// the series id (the revoke target, plus a short prefix for display). It NEVER
// carries an access/refresh token, authorization code, or PKCE value — those
// live only in the store's SeriesRecord/CodeRecord and never reach List.
type OAuthConnectionInfo struct {
	// SeriesID is the full opaque series identifier — the OAUTH_REVOKE target. It
	// is NOT a bearer credential (it cannot authenticate anything; only access
	// tokens can), so exposing it to an admin client is safe.
	SeriesID      string `json:"series_id"`
	SeriesIDShort string `json:"series_id_short"`
	Name          string `json:"name,omitempty"`
	// ClientID is the connecting client's CIMD metadata URL (its identity). Shown
	// at runtime only; no concrete value is ever written into source/tests.
	ClientID       string    `json:"client_id"`
	PrincipalName  string    `json:"principal_name"`
	PrincipalEmail string    `json:"principal_email"`
	IssuedAt       time.Time `json:"issued_at"`
	AccessExpiry   time.Time `json:"access_expiry"`
	// Scope is the token's authorization grant (the 2026-06-15 authz foundation):
	// "*" for an all-access (DCR/self-issued) token, or a namespace grant for a
	// future pre-issued scoped token. It is non-secret routing metadata, shown in
	// the admin connections table so the operator can see what each token may reach.
	Scope string `json:"scope"`
	// Domain is the trusted-"domain" entry this connection groups under (B-71 Stage 2d) —
	// the matched entry's identifier (CIMD + DCR connections sit under their domain); "" for
	// the operator self-issued / confidential / untrusted-leftover section. Non-secret.
	Domain string `json:"domain"`
}

// OAuthListPayload is the OAUTH_LIST response body: the live connections. The
// slice is always non-nil so the client receives [] rather than null on zero
// connections (the empty-state case).
type OAuthListPayload struct {
	Connections []OAuthConnectionInfo `json:"connections"`
}

// OAuthRevokeRequest is the OAUTH_REVOKE request body: the series id to revoke.
type OAuthRevokeRequest struct {
	SeriesID string `json:"series_id"`
}

// OAuthRevokePayload is the OAUTH_REVOKE ack: the series id that was revoked.
type OAuthRevokePayload struct {
	SeriesID string `json:"series_id"`
	Status   string `json:"status"`
}

// OAuthDeniedPayload is the OAUTH_DENIED frame's body. Reason is "forbidden"
// (caller is not an administrator) or "oauth_disabled" (OAuth is off).
type OAuthDeniedPayload struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// OAuthIssueSelfPayload is the OAUTH_ISSUE_SELF response body: the freshly minted
// access token (display-once — the operator copies it into their CLI config) and
// its expiry, so the UI can warn how long it lasts. The token is the one secret
// that crosses /ws/ui; it is never logged or stored beyond the token store.
type OAuthIssueSelfPayload struct {
	AccessToken  string    `json:"access_token"`
	AccessExpiry time.Time `json:"access_expiry"`
	Name         string    `json:"name,omitempty"`
}

// OAuthIssueSelfRequest carries the operator's per-issuance FINITE expiry (B-71 Stage 4):
// validity_seconds is the chosen token lifetime in whole seconds. 0/absent ⇒ the finite global
// default (never infinite); a NEGATIVE value is rejected (no indefinite).
type OAuthIssueSelfRequest struct {
	ValiditySeconds  int64          `json:"validity_seconds"`
	Name             string         `json:"name,omitempty"`
	ExtraPermissions map[string]any `json:"extra_permissions,omitempty"`
}
