package uiws

import (
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/sopranoworks/shoka/pkg/oauthstore"
)

// OAuth/domain/confidential management handlers — the OAuth slice of CoreHandlers
// (the 2026-06-21 core-handler extraction). They need ONLY the OAuth stores
// (h.oauth / h.selfIssuer), never a document StorageService. Moved verbatim from
// manager.go; behaviour is unchanged.

// oauthAvailable is the OAUTH_* CAPABILITY check: it returns false (sending an
// OAUTH_DENIED "oauth_disabled" frame) when OAuth is off so there is no store, so the
// handlers never nil-panic. Administrator AUTHORIZATION is NOT checked here — it is
// enforced upstream by the single stage-2 dispatch authzGate (OAUTH_* are admin-level
// in wsLevels). This replaced the retired adminGate/singleUserAdmin seam (stage 4).
func (h *CoreHandlers) oauthAvailable(client *Client) bool {
	if h.oauth == nil {
		client.SendResponse(MsgOAuthDenied, OAuthDeniedPayload{
			Reason:  "oauth_disabled",
			Message: "OAuth is not enabled on this server",
		})
		return false
	}
	return true
}

// handleOAuthList returns the live OAuth/MCP connections as no-secret summaries
// (oauthstore.SeriesInfo). Administrator-only (the dispatch authz gate). Read-only — no commit,
// no NOTIFY — so, like handleSearchFiles, it carries no identity or sender
// context. The Connections slice is always non-nil so the wire shape is always
// {"connections": [...]} (the empty-state client renders [] as "no connections").
func (h *CoreHandlers) handleOAuthList(client *Client) {
	if !h.oauthAvailable(client) {
		return
	}
	infos, err := h.oauth.List()
	if err != nil {
		client.SendError(fmt.Sprintf("Failed to list OAuth connections: %v", err))
		return
	}
	// Defined order (the 2026-06-15 admin-UI directive): newest connection first.
	// Store.List() iterates bbolt key order (= random series-id order), which left
	// the admin table in an unexplained sequence; the handler is the sort owner so
	// the wire response is authoritatively issued_at-descending. Ties (same issue
	// instant) fall back to the series id for a stable, deterministic order.
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].IssuedAt.Equal(infos[j].IssuedAt) {
			return infos[i].SeriesID < infos[j].SeriesID
		}
		return infos[i].IssuedAt.After(infos[j].IssuedAt)
	})
	conns := make([]OAuthConnectionInfo, 0, len(infos))
	for _, s := range infos {
		c := toOAuthConnectionInfo(s)
		// B-71 Stage 2d: tag each connection with the trusted-"domain" entry it groups under
		// (CIMD + DCR sit under their domain; self-issued/confidential/untrusted ⇒ ""). The
		// UI groups by this; OAUTH_LIST stays a flat array so existing readers are unaffected.
		if entry, ok := h.oauth.DomainEntryForClient(s.ClientID); ok {
			c.Domain = entry.Identifier
		}
		conns = append(conns, c)
	}
	client.SendResponse(MsgOAuthList, OAuthListPayload{Connections: conns})
}

// handleOAuthRevoke revokes one connection by series id (oauthstore.Revoke).
// Administrator-only (the dispatch authz gate). Revoking one series leaves every other intact
// (the store guarantees it). An absent series_id is a typed error rather than a
// silent no-op; a well-formed but already-revoked id succeeds idempotently (the
// store's Revoke is idempotent — the right behaviour when two admins race or the
// row is already gone).
func (h *CoreHandlers) handleOAuthRevoke(client *Client, payload json.RawMessage) {
	if !h.oauthAvailable(client) {
		return
	}
	var p OAuthRevokeRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for OAUTH_REVOKE")
		return
	}
	if p.SeriesID == "" {
		client.SendError("OAUTH_REVOKE requires a series_id")
		return
	}
	if err := h.oauth.Revoke(p.SeriesID); err != nil {
		client.SendError(fmt.Sprintf("Failed to revoke OAuth connection: %v", err))
		return
	}
	client.SendResponse(MsgOAuthRevoke, OAuthRevokePayload{SeriesID: p.SeriesID, Status: "ok"})
	h.notifyOAuthChange()
}

// --- B-71 Stage 2d: domain-mode management (DOMAIN_* ws ops) ---

