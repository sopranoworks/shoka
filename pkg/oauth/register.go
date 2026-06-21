package oauth

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/sopranoworks/shoka/pkg/oauthstore"
)

// This file implements OAuth 2.0 Dynamic Client Registration (RFC 7591), the
// 2026-06-12 B-63 directive. claude.ai's official connector docs
// (support.claude.com/en/articles/11503834) REQUIRE DCR for OAuth-based remote
// MCP servers and do NOT yet support a manually-supplied client_id/secret, so a
// CIMD-only AS cannot complete the claude.ai connect. /register accepts the
// client-metadata POST, issues + PERSISTS a PUBLIC client_id (no secret —
// token_endpoint_auth_method "none", matching the AS's advertised posture and the
// MCP public-client + PKCE model), and returns the RFC 7591 registration response.
//
// DCR is ADDITIVE to CIMD: a DCR client_id is an opaque handle (NewHandle), a CIMD
// client_id is an https URL, so /authorize and /token distinguish them and CIMD is
// unchanged. Only the supported grant/response/auth methods are accepted; a
// confidential-client request (any token_endpoint_auth_method other than "none")
// is rejected — there is no confidential-client path.

// registrationRequest is the RFC 7591 §2 client-metadata document a client POSTs.
// Unknown fields are tolerated (forward-compatible). Only the fields Shoka acts on
// or echoes are modelled.
type registrationRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	ClientName              string   `json:"client_name"`
	ClientURI               string   `json:"client_uri"`
	Scope                   string   `json:"scope"`
}

// registrationResponse is the RFC 7591 §3.2.1 successful registration response.
// No client_secret is issued — this is a public client.
type registrationResponse struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	ClientName              string   `json:"client_name,omitempty"`
	ClientURI               string   `json:"client_uri,omitempty"`
	Scope                   string   `json:"scope,omitempty"`
}

// supportedGrantTypes / supportedResponseTypes mirror what the AS metadata
// advertises; a registration requesting anything outside these is rejected.
var (
	supportedGrantTypes    = map[string]bool{"authorization_code": true, "refresh_token": true}
	supportedResponseTypes = map[string]bool{"code": true}
)

