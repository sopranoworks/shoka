package uiws

import (
	"net/http"
	"time"

	"github.com/sopranoworks/shoka/pkg/oauthstore"
	"github.com/sopranoworks/shoka/pkg/userstore"
)

// OAuthConnectionStore is the narrow capability the OAuth management requests
// (OAUTH_LIST/OAUTH_REVOKE) depend on — exactly the (b) oauthstore's no-secret
// List and per-series Revoke. CoreHandlers depends on this interface, not the
// concrete *oauthstore.Store, so the handle stays nil when OAuth is disabled and
// tests can inject a fake. NO store change is implied: *oauthstore.Store already
// satisfies it.
type OAuthConnectionStore interface {
	List() ([]oauthstore.SeriesInfo, error)
	Revoke(seriesID string) error
	// RevokeByPrincipalEmail revokes every token series (and pending auth code) for a
	// principal email — the cross-store access cut when a user is disabled or deleted
	// (B-28). Returns the number of series revoked.
	RevokeByPrincipalEmail(email string) (int, error)

	// B-71 Stage 2d — the dynamic "domain" registration store the domain-mode management
	// screen drives (DOMAIN_* ws ops) + the connection grouping. *oauthstore.Store already
	// satisfies these.
	ListRegistrations() ([]oauthstore.RegistrationEntry, error)
	CreateRegistration(mode, identifier string, now time.Time) (oauthstore.RegistrationEntry, error)
	GetRegistration(id string) (oauthstore.RegistrationEntry, error)
	UpdateRegistration(entry oauthstore.RegistrationEntry) error
	DeleteRegistration(id string) error
	// GenerateDomainConsent mints (or re-rolls) a domain entry's plaintext per-domain consent and
	// returns it (2026-06-20 model — operator-readable, refreshable).
	GenerateDomainConsent(id string) (string, error)
	// RevokeByDomain revokes every token series under a domain (the Stage 2d domain-delete
	// cascade). Returns the number revoked.
	RevokeByDomain(domain string) (int, error)
	// DomainEntryForClient returns the "domain" entry a connection's client_id belongs to (for
	// grouping); ok=false for a self-issued/confidential or untrusted-leftover connection.
	DomainEntryForClient(clientID string) (oauthstore.RegistrationEntry, bool)
	// IssueConfidentialClient mints a confidential pre-issued client (B-71 Stage 3): a client_id
	// + a high-entropy secret; only the hash is stored; the RAW secret is returned ONCE.
	IssueConfidentialClient(scope string, validity time.Duration, now time.Time) (oauthstore.RegistrationEntry, string, error)
	// RevokeByClientID revokes every token series issued to a client_id (the confidential-client
	// delete cascade). Returns the number revoked.
	RevokeByClientID(clientID string) (int, error)
}

// OAuthSelfIssuer mints a fresh access token bound to the current-mode operator
// (the "token to self" path, B-46b §2.2). It is a SEPARATE capability from
// OAuthConnectionStore so the holder stays free of oauth/serverurl/identity
// wiring: the concrete issuer is built in cmd/shoka (it holds the store, the
// operator principal, the TTLs, and the resource deriver) and injected via
// SetOAuthSelfIssuer. The request is passed so the issuer can derive the RFC 8707
// resource exactly as /authorize does (forwarded-header aware). accessTTL is the
// operator's per-issuance FINITE lifetime chosen at issue time (B-71 Stage 4); a
// 0/non-positive value means "use the finite global default" — never infinite. It
// returns the access token and its expiry; the holder never sees how it is minted.
// nil when OAuth is disabled.
type OAuthSelfIssuer interface {
	IssueSelf(r *http.Request, accessTTL time.Duration) (accessToken string, accessExpiry time.Time, err error)
}

// OAuthSelfIssuerFunc adapts a function to OAuthSelfIssuer.
type OAuthSelfIssuerFunc func(r *http.Request, accessTTL time.Duration) (string, time.Time, error)

// IssueSelf calls f.
func (f OAuthSelfIssuerFunc) IssueSelf(r *http.Request, accessTTL time.Duration) (string, time.Time, error) {
	return f(r, accessTTL)
}

// UserAdminStore is the narrow capability the super-user-only user-management ops
// (B-28 stage 3) depend on — exactly the userstore admin/invite methods. CoreHandlers
// depends on this interface, not the concrete *userstore.Store, so the handle stays
// nil when the user store is absent and tests can inject a fake. *userstore.Store
// already satisfies it.
type UserAdminStore interface {
	ListUsers() ([]userstore.UserInfo, error)
	UpdateUserScope(email, scope string) error
	SetUserDisabled(email string, disabled bool) error
	RemoveUser(email string) error
	CreateInvite(email, scope, createdBy string, now time.Time, ttl time.Duration) (string, userstore.InviteRecord, error)
	ListInvites() ([]userstore.InviteInfo, error)
	RevokeInvite(codeHash string) error
	// GetUser / PutUser back the self-service "My Account" ops (read own record;
	// persist a name or re-hashed password). *userstore.Store already satisfies them.
	GetUser(email string) (*userstore.UserRecord, error)
	PutUser(rec *userstore.UserRecord) error
	// SetUserPassword resets a target's password hash and drops their sessions in one
	// tx (the admin reset, case 1). *userstore.Store already satisfies it.
	SetUserPassword(email, passwordHash string) error
}