// DomainInfo is the no-secret view of a "domain" RegistrationEntry: its identifier, per-domain
// TTL (seconds; 0 = unset → the finite global default), and its per-domain consent VALUE. The
// consent is PLAINTEXT and intentionally operator-readable (the 2026-06-20 threat model) — it is
// returned here so the card can always show it; "" means no consent is set (⇒ the domain cannot
// authorize connections until the operator generates one).
type DomainInfo struct {
	ID                string `json:"id"`
	Domain            string `json:"domain"`
	AccessTTLSeconds  int64  `json:"access_ttl_seconds"`
	RefreshTTLSeconds int64  `json:"refresh_ttl_seconds"`
	Consent           string `json:"consent"`
	Scope             string `json:"scope"`
	RevokedTokens     int    `json:"revoked_tokens,omitempty"`
}

// DomainListPayload is the DOMAIN_LIST response (sorted by identifier).
type DomainListPayload struct {
	Domains []DomainInfo `json:"domains"`
}

// DomainCreateRequest creates a "domain" entry. Consent is NOT set here — the operator generates it
// afterwards via DOMAIN_GENERATE_CONSENT (the 2026-06-20 plaintext/generate model).
type DomainCreateRequest struct {
	Domain            string `json:"domain"`
	AccessTTLSeconds  int64  `json:"access_ttl_seconds"`
	RefreshTTLSeconds int64  `json:"refresh_ttl_seconds"`
	Scope             string `json:"scope"`
}

// DomainUpdateRequest edits a domain's TTL and/or scope. Consent is managed by
// DOMAIN_GENERATE_CONSENT, not here. A scope change triggers batch revocation of all
// active tokens issued to the domain (the client confirmed via a modal before sending).
type DomainUpdateRequest struct {
	ID                string `json:"id"`
	AccessTTLSeconds  int64  `json:"access_ttl_seconds"`
	RefreshTTLSeconds int64  `json:"refresh_ttl_seconds"`
	Scope             string `json:"scope"`
}

// DomainGenerateConsentRequest / DomainGenerateConsentPayload — mint (or re-roll) a domain's
// per-domain consent value and return it. The value is PLAINTEXT and operator-readable.
type DomainGenerateConsentRequest struct {
	ID string `json:"id"`
}
type DomainGenerateConsentPayload struct {
	ID      string `json:"id"`
	Consent string `json:"consent"`
}

// DomainDeleteRequest / DomainDeletePayload — delete a domain (revoking its tokens).
type DomainDeleteRequest struct {
	ID string `json:"id"`
}
type DomainDeletePayload struct {
	ID            string `json:"id"`
	RevokedTokens int    `json:"revoked_tokens"`
	Status        string `json:"status"`
}

func domainInfoOf(e oauthstore.RegistrationEntry) DomainInfo {
	scope := e.Scope
	if scope == "" {
		scope = "*"
	}
	di := DomainInfo{ID: e.ID, Domain: e.Identifier, Consent: e.ConsentValue(), Scope: scope}
	if e.TTL != nil {
		di.AccessTTLSeconds = e.TTL.AccessSeconds
		di.RefreshTTLSeconds = e.TTL.RefreshSeconds
	}
	return di
}

