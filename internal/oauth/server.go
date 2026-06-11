package oauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"encoding/base64"
	"encoding/json"

	"github.com/shoka/mcp-server/internal/serverurl"
	"github.com/shoka/mcp-server/internal/storage/oauthstore"
)

// This file builds Shoka's built-in OAuth 2.1 authorization-server core (the
// 2026-06-03 MCP OAuth (b) directive): the /authorize consent gate (+ PKCE S256)
// and the /token endpoint (form-urlencoded; authorization_code -> access +
// rotating refresh; RFC 8707 audience). CIMD (cimd.go) is the only client
// registration path; the token state lives in the go-git-free oauthstore.
//
// The principal stamped on a token is the CURRENT-MODE principal, obtained from
// the PrincipalAuthenticator seam — NOT a baked-in single-user constant. Today
// that seam authenticates with a configured consent credential and returns the
// configured operator; multi-user enablement (a later B-28 leg) replaces the
// seam with per-user authentication, additively, with no change here.

// PrincipalAuthenticator is the pluggable "authenticate the principal" step the
// consent gate runs before approving. In single-user mode it validates a shared
// consent credential and returns the configured operator principal; under
// multi-user mode it is replaced by real per-user authentication returning that
// user's principal. ok=false means the submission failed authentication.
type PrincipalAuthenticator interface {
	Authenticate(r *http.Request) (principal oauthstore.Principal, ok bool)
}

// ConsentCredentialAuth is the single-user-mode PrincipalAuthenticator: it
// constant-time-compares a form-submitted credential against the configured
// consent credential and, on success, returns the one configured principal. This
// is the seam multi-user mode later replaces — the value here being one principal
// is a property of the current mode, not an assumption baked into the flow.
type ConsentCredentialAuth struct {
	Credential string
	Principal  oauthstore.Principal
}

// Authenticate validates the "consent_credential" form field. An empty configured
// credential denies all approvals (a safe default: consent cannot be granted until
// the operator sets one).
func (c ConsentCredentialAuth) Authenticate(r *http.Request) (oauthstore.Principal, bool) {
	if c.Credential == "" {
		return oauthstore.Principal{}, false
	}
	got := r.PostFormValue("consent_credential")
	if subtle.ConstantTimeCompare([]byte(got), []byte(c.Credential)) != 1 {
		return oauthstore.Principal{}, false
	}
	return c.Principal, true
}

// AuthServerConfig configures the AuthServer.
type AuthServerConfig struct {
	ExternalURL   string // Server.MCP.OAuth.ExternalURL; empty falls back to forwarded headers
	PrincipalAuth PrincipalAuthenticator
	AccessTTL     time.Duration
	RefreshTTL    time.Duration
	CodeTTL       time.Duration
	Logger        *slog.Logger // operational log for client-verification outcomes; nil → slog.Default()
}

// AuthServer serves /authorize and /token for the built-in AS.
type AuthServer struct {
	store         *oauthstore.Store
	verifier      *Verifier
	externalURL   string
	principalAuth PrincipalAuthenticator
	accessTTL     time.Duration
	refreshTTL    time.Duration
	codeTTL       time.Duration
	logger        *slog.Logger
	now           func() time.Time // injectable for tests
}