// handleRegister implements the RFC 7591 registration endpoint. It is mounted
// WITHOUT a bearer (like /authorize and /token — registration is how a client
// becomes known). It accepts a JSON client-metadata document, issues + persists a
// public client_id, and returns the 201 registration response.
func (s *AuthServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	lg := s.reqLogger(r)
	if r.Method != http.MethodPost {
		lg.Warn("oauth register rejected", "error", "invalid_request", "reason", "method-not-post")
		s.registerError(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	// RFC 7591 §3.1: the request body is a JSON client-metadata document.
	ct := r.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ct)), "application/json") {
		lg.Warn("oauth register rejected", "error", "invalid_client_metadata", "reason", "content-type-not-json")
		s.registerError(w, http.StatusBadRequest, "invalid_client_metadata", "Content-Type must be application/json")
		return
	}
	var req registrationRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := dec.Decode(&req); err != nil {
		lg.Warn("oauth register rejected", "error", "invalid_client_metadata", "reason", "body-not-json")
		s.registerError(w, http.StatusBadRequest, "invalid_client_metadata", "request body is not a valid JSON client metadata document")
		return
	}

	// redirect_uris: required and each must be a syntactically valid absolute URI
	// (RFC 7591 §2 / §3.2.2 invalid_redirect_uri). The per-request /authorize
	// binding still applies the exact / loopback-port match at use time.
	if len(req.RedirectURIs) == 0 {
		lg.Warn("oauth register rejected", "error", "invalid_redirect_uri", "reason", "no-redirect-uris")
		s.registerError(w, http.StatusBadRequest, "invalid_redirect_uri", "at least one redirect_uri is required")
		return
	}
	for _, ru := range req.RedirectURIs {
		if !validRegistrationRedirectURI(ru) {
			lg.Warn("oauth register rejected", "error", "invalid_redirect_uri", "reason", "malformed-redirect-uri")
			s.registerError(w, http.StatusBadRequest, "invalid_redirect_uri", "a redirect_uri is not a valid absolute URI")
			return
		}
	}

	// Public client only: token_endpoint_auth_method must be "none" (or omitted,
	// which we treat as "none"). A confidential method is rejected — there is no
	// client_secret path, matching token_endpoint_auth_methods_supported:["none"].
	authMethod := strings.TrimSpace(req.TokenEndpointAuthMethod)
	if authMethod == "" {
		authMethod = "none"
	}
	if authMethod != "none" {
		lg.Warn("oauth register rejected", "error", "invalid_client_metadata", "reason", "non-public-auth-method")
		s.registerError(w, http.StatusBadRequest, "invalid_client_metadata",
			"only token_endpoint_auth_method \"none\" (public client) is supported")
		return
	}

	// grant_types / response_types: default to the supported set when omitted;
	// reject anything outside what the AS advertises.
	grantTypes := req.GrantTypes
	if len(grantTypes) == 0 {
		grantTypes = []string{"authorization_code", "refresh_token"}
	}
	for _, g := range grantTypes {
		if !supportedGrantTypes[g] {
			lg.Warn("oauth register rejected", "error", "invalid_client_metadata", "reason", "unsupported-grant-type")
			s.registerError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported grant_type requested")
			return
		}
	}
	responseTypes := req.ResponseTypes
	if len(responseTypes) == 0 {
		responseTypes = []string{"code"}
	}
	for _, rt := range responseTypes {
		if !supportedResponseTypes[rt] {
			lg.Warn("oauth register rejected", "error", "invalid_client_metadata", "reason", "unsupported-response-type")
			s.registerError(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported response_type requested")
			return
		}
	}

	// Trusted-domain gate (B-71 Stage 2c, the enforcement deferred from Stage 2a): a DCR
	// client is registered only if its derived domain (the redirect_uris host) is a trusted
	// "domain" entry — REJECT otherwise, consistent with how the CIMD verifier rejects an
	// untrusted client_id host (ErrUntrustedDomain). A multi-host / unparseable client has no
	// single domain and is likewise rejected. After the startup seed, exactly the domains that
	// were trusted via static config are trusted here.
	domain := oauthstore.DomainFromRedirectURIs(req.RedirectURIs)
	if domain == "" || !s.store.TrustedDomain(domain) {
		lg.Warn("oauth register rejected", "error", "invalid_client_metadata", "reason", "untrusted-domain")
		s.registerError(w, http.StatusBadRequest, "invalid_client_metadata",
			"the client's redirect_uris host is not a trusted domain")
		return
	}

	clientID, err := oauthstore.NewHandle()
	if err != nil {
		lg.Error("oauth register failed", "reason", "handle-generation-failed")
		s.registerError(w, http.StatusInternalServerError, "server_error", "could not issue client_id")
		return
	}
	issuedAt := s.now()
	rec := oauthstore.RegisteredClient{
		ClientID:                clientID,
		RedirectURIs:            req.RedirectURIs,
		TokenEndpointAuthMethod: "none",
		ClientName:              req.ClientName,
		GrantTypes:              grantTypes,
		ResponseTypes:           responseTypes,
		ClientIDIssuedAt:        issuedAt,
		// B-71 Stage 2a recorded the domain; Stage 2c gated on it above (so it is non-empty
		// and trusted here). The series issued for this client groups under this domain.
		Domain: domain,
	}
	if err := s.store.PutClient(rec); err != nil {
		lg.Error("oauth register failed", "reason", "client-persist-failed")
		s.registerError(w, http.StatusInternalServerError, "server_error", "could not persist client registration")
		return
	}
	// Registered: log the issuance (client_id + redirect target count + name) — no
	// secret is involved (a public client has none). The client_id is a public
	// identifier, safe to log.
	lg.Info("oauth client registered",
		"client_id", clientID,
		"client_name", req.ClientName,
		"redirect_uri_count", len(req.RedirectURIs),
		"token_endpoint_auth_method", "none",
	)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(registrationResponse{
		ClientID:                clientID,
		ClientIDIssuedAt:        issuedAt.Unix(),
		RedirectURIs:            req.RedirectURIs,
		TokenEndpointAuthMethod: "none",
		GrantTypes:              grantTypes,
		ResponseTypes:           responseTypes,
		ClientName:              req.ClientName,
		ClientURI:               req.ClientURI,
		Scope:                   req.Scope,
	})
}

// registerError writes an RFC 7591 §3.2.2 error response (a JSON object with
// "error" and optional "error_description").
func (s *AuthServer) registerError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             code,
		"error_description": desc,
	})
}

// validRegistrationRedirectURI reports whether ru is a syntactically acceptable
// redirect_uri for registration: a parseable absolute URI with a scheme. https,
// loopback http, and native-app custom schemes are all permitted at registration
// time; the exact-match / loopback-port binding is enforced per-request at
// /authorize (RedirectURIAllowed). A bare or relative reference is rejected.
func validRegistrationRedirectURI(ru string) bool {
	ru = strings.TrimSpace(ru)
	if ru == "" || strings.ContainsAny(ru, " \t\r\n") {
		return false
	}
	u, err := url.Parse(ru)
	if err != nil || !u.IsAbs() || u.Scheme == "" {
		return false
	}
	return true
}

// isDCRClientID reports whether clientID is a Dynamic-Client-Registration handle
// rather than a CIMD client_id. CIMD client_ids are absolute https URLs; a DCR
// client_id is an opaque NewHandle (base64url), which never parses as an https
// URL. This is the discriminator /authorize and /token use to route to the DCR
// store (GetClient) vs the CIMD verifier — keeping the CIMD path byte-identical.
func isDCRClientID(clientID string) bool {
	u, err := url.Parse(strings.TrimSpace(clientID))
	if err != nil {
		return true // unparseable as a URL → not a CIMD URL → treat as a DCR handle
	}
	return !(u.Scheme == "https" && u.Host != "")
}