func (h *CoreHandlers) handleDomainList(client *Client) {
	if !h.oauthAvailable(client) {
		return
	}
	entries, err := h.oauth.ListRegistrations()
	if err != nil {
		client.SendError(fmt.Sprintf("Failed to list domains: %v", err))
		return
	}
	out := make([]DomainInfo, 0)
	for _, e := range entries {
		if e.RegistrationMode == oauthstore.RegistrationModeDomain {
			out = append(out, domainInfoOf(e))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	client.SendResponse(MsgDomainList, DomainListPayload{Domains: out})
}

func (h *CoreHandlers) handleDomainCreate(client *Client, payload json.RawMessage) {
	if !h.oauthAvailable(client) {
		return
	}
	var p DomainCreateRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for DOMAIN_CREATE")
		return
	}
	if strings.TrimSpace(p.Domain) == "" {
		client.SendError("DOMAIN_CREATE requires a domain")
		return
	}
	if p.AccessTTLSeconds < 0 || p.RefreshTTLSeconds < 0 {
		client.SendError("DOMAIN_CREATE TTL must not be negative")
		return
	}
	entry, err := h.oauth.CreateRegistration(oauthstore.RegistrationModeDomain, p.Domain, time.Now())
	if err != nil {
		client.SendError(fmt.Sprintf("Failed to create domain: %v", err))
		return
	}
	needUpdate := false
	if p.AccessTTLSeconds > 0 || p.RefreshTTLSeconds > 0 {
		entry.TTL = &oauthstore.EntryTTL{AccessSeconds: p.AccessTTLSeconds, RefreshSeconds: p.RefreshTTLSeconds}
		needUpdate = true
	}
	scope := strings.TrimSpace(p.Scope)
	if scope != "" && scope != "*" {
		entry.Scope = scope
		needUpdate = true
	}
	if needUpdate {
		if err := h.oauth.UpdateRegistration(entry); err != nil {
			client.SendError(fmt.Sprintf("Failed to set domain fields: %v", err))
			return
		}
	}
	client.SendResponse(MsgDomainCreate, domainInfoOf(entry))
	h.notifyOAuthChange()
}

func (h *CoreHandlers) handleDomainUpdate(client *Client, payload json.RawMessage) {
	if !h.oauthAvailable(client) {
		return
	}
	var p DomainUpdateRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for DOMAIN_UPDATE")
		return
	}
	if p.ID == "" {
		client.SendError("DOMAIN_UPDATE requires an id")
		return
	}
	if p.AccessTTLSeconds < 0 || p.RefreshTTLSeconds < 0 {
		client.SendError("DOMAIN_UPDATE TTL must not be negative")
		return
	}
	entry, err := h.oauth.GetRegistration(p.ID)
	if err != nil {
		client.SendError("Domain not found")
		return
	}
	if entry.RegistrationMode != oauthstore.RegistrationModeDomain {
		client.SendError("not a domain entry")
		return
	}
	if p.AccessTTLSeconds > 0 || p.RefreshTTLSeconds > 0 {
		entry.TTL = &oauthstore.EntryTTL{AccessSeconds: p.AccessTTLSeconds, RefreshSeconds: p.RefreshTTLSeconds}
	} else {
		entry.TTL = nil // both 0 ⇒ unset → the finite global default
	}
	// Scope change detection: normalize both sides (empty/"*" ⇒ "*") before comparing.
	oldScope := entry.Scope
	if oldScope == "" {
		oldScope = "*"
	}
	newScope := strings.TrimSpace(p.Scope)
	if newScope == "" {
		newScope = "*"
	}
	scopeChanged := oldScope != newScope
	var revoked int
	if scopeChanged {
		revoked, _ = h.oauth.RevokeByDomain(entry.Identifier)
		if newScope == "*" {
			entry.Scope = ""
		} else {
			entry.Scope = newScope
		}
	}
	if err := h.oauth.UpdateRegistration(entry); err != nil {
		client.SendError(fmt.Sprintf("Failed to update domain: %v", err))
		return
	}
	di := domainInfoOf(entry)
	di.RevokedTokens = revoked
	client.SendResponse(MsgDomainUpdate, di)
	h.notifyOAuthChange()
}

// handleDomainGenerateConsent mints (or re-rolls) a domain's per-domain consent value and returns
// the PLAINTEXT value once in the response. Calling it again regenerates (the old value stops
// verifying). Admin-gated like the other DOMAIN_* ops.
func (h *CoreHandlers) handleDomainGenerateConsent(client *Client, payload json.RawMessage) {
	if !h.oauthAvailable(client) {
		return
	}
	var p DomainGenerateConsentRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for DOMAIN_GENERATE_CONSENT")
		return
	}
	if p.ID == "" {
		client.SendError("DOMAIN_GENERATE_CONSENT requires an id")
		return
	}
	value, err := h.oauth.GenerateDomainConsent(p.ID)
	if err != nil {
		client.SendError(fmt.Sprintf("Failed to generate consent: %v", err))
		return
	}
	client.SendResponse(MsgDomainGenerateConsent, DomainGenerateConsentPayload{ID: p.ID, Consent: value})
	h.notifyOAuthChange()
}

func (h *CoreHandlers) handleDomainDelete(client *Client, payload json.RawMessage) {
	if !h.oauthAvailable(client) {
		return
	}
	var p DomainDeleteRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for DOMAIN_DELETE")
		return
	}
	if p.ID == "" {
		client.SendError("DOMAIN_DELETE requires an id")
		return
	}
	entry, err := h.oauth.GetRegistration(p.ID)
	if err != nil {
		client.SendError("Domain not found")
		return
	}
	// Policy (B-71 Stage 2d): deleting a domain makes it untrusted AND revokes its live tokens
	// (mirroring the user-delete OAuth-revoke cascade) — its connections cannot keep working.
	revoked, _ := h.oauth.RevokeByDomain(entry.Identifier)
	if err := h.oauth.DeleteRegistration(p.ID); err != nil {
		client.SendError(fmt.Sprintf("Failed to delete domain: %v", err))
		return
	}
	client.SendResponse(MsgDomainDelete, DomainDeletePayload{ID: p.ID, RevokedTokens: revoked, Status: "ok"})
	h.notifyOAuthChange()
}