// NewAuthServer builds an AuthServer. TTLs default to sensible values when zero.
func NewAuthServer(store *oauthstore.Store, verifier *Verifier, cfg AuthServerConfig) *AuthServer {
	accessTTL := cfg.AccessTTL
	if accessTTL <= 0 {
		accessTTL = time.Hour
	}
	refreshTTL := cfg.RefreshTTL
	if refreshTTL <= 0 {
		refreshTTL = 30 * 24 * time.Hour
	}
	codeTTL := cfg.CodeTTL
	if codeTTL <= 0 {
		codeTTL = time.Minute
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &AuthServer{
		store:         store,
		verifier:      verifier,
		externalURL:   cfg.ExternalURL,
		principalAuth: cfg.PrincipalAuth,
		accessTTL:     accessTTL,
		refreshTTL:    refreshTTL,
		codeTTL:       codeTTL,
		logger:        logger,
		now:           time.Now,
	}
}

// RegisterEndpoints mounts /authorize and /token on mux. These are reachable
// without a bearer token (they are how a token is obtained).
func (s *AuthServer) RegisterEndpoints(mux *http.ServeMux) {
	mux.Handle("/authorize", http.HandlerFunc(s.handleAuthorize))
	mux.Handle("/token", http.HandlerFunc(s.handleToken))
}

// --- /authorize -------------------------------------------------------------

// authRequest is the validated authorization request, carried through the consent
// page's hidden fields between GET and POST.
type authRequest struct {
	ClientID            string
	RedirectURI         string
	ResponseType        string
	CodeChallenge       string
	CodeChallengeMethod string
	Resource            string
	State               string
	Scope               string
}

func (s *AuthServer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.authError(w, "invalid_request", "could not parse request")
		return
	}
	req := authRequest{
		ClientID:            r.FormValue("client_id"),
		RedirectURI:         r.FormValue("redirect_uri"),
		ResponseType:        r.FormValue("response_type"),
		CodeChallenge:       r.FormValue("code_challenge"),
		CodeChallengeMethod: r.FormValue("code_challenge_method"),
		Resource:            r.FormValue("resource"),
		State:               r.FormValue("state"),
		Scope:               r.FormValue("scope"),
	}

	// Validate the client via CIMD and the redirect_uri against its metadata
	// BEFORE we are willing to redirect anywhere — an unverified redirect_uri must
	// never receive a redirect (open-redirect / error-leak protection).
	md, err := s.verifier.Verify(r.Context(), req.ClientID)
	if err != nil {
		// Make the rejection self-diagnosing: an operator reading the log learns
		// the received client_id, the domain evaluated (or the stage reached),
		// why it was denied, and how many trusted domains are configured — so the
		// empty-list and wrong-domain cases reveal what to add. The wire response
		// is unchanged (the caller still gets invalid_client); only the diagnostic
		// payload goes to the operator's log. Secrets and the trusted-domain
		// VALUES are never logged (only the count).
		trustedCount := s.verifier.TrustedCount()
		s.logger.Warn("oauth client verification rejected",
			"client_id", req.ClientID,
			"evaluated_domain", clientIDDomain(req.ClientID),
			"reason", cimdRejectCategory(err, trustedCount),
			"trusted_domains_configured", trustedCount,
		)
		s.authError(w, "invalid_client", "client verification failed: "+cimdReason(err))
		return
	}
	// Success is equally observable: confirm which client_id/domain was accepted.
	s.logger.Info("oauth client verification accepted",
		"client_id", req.ClientID,
		"evaluated_domain", clientIDDomain(req.ClientID),
	)
	if !RedirectURIAllowed(req.RedirectURI, md.RedirectURIs) {
		s.authError(w, "invalid_request", "redirect_uri is not registered for this client")
		return
	}

	// From here a redirect_uri is trusted, so protocol errors redirect back per
	// OAuth 2.1 (RFC 6749 §4.1.2.1) rather than rendering on-page.
	if req.ResponseType != "code" {
		s.redirectError(w, r, req, "unsupported_response_type", "only response_type=code is supported")
		return
	}
	if req.CodeChallenge == "" || req.CodeChallengeMethod != "S256" {
		s.redirectError(w, r, req, "invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	resource, ok := s.resolveResource(r, req.Resource)
	if !ok {
		s.redirectError(w, r, req, "invalid_target", "resource does not identify this server")
		return
	}
	req.Resource = resource

	if r.Method == http.MethodGet {
		s.renderConsent(w, req, md, "")
		return
	}

	// POST: an approval or denial.
	switch {
	case r.PostFormValue("deny") != "":
		s.redirectError(w, r, req, "access_denied", "the operator denied the request")
		return
	case r.PostFormValue("approve") != "":
		principal, authed := s.principalAuth.Authenticate(r)
		if !authed {
			// Re-render the consent page with an error; do NOT redirect (the
			// credential was wrong — treat like an unauthenticated retry).
			w.WriteHeader(http.StatusUnauthorized)
			s.renderConsent(w, req, md, "Incorrect consent credential.")
			return
		}
		code, herr := oauthstore.NewHandle()
		if herr != nil {
			s.redirectError(w, r, req, "server_error", "could not issue code")
			return
		}
		now := s.now()
		perr := s.store.PutCode(code, oauthstore.CodeRecord{
			ClientID:            req.ClientID,
			RedirectURI:         req.RedirectURI,
			CodeChallenge:       req.CodeChallenge,
			CodeChallengeMethod: req.CodeChallengeMethod,
			Resource:            req.Resource,
			Principal:           principal,
			Expiry:              now.Add(s.codeTTL),
		})
		if perr != nil {
			s.redirectError(w, r, req, "server_error", "could not persist code")
			return
		}
		s.redirectSuccess(w, r, req, code)
		return
	default:
		s.renderConsent(w, req, md, "Choose approve or deny.")
	}
}

// resolveResource validates the RFC 8707 resource indicator: if present it must
// equal this server's MCP resource URL; if absent it defaults to that URL. Returns
// the bound audience and ok=false on a mismatch.
func (s *AuthServer) resolveResource(r *http.Request, presented string) (string, bool) {
	base, err := serverurl.Base(s.externalURL, r)
	if err != nil {
		return "", false
	}
	want := serverurl.ResourceURL(base)
	if strings.TrimSpace(presented) == "" {
		return want, true
	}
	if presented == want {
		return want, true
	}
	return "", false
}

func (s *AuthServer) redirectSuccess(w http.ResponseWriter, r *http.Request, req authRequest, code string) {
	u, err := url.Parse(req.RedirectURI)
	if err != nil {
		s.authError(w, "server_error", "bad redirect_uri")
		return
	}
	q := u.Query()
	q.Set("code", code)
	if req.State != "" {
		q.Set("state", req.State)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (s *AuthServer) redirectError(w http.ResponseWriter, r *http.Request, req authRequest, code, desc string) {
	u, err := url.Parse(req.RedirectURI)
	if err != nil {
		s.authError(w, code, desc)
		return
	}
	q := u.Query()
	q.Set("error", code)
	if desc != "" {
		q.Set("error_description", desc)
	}
	if req.State != "" {
		q.Set("state", req.State)
	}
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// authError renders an on-page error (used only before a redirect_uri is trusted).
func (s *AuthServer) authError(w http.ResponseWriter, code, desc string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	_ = errorPageTmpl.Execute(w, map[string]string{"Error": code, "Description": desc})
}

var consentTmpl = template.Must(template.New("consent").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Authorize</title></head><body>
<h1>Authorize access</h1>
<p>The client <strong>{{.ClientName}}</strong> is requesting access.</p>
{{if .Notice}}<p style="color:#b00"><strong>{{.Notice}}</strong></p>{{end}}
<form method="POST" action="">
<input type="hidden" name="client_id" value="{{.Req.ClientID}}">
<input type="hidden" name="redirect_uri" value="{{.Req.RedirectURI}}">
<input type="hidden" name="response_type" value="{{.Req.ResponseType}}">
<input type="hidden" name="code_challenge" value="{{.Req.CodeChallenge}}">
<input type="hidden" name="code_challenge_method" value="{{.Req.CodeChallengeMethod}}">
<input type="hidden" name="resource" value="{{.Req.Resource}}">
<input type="hidden" name="state" value="{{.Req.State}}">
<input type="hidden" name="scope" value="{{.Req.Scope}}">
<p><label>Consent credential: <input type="password" name="consent_credential" autocomplete="off"></label></p>
<button type="submit" name="approve" value="1">Approve</button>
<button type="submit" name="deny" value="1">Deny</button>
</form>
</body></html>`))

var errorPageTmpl = template.Must(template.New("err").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>Error</title></head><body>
<h1>Authorization error</h1>
<p><strong>{{.Error}}</strong></p>
<p>{{.Description}}</p>
</body></html>`))

func (s *AuthServer) renderConsent(w http.ResponseWriter, req authRequest, md *ClientMetadata, notice string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	name := md.ClientName
	if name == "" {
		name = md.ClientID
	}
	_ = consentTmpl.Execute(w, struct {
		Req        authRequest
		ClientName string
		Notice     string
	}{Req: req, ClientName: name, Notice: notice})
}

// --- /token -----------------------------------------------------------------

// tokenResponse is the RFC 6749 §5.1 successful token response.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope,omitempty"`
}

func (s *AuthServer) handleToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.tokenError(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	// RFC 6749 §4.1.3: the token endpoint is application/x-www-form-urlencoded.
	// Reject a JSON body explicitly (some frameworks default to JSON-only).
	ct := r.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		s.tokenError(w, http.StatusBadRequest, "invalid_request", "Content-Type must be application/x-www-form-urlencoded")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.tokenError(w, http.StatusBadRequest, "invalid_request", "could not parse form body")
		return
	}
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		s.grantAuthorizationCode(w, r)
	case "refresh_token":
		s.grantRefreshToken(w, r)
	default:
		s.tokenError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

func (s *AuthServer) grantAuthorizationCode(w http.ResponseWriter, r *http.Request) {
	code := r.PostFormValue("code")
	clientID := r.PostFormValue("client_id")
	redirectURI := r.PostFormValue("redirect_uri")
	verifier := r.PostFormValue("code_verifier")
	if code == "" || clientID == "" || verifier == "" {
		s.tokenError(w, http.StatusBadRequest, "invalid_request", "code, client_id and code_verifier are required")
		return
	}
	rec, err := s.store.TakeCode(code, s.now())
	if err != nil {
		// Unknown, already-used, or expired code — all invalid_grant.
		s.tokenError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid or expired")
		return
	}
	if rec.ClientID != clientID || rec.RedirectURI != redirectURI {
		s.tokenError(w, http.StatusBadRequest, "invalid_grant", "code was issued to a different client or redirect_uri")
		return
	}
	if !verifyPKCE(verifier, rec.CodeChallenge) {
		s.tokenError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	series, err := s.store.NewSeries(rec.ClientID, rec.Principal, rec.Resource, s.now(), s.accessTTL, s.refreshTTL)
	if err != nil {
		s.tokenError(w, http.StatusInternalServerError, "server_error", "could not issue tokens")
		return
	}
	s.writeTokens(w, series)
}

func (s *AuthServer) grantRefreshToken(w http.ResponseWriter, r *http.Request) {
	refresh := r.PostFormValue("refresh_token")
	if refresh == "" {
		s.tokenError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	series, err := s.store.Rotate(refresh, s.now(), s.accessTTL, s.refreshTTL)
	if err != nil {
		// Unknown, already-rotated, revoked, or expired — all invalid_grant.
		s.tokenError(w, http.StatusBadRequest, "invalid_grant", "refresh token is invalid or expired")
		return
	}
	s.writeTokens(w, series)
}

func (s *AuthServer) writeTokens(w http.ResponseWriter, series oauthstore.SeriesRecord) {
	expiresIn := int(s.accessTTL / time.Second)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	_ = json.NewEncoder(w).Encode(tokenResponse{
		AccessToken:  series.AccessToken,
		TokenType:    "Bearer",
		ExpiresIn:    expiresIn,
		RefreshToken: series.RefreshToken,
	})
}

func (s *AuthServer) tokenError(w http.ResponseWriter, status int, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error":             code,
		"error_description": desc,
	})
}

// verifyPKCE checks base64url(sha256(verifier)) == challenge in constant time
// (PKCE S256, RFC 7636).
func verifyPKCE(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(computed), []byte(challenge)) == 1
}

// clientIDDomain extracts the host of a client_id URL for operational logging,
// returning "" when client_id is empty or not a parseable https URL (in which
// case the reason category records the stage instead). The host is the external
// client's domain being diagnosed — in-scope per the directive — not Shoka's.
func clientIDDomain(clientID string) string {
	u, err := url.Parse(strings.TrimSpace(clientID))
	if err != nil || u.Scheme != "https" {
		return ""
	}
	return u.Hostname()
}

// cimdRejectCategory maps a CIMD verification error to a discrete, log-only
// reason category (distinct from the wire-facing cimdReason, which is unchanged).
// ErrUntrustedDomain is split by the configured-domain count so the default-deny
// empty-list case is distinguishable from a configured-but-no-match rejection.
func cimdRejectCategory(err error, trustedCount int) string {
	switch {
	case errors.Is(err, ErrClientIDNotHTTPS):
		return "client_id-missing-or-malformed"
	case errors.Is(err, ErrUntrustedDomain):
		if trustedCount == 0 {
			return "trusted-list-empty"
		}
		return "domain-not-in-trusted-list"
	case errors.Is(err, ErrBlockedAddress):
		return "blocked-address"
	case errors.Is(err, ErrRedirectAttempted):
		return "metadata-fetch-failed"
	case errors.Is(err, ErrFetchFailed):
		return "metadata-fetch-failed"
	case errors.Is(err, ErrDocumentTooLarge):
		return "metadata-invalid"
	case errors.Is(err, ErrInvalidDocument):
		return "metadata-invalid"
	case errors.Is(err, ErrClientIDMismatch):
		return "metadata-client_id-mismatch"
	case errors.Is(err, ErrNoRedirectURIs):
		return "metadata-no-redirect-uris"
	default:
		return "metadata-fetch-failed"
	}
}

// cimdReason maps a CIMD verification error to a short, non-leaking reason.
func cimdReason(err error) string {
	switch {
	case errors.Is(err, ErrClientIDNotHTTPS):
		return "client_id is not an https URL"
	case errors.Is(err, ErrUntrustedDomain):
		return "client domain is not trusted"
	case errors.Is(err, ErrBlockedAddress):
		return "client address is not permitted"
	case errors.Is(err, ErrClientIDMismatch):
		return "metadata client_id mismatch"
	case errors.Is(err, ErrNoRedirectURIs):
		return "metadata declares no redirect_uris"
	default:
		return "metadata could not be retrieved"
	}
}