// --- Confidential-client management (B-71 Stage 3) --------------------------

// ConfidentialClientInfo is the no-secret view of a "confidential" RegistrationEntry: its issued
// client_id, pre-issued scope, finite credential expiry, and creation time. The secret/hash is
// NEVER returned (only the issue response carries the raw secret, once).
type ConfidentialClientInfo struct {
	ID        string `json:"id"`
	ClientID  string `json:"client_id"`
	Name      string `json:"name,omitempty"`
	Scope     string `json:"scope"`
	ExpiresAt string `json:"expires_at"` // RFC3339
	CreatedAt string `json:"created_at"` // RFC3339
}

// ConfidentialListPayload is the CLIENT_LIST response (newest first).
type ConfidentialListPayload struct {
	Clients []ConfidentialClientInfo `json:"clients"`
}

// ConfidentialIssueRequest issues a confidential client: a pre-issued scope + a finite validity
// in seconds (no indefinite).
type ConfidentialIssueRequest struct {
	Scope           string `json:"scope"`
	Name            string `json:"name,omitempty"`
	ValiditySeconds int64  `json:"validity_seconds"`
}

// ConfidentialIssuePayload returns the no-secret info PLUS the raw client_secret — shown ONCE at
// issuance and never persisted or returned again.
type ConfidentialIssuePayload struct {
	ConfidentialClientInfo
	ClientSecret string `json:"client_secret"`
}

// ConfidentialRevokeRequest / ConfidentialRevokePayload — revoke a confidential client (deleting
// its entry and cascade-revoking the tokens it issued).
type ConfidentialRevokeRequest struct {
	ID string `json:"id"`
}
type ConfidentialRevokePayload struct {
	ID            string `json:"id"`
	RevokedTokens int    `json:"revoked_tokens"`
	Status        string `json:"status"`
}

func confidentialInfoOf(e oauthstore.RegistrationEntry) ConfidentialClientInfo {
	return ConfidentialClientInfo{
		ID:        e.ID,
		ClientID:  e.Identifier,
		Name:      e.Name,
		Scope:     e.Scope,
		ExpiresAt: e.ExpiresAt.UTC().Format(time.RFC3339),
		CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func (h *CoreHandlers) handleConfidentialList(client *Client) {
	if !h.oauthAvailable(client) {
		return
	}
	entries, err := h.oauth.ListRegistrations()
	if err != nil {
		client.SendError(fmt.Sprintf("Failed to list confidential clients: %v", err))
		return
	}
	out := make([]ConfidentialClientInfo, 0)
	for _, e := range entries {
		if e.RegistrationMode == oauthstore.RegistrationModeConfidential {
			out = append(out, confidentialInfoOf(e))
		}
	}
	// Newest first (CreatedAt is RFC3339, so lexical order is chronological).
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	client.SendResponse(MsgClientList, ConfidentialListPayload{Clients: out})
}

func (h *CoreHandlers) handleConfidentialIssue(client *Client, payload json.RawMessage) {
	if !h.oauthAvailable(client) {
		return
	}
	var p ConfidentialIssueRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for CLIENT_ISSUE")
		return
	}
	if strings.TrimSpace(p.Scope) == "" {
		client.SendError("CLIENT_ISSUE requires a scope")
		return
	}
	if p.ValiditySeconds <= 0 {
		client.SendError("CLIENT_ISSUE requires a finite positive validity (no indefinite)")
		return
	}
	entry, secret, err := h.oauth.IssueConfidentialClient(p.Scope, strings.TrimSpace(p.Name), time.Duration(p.ValiditySeconds)*time.Second, time.Now())
	if err != nil {
		client.SendError(fmt.Sprintf("Failed to issue confidential client: %v", err))
		return
	}
	// The raw secret crosses /ws/ui ONCE here (admin-gated), like OAUTH_ISSUE_SELF; it is never
	// logged and never returned again.
	client.SendResponse(MsgClientIssue, ConfidentialIssuePayload{
		ConfidentialClientInfo: confidentialInfoOf(entry),
		ClientSecret:           secret,
	})
	h.notifyOAuthChange()
}

func (h *CoreHandlers) handleConfidentialRevoke(client *Client, payload json.RawMessage) {
	if !h.oauthAvailable(client) {
		return
	}
	var p ConfidentialRevokeRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for CLIENT_REVOKE")
		return
	}
	if p.ID == "" {
		client.SendError("CLIENT_REVOKE requires an id")
		return
	}
	entry, err := h.oauth.GetRegistration(p.ID)
	if err != nil {
		client.SendError("Confidential client not found")
		return
	}
	if entry.RegistrationMode != oauthstore.RegistrationModeConfidential {
		client.SendError("not a confidential client")
		return
	}
	// Cascade: revoking the credential cuts the tokens it issued, then deletes the entry.
	revoked, _ := h.oauth.RevokeByClientID(entry.Identifier)
	if err := h.oauth.DeleteRegistration(p.ID); err != nil {
		client.SendError(fmt.Sprintf("Failed to revoke confidential client: %v", err))
		return
	}
	client.SendResponse(MsgClientRevoke, ConfidentialRevokePayload{ID: p.ID, RevokedTokens: revoked, Status: "ok"})
	h.notifyOAuthChange()
}

// handleOAuthIssueSelf mints a fresh access token bound to the current-mode
// operator (the "token to self" path, B-46b §2.2) and returns it ONCE. It is the
// only place a secret token crosses /ws/ui — a deliberate, admin-gated exception
// so the operator can paste the token into their CLI client config. The token is
// NOT logged (no log statement carries it) and is persisted only in the normal
// token store. Administrator-only via the dispatch authz gate, like List/Revoke; the
// issuer being nil (OAuth disabled) is reported as oauth_disabled.
func (h *CoreHandlers) handleOAuthIssueSelf(client *Client, payload json.RawMessage) {
	if !h.oauthAvailable(client) {
		return
	}
	if h.selfIssuer == nil {
		client.SendResponse(MsgOAuthDenied, OAuthDeniedPayload{
			Reason:  "oauth_disabled",
			Message: "token issuance is not available on this server",
		})
		return
	}
	// B-71 Stage 4 — the operator chooses a per-issuance FINITE expiry. An absent/empty payload
	// (validity_seconds 0) falls back to the finite global default; a NEGATIVE value is rejected
	// (no indefinite). A positive value sets the issued token's lifetime.
	var p OAuthIssueSelfRequest
	if len(payload) > 0 {
		if err := json.Unmarshal(payload, &p); err != nil {
			client.SendError("Invalid payload for OAUTH_ISSUE_SELF")
			return
		}
	}
	if p.ValiditySeconds < 0 {
		client.SendError("OAUTH_ISSUE_SELF validity must not be negative (no indefinite)")
		return
	}
	name := strings.TrimSpace(p.Name)
	scope := strings.TrimSpace(p.Scope)
	if scope == "" {
		scope = "*"
	}
	token, expiry, err := h.selfIssuer.IssueSelf(client.req, name, scope, time.Duration(p.ValiditySeconds)*time.Second, p.ExtraPermissions)
	if err != nil {
		// The error is generic on the wire; the token never appears in it.
		client.SendError("Failed to issue a token")
		log.Printf("OAUTH_ISSUE_SELF mint failed: %v", err)
		return
	}
	client.SendResponse(MsgOAuthIssueSelf, OAuthIssueSelfPayload{
		AccessToken:  token,
		AccessExpiry: expiry,
		Name:         name,
		Scope:        scope,
	})
	h.notifyOAuthChange()
}

// toOAuthConnectionInfo maps a store SeriesInfo to the no-secret wire DTO,
// computing a short series-id prefix for display. The full series id rides along
// as the revoke target (it is not a bearer credential).
func toOAuthConnectionInfo(s oauthstore.SeriesInfo) OAuthConnectionInfo {
	short := s.SeriesID
	if len(short) > 8 {
		short = short[:8]
	}
	return OAuthConnectionInfo{
		SeriesID:       s.SeriesID,
		SeriesIDShort:  short,
		Name:           s.Name,
		ClientID:       s.ClientID,
		PrincipalName:  s.Principal.Name,
		PrincipalEmail: s.Principal.Email,
		IssuedAt:       s.IssuedAt,
		AccessExpiry:   s.AccessExpiry,
		Scope:          s.Scope,
	}
}
