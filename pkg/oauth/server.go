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

	"github.com/sopranoworks/shoka/pkg/oauthstore"
	"github.com/sopranoworks/shoka/pkg/reqtrace"
	"github.com/sopranoworks/shoka/pkg/serverurl"
	"github.com/sopranoworks/shoka/pkg/tokenfp"
)

// This file builds Shoka's built-in OAuth 2.1 authorization-server core (the
// 2026-06-03 MCP OAuth (b) directive): the /authorize consent gate (+ PKCE S256)
// and the /token endpoint (form-urlencoded; authorization_code -> access +
// rotating refresh; RFC 8707 audience). CIMD (cimd.go) is the only client
// registration path; the token state lives in the go-git-free oauthstore.
//
// The principal stamped on a token is the CURRENT-MODE operator principal, carried as
// BoundPrincipal — NOT a baked-in single-user constant. Today that is the one configured
// operator; multi-user enablement (a later B-28 leg) replaces this with per-user
// authentication, additively. Consent itself is per-domain (B-71 Stage 2c/2e): the
// /authorize gate verifies the submitted credential against the connecting domain's own
// stored consent; there is no global consent credential (B-71 Stage 2e retired it).

// AuthServerConfig configures the AuthServer.
type AuthServerConfig struct {
	ExternalURL string // Server.MCP.OAuth.ExternalURL; empty falls back to forwarded headers
	// BoundPrincipal is the operator principal stamped on an issued token once a domain's
	// per-domain consent has verified the /authorize submission (B-71 Stage 2e). It carries no
	// credential — the consent secret lives only in the domain entry (hashed), never here.
	BoundPrincipal oauthstore.Principal
	AccessTTL      time.Duration
	RefreshTTL     time.Duration
	CodeTTL        time.Duration
	Logger         *slog.Logger // operational log for client-verification outcomes; nil → slog.Default()
}

// AuthServer serves /authorize and /token for the built-in AS.
type AuthServer struct {
	store          *oauthstore.Store
	verifier       *Verifier
	externalURL    string
	boundPrincipal oauthstore.Principal
	accessTTL      time.Duration
	refreshTTL     time.Duration
	codeTTL        time.Duration
	logger         *slog.Logger
	now            func() time.Time // injectable for tests
}

// NewAuthServer builds an AuthServer. TTLs default to sensible FINITE values when
// zero/negative — the no-indefinite floor (B-71 Stage 5): 0/unset is never
// "forever," it resolves to a finite default. Production wiring resolves these in
// config.applyDefaults (so the self-issued path is covered too); this clamp is the
// defense-in-depth fallback for any direct constructor (e.g. tests). The values
// match config's defaults: access 1h, refresh 90d (was an un-reviewed 30d; revisited
// against GitHub's ~6mo rotating refresh — Shoka rotates too), code 1m.
func NewAuthServer(store *oauthstore.Store, verifier *Verifier, cfg AuthServerConfig) *AuthServer {
	accessTTL := cfg.AccessTTL
	if accessTTL <= 0 {
		accessTTL = time.Hour
	}
	refreshTTL := cfg.RefreshTTL
	if refreshTTL <= 0 {
		refreshTTL = 90 * 24 * time.Hour
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
		store:          store,
		verifier:       verifier,
		externalURL:    cfg.ExternalURL,
		boundPrincipal: cfg.BoundPrincipal,
		accessTTL:      accessTTL,
		refreshTTL:     refreshTTL,
		codeTTL:        codeTTL,
		logger:         logger,
		now:            time.Now,
	}
}

// RegisterEndpoints mounts /authorize and /token on mux. These are reachable
// without a bearer token (they are how a token is obtained).
func (s *AuthServer) RegisterEndpoints(mux *http.ServeMux) {
	// reqtrace.Route tags the routing stage (B-53 §2.5): the response line names
	// route="oauth-authorize"/"oauth-token" under the shared request_id, alongside
	// the B-51/B-52 reason-rich lines (which now also carry the id from context).
	mux.Handle("/authorize", reqtrace.Route("oauth-authorize", http.HandlerFunc(s.handleAuthorize)))
	mux.Handle("/token", reqtrace.Route("oauth-token", http.HandlerFunc(s.handleToken)))
	// /register is the RFC 7591 Dynamic Client Registration endpoint (B-63),
	// required by claude.ai's connector docs. Like /authorize and /token it is
	// reachable without a bearer (registration is how a client becomes known).
	mux.Handle("/register", reqtrace.Route("oauth-register", http.HandlerFunc(s.handleRegister)))
}

// reqLogger returns s.logger with the request's correlation id (B-53) attached, so
// every authorize/token line for one request shares the id with its entry/auth/
// response lines. The id is "" when the request did not pass through reqtrace (e.g.
// a unit test exercising the handler directly).
func (s *AuthServer) reqLogger(r *http.Request) *slog.Logger {
	base := s.logger
	return base.With("request_id", reqtrace.ID(r.Context()))
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

// authorizeConsent runs the /authorize approval gate (B-71 Stage 2c/2e). Consent is PER-DOMAIN
// only: the submitted consent_credential is verified against the connecting client's domain's
// own stored consent (hashed, constant-time); on success the operator principal is returned.
// A client whose host has no "domain" entry, or a domain entry with NO per-domain consent set
// (Consent == nil), CANNOT be approved — it is denied. There is no global consent_credential
// fallback: B-71 Stage 2e retired that static key, so a domain inherits nothing; the operator
// sets per-domain consent via the web UI (never silently allow). After the 2c seed (which
// carried the former global consent into each seeded domain), the common path is a per-domain
// match — unchanged for a seeded deployment.
func (s *AuthServer) authorizeConsent(r *http.Request, clientID string) (oauthstore.Principal, bool) {
	// B-71 Stage 3 — a confidential pre-issued client is PRE-AUTHORIZED by issuance (the operator
	// created the credential), so consent is granted on the bare approve, with NO consent
	// credential: the real gate is the client SECRET verified at /token (a code minted here is
	// useless without the secret + the PKCE verifier). A non-existent or expired credential is not
	// approved here — it is rejected earlier in resolveAuthorizeClient.
	if conf, ok := s.store.ConfidentialClient(clientID); ok && !conf.CredentialExpired(s.now()) {
		return s.boundPrincipal, true
	}
	// Domain mode — per-domain consent (B-71 Stage 2c/2e), no global fallback.
	entry, ok := s.store.DomainEntryForClient(clientID)
	if !ok || entry.Consent == nil {
		return oauthstore.Principal{}, false
	}
	if entry.VerifyConsent(r.PostFormValue("consent_credential")) {
		return s.boundPrincipal, true
	}
	return oauthstore.Principal{}, false
}

// issuanceTTL returns the access/refresh TTL for a token issued to clientID (B-71 Stage 2c):
// the connecting domain's EffectiveTTL when its "domain" entry exists, else the global finite
// defaults (Stage 5). A self-issued/confidential client (no domain) uses the global defaults.
// Never indefinite — EffectiveTTL floors to the finite default.
func (s *AuthServer) issuanceTTL(clientID string) (access, refresh time.Duration) {
	if entry, ok := s.store.DomainEntryForClient(clientID); ok {
		return entry.EffectiveTTL(s.accessTTL, s.refreshTTL)
	}
	return s.accessTTL, s.refreshTTL
}

func (s *AuthServer) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	lg := s.reqLogger(r)
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

	// Make the whole authorize step legible (B-52): log the request as received,
	// with non-secret identifiers and presence bools only — never the
	// code_challenge value (PKCE challenge is logged as present/absent + method).
	lg.Info("oauth authorize request",
		"http_method", r.Method,
		"response_type", req.ResponseType,
		"client_id", req.ClientID,
		"redirect_uri", req.RedirectURI,
		"scope", req.Scope,
		"state_present", req.State != "",
		"code_challenge_present", req.CodeChallenge != "",
		"code_challenge_method", req.CodeChallengeMethod,
	)

	// Resolve the client and the redirect_uri it is allowed to use BEFORE we are
	// willing to redirect anywhere — an unverified redirect_uri must never receive a
	// redirect (open-redirect / error-leak protection). Two registration paths
	// coexist (B-63): a DCR client_id (an opaque handle) resolves via the persisted
	// registration; a CIMD client_id (an https URL) is verified by fetching its
	// metadata document. The discriminator is isDCRClientID; the CIMD branch is
	// byte-identical to its pre-B-63 form.
	md, ok := s.resolveAuthorizeClient(w, r, lg, req.ClientID)
	if !ok {
		return
	}
	// A confidential client (B-71 Stage 3) has no registered redirect_uris (a pre-issued credential
	// has no fixed redirect); it accepts any https redirect_uri, which is bound to the code and
	// re-checked at /token — the client SECRET is the load-bearing gate. Every other client binds
	// to its registered/verified redirect_uris.
	redirectOK := md.Confidential && isHTTPSURL(req.RedirectURI)
	if !redirectOK && !RedirectURIAllowed(req.RedirectURI, md.RedirectURIs) {
		lg.Warn("oauth authorize rejected",
			"client_id", req.ClientID, "redirect_uri", req.RedirectURI,
			"reason", "redirect-uri-not-registered")
		s.authError(w, "invalid_request", "redirect_uri is not registered for this client")
		return
	}

	// From here a redirect_uri is trusted, so protocol errors redirect back per
	// OAuth 2.1 (RFC 6749 §4.1.2.1) rather than rendering on-page.
	if req.ResponseType != "code" {
		lg.Warn("oauth authorize rejected",
			"client_id", req.ClientID, "reason", "unsupported-response-type")
		s.redirectError(w, r, req, "unsupported_response_type", "only response_type=code is supported")
		return
	}
	if req.CodeChallenge == "" || req.CodeChallengeMethod != "S256" {
		lg.Warn("oauth authorize rejected",
			"client_id", req.ClientID, "reason", "pkce-s256-required")
		s.redirectError(w, r, req, "invalid_request", "PKCE with code_challenge_method=S256 is required")
		return
	}
	resource, ok := s.resolveResource(r, req.Resource)
	if !ok {
		lg.Warn("oauth authorize rejected",
			"client_id", req.ClientID, "reason", "resource-mismatch")
		s.redirectError(w, r, req, "invalid_target", "resource does not identify this server")
		return
	}
	req.Resource = resource

	if r.Method == http.MethodGet {
		lg.Info("oauth authorize consent rendered", "client_id", req.ClientID)
		s.renderConsent(w, req, md, "")
		return
	}

	// POST: an approval or denial.
	switch {
	case r.PostFormValue("deny") != "":
		lg.Info("oauth authorize denied", "client_id", req.ClientID, "reason", "user-denied")
		s.redirectError(w, r, req, "access_denied", "the operator denied the request")
		return
	case r.PostFormValue("approve") != "":
		principal, authed := s.authorizeConsent(r, req.ClientID)
		if !authed {
			// Re-render the consent page with an error; do NOT redirect (the
			// credential was wrong — treat like an unauthenticated retry). Logged as
			// a discrete category so a wrong/empty consent credential is visible
			// (the credential VALUE is never logged).
			lg.Warn("oauth authorize consent rejected",
				"client_id", req.ClientID, "reason", "consent-credential-mismatch")
			w.WriteHeader(http.StatusUnauthorized)
			s.renderConsent(w, req, md, "Incorrect consent credential.")
			return
		}
		code, herr := oauthstore.NewHandle()
		if herr != nil {
			lg.Error("oauth authorize code issuance failed",
				"client_id", req.ClientID, "reason", "handle-generation-failed")
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
			lg.Error("oauth authorize code issuance failed",
				"client_id", req.ClientID, "reason", "code-persist-failed")
			s.redirectError(w, r, req, "server_error", "could not persist code")
			return
		}
		// Code minted: log the issuance (for client_id + redirect target) — the
		// authorization-code VALUE is NEVER logged.
		lg.Info("oauth authorize code issued",
			"client_id", req.ClientID, "redirect_uri", req.RedirectURI)
		s.redirectSuccess(w, r, req, code)
		return
	default:
		lg.Info("oauth authorize consent re-rendered",
			"client_id", req.ClientID, "reason", "no-approve-or-deny")
		s.renderConsent(w, req, md, "Choose approve or deny.")
	}
}

// resolveAuthorizeClient resolves the authorize-request client_id to a
// ClientMetadata (for the redirect_uri binding + consent rendering), handling both
// registration paths (B-63). On failure it writes the on-page invalid_client error
// itself and returns ok=false (no redirect — the redirect_uri is not yet trusted).
//
//   - DCR (the client_id is an opaque handle): resolve the persisted registration;
//     an unknown id is invalid_client.
//   - CIMD (the client_id is an https URL): fetch + verify the metadata document —
//     unchanged from the pre-B-63 behaviour, including the self-diagnosing logging.
func (s *AuthServer) resolveAuthorizeClient(w http.ResponseWriter, r *http.Request, lg *slog.Logger, clientID string) (*ClientMetadata, bool) {
	// B-71 Stage 3 — a confidential client_id is opaque (like a DCR handle), so resolve it BEFORE
	// the DCR path. An unknown or expired confidential credential is invalid_client. A confidential
	// client accepts any https redirect_uri (no fixed redirect; the secret at /token is the gate).
	if conf, ok := s.store.ConfidentialClient(clientID); ok {
		if conf.CredentialExpired(s.now()) {
			lg.Warn("oauth authorize rejected",
				"client_id", clientID, "registration", "confidential", "reason", "confidential-credential-expired")
			s.authError(w, "invalid_client", "client credential has expired")
			return nil, false
		}
		lg.Info("oauth client resolved", "client_id", clientID, "registration", "confidential")
		return &ClientMetadata{
			ClientID:                clientID,
			ClientName:              "confidential client",
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			TokenEndpointAuthMethod: "client_secret_post",
			Confidential:            true,
		}, true
	}
	if isDCRClientID(clientID) {
		rc, err := s.store.GetClient(clientID)
		if err != nil {
			lg.Warn("oauth authorize rejected",
				"client_id", clientID, "registration", "dcr", "reason", "dcr-client-unknown")
			s.authError(w, "invalid_client", "unknown client_id (not registered)")
			return nil, false
		}
		lg.Info("oauth client resolved",
			"client_id", clientID, "registration", "dcr", "client_name", rc.ClientName)
		return &ClientMetadata{
			ClientID:                rc.ClientID,
			ClientName:              rc.ClientName,
			RedirectURIs:            rc.RedirectURIs,
			GrantTypes:              rc.GrantTypes,
			ResponseTypes:           rc.ResponseTypes,
			TokenEndpointAuthMethod: rc.TokenEndpointAuthMethod,
		}, true
	}

	// CIMD path (unchanged): verify the client by fetching its metadata URL.
	md, err := s.verifier.Verify(r.Context(), clientID)
	if err != nil {
		// Make the rejection self-diagnosing: an operator reading the log learns
		// the received client_id, the domain evaluated (or the stage reached),
		// why it was denied, and how many trusted domains are configured — so the
		// empty-list and wrong-domain cases reveal what to add. The wire response
		// is unchanged (the caller still gets invalid_client); only the diagnostic
		// payload goes to the operator's log. Secrets and the trusted-domain
		// VALUES are never logged (only the count).
		trustedCount := s.verifier.TrustedCount()
		lg.Warn("oauth client verification rejected",
			"client_id", clientID,
			"evaluated_domain", clientIDDomain(clientID),
			"reason", cimdRejectCategory(err, trustedCount),
			"trusted_domains_configured", trustedCount,
		)
		s.authError(w, "invalid_client", "client verification failed: "+cimdReason(err))
		return nil, false
	}
	// Success is equally observable: confirm which client_id/domain was accepted.
	lg.Info("oauth client verification accepted",
		"client_id", clientID,
		"evaluated_domain", clientIDDomain(clientID),
	)
	return md, true
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
	lg := s.reqLogger(r)
	if r.Method != http.MethodPost {
		lg.Warn("oauth token rejected", "error", "invalid_request", "reason", "method-not-post")
		s.tokenError(w, http.StatusMethodNotAllowed, "invalid_request", "POST required")
		return
	}
	// RFC 6749 §4.1.3: the token endpoint is application/x-www-form-urlencoded.
	// Reject a JSON body explicitly (some frameworks default to JSON-only).
	ct := r.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		lg.Warn("oauth token rejected", "error", "invalid_request", "reason", "content-type-not-form-urlencoded")
		s.tokenError(w, http.StatusBadRequest, "invalid_request", "Content-Type must be application/x-www-form-urlencoded")
		return
	}
	if err := r.ParseForm(); err != nil {
		lg.Warn("oauth token rejected", "error", "invalid_request", "reason", "form-parse-failed")
		s.tokenError(w, http.StatusBadRequest, "invalid_request", "could not parse form body")
		return
	}
	// The currently-invisible step made legible (B-52): log the request as received
	// — grant_type + non-secret identifiers + presence bools, never the code,
	// refresh token, or code_verifier VALUES.
	lg.Info("oauth token request",
		"grant_type", r.PostFormValue("grant_type"),
		"client_id", r.PostFormValue("client_id"),
		"redirect_uri", r.PostFormValue("redirect_uri"),
		"code_present", r.PostFormValue("code") != "",
		"refresh_token_present", r.PostFormValue("refresh_token") != "",
		"code_verifier_present", r.PostFormValue("code_verifier") != "",
	)
	switch r.PostFormValue("grant_type") {
	case "authorization_code":
		s.grantAuthorizationCode(w, r)
	case "refresh_token":
		s.grantRefreshToken(w, r)
	default:
		lg.Warn("oauth token rejected", "error", "unsupported_grant_type", "reason", "grant-type-unsupported")
		s.tokenError(w, http.StatusBadRequest, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
	}
}

func (s *AuthServer) grantAuthorizationCode(w http.ResponseWriter, r *http.Request) {
	lg := s.reqLogger(r)
	code := r.PostFormValue("code")
	clientID := r.PostFormValue("client_id")
	redirectURI := r.PostFormValue("redirect_uri")
	verifier := r.PostFormValue("code_verifier")
	// B-71 Stage 3 — a confidential client may present its credentials via HTTP Basic
	// (client_secret_basic): the username is the client_id, the password the secret. Accept the
	// client_id from Basic when the body omits it, so a Basic-only confidential client resolves.
	basicID, basicSecret, hasBasic := r.BasicAuth()
	if clientID == "" && hasBasic {
		clientID = basicID
	}
	if code == "" || clientID == "" || verifier == "" {
		// Distinguish a missing PKCE verifier (the §2.2 "missing" outcome) from the
		// other missing params, without logging any value.
		reason := "code-or-client_id-missing"
		if verifier == "" && code != "" && clientID != "" {
			reason = "code_verifier-missing"
		}
		lg.Warn("oauth token rejected",
			"grant_type", "authorization_code", "error", "invalid_request", "reason", reason)
		s.tokenError(w, http.StatusBadRequest, "invalid_request", "code, client_id and code_verifier are required")
		return
	}
	// Deleted-client signal (B-63): if the presented client_id is a DCR handle whose
	// registration is gone (store reset, or the client was removed), reply 401
	// invalid_client so claude.ai re-registers (per the help-center article +
	// RFC 6749). The CIMD path (client_id is an https URL) is untouched — it is not
	// store-backed, so it never hits this check. The code is NOT consumed here: an
	// unknown client should re-register, not burn the code on an error.
	// B-71 Stage 3 — resolve a confidential client up front (its client_id is opaque, like a DCR
	// handle). The DCR-unknown check below is skipped for a confidential client (it is not in the
	// DCR clients bucket); its secret + expiry are enforced after PKCE, below.
	confEntry, isConfidential := s.store.ConfidentialClient(clientID)
	if !isConfidential && isDCRClientID(clientID) {
		if _, gerr := s.store.GetClient(clientID); gerr != nil {
			lg.Warn("oauth token rejected",
				"grant_type", "authorization_code", "client_id", clientID,
				"error", "invalid_client", "reason", "dcr-client-unknown")
			s.tokenError(w, http.StatusUnauthorized, "invalid_client", "client is not registered")
			return
		}
	}
	rec, err := s.store.TakeCode(code, s.now())
	if err != nil {
		// Unknown, already-used, or expired code — all invalid_grant on the wire,
		// but logged with a discrete category (expired vs unknown/used; the store
		// collapses unknown and already-used into ErrNotFound).
		reason := "code-unknown-or-used"
		if errors.Is(err, oauthstore.ErrExpired) {
			reason = "code-expired"
		}
		lg.Warn("oauth token rejected",
			"grant_type", "authorization_code", "client_id", clientID,
			"error", "invalid_grant", "reason", reason)
		s.tokenError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid or expired")
		return
	}
	if rec.ClientID != clientID || rec.RedirectURI != redirectURI {
		// Split the combined check for the log only (wire response unchanged).
		reason := "code-client-mismatch"
		if rec.ClientID == clientID {
			reason = "code-redirect-uri-mismatch"
		}
		lg.Warn("oauth token rejected",
			"grant_type", "authorization_code", "client_id", clientID,
			"error", "invalid_grant", "reason", reason)
		s.tokenError(w, http.StatusBadRequest, "invalid_grant", "code was issued to a different client or redirect_uri")
		return
	}
	if !verifyPKCE(verifier, rec.CodeChallenge) {
		// PKCE mismatch — the verifier/challenge VALUES are never logged, only the
		// match outcome.
		lg.Warn("oauth token rejected",
			"grant_type", "authorization_code", "client_id", rec.ClientID,
			"error", "invalid_grant", "reason", "pkce-mismatch")
		s.tokenError(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
		return
	}
	// B-71 Stage 3 — confidential client authentication, REQUIRED IN ADDITION to the PKCE check
	// above (defence in depth; the operator's higher-security choice). The presented secret comes
	// from HTTP Basic (client_secret_basic) or the client_secret body param (client_secret_post);
	// it is hash-compared constant-time to the stored hash. A missing/wrong secret, or an expired
	// credential, is invalid_client (401). The token then carries the entry's pre-issued SCOPE
	// (which the tools/call namespace gate enforces). The public/PKCE-only path is unchanged.
	scope := "*"
	if domEntry, ok := s.store.DomainEntryForClient(rec.ClientID); ok && domEntry.Scope != "" {
		scope = domEntry.Scope
	}
	if isConfidential {
		if confEntry.CredentialExpired(s.now()) {
			lg.Warn("oauth token rejected",
				"grant_type", "authorization_code", "client_id", clientID,
				"error", "invalid_client", "reason", "confidential-credential-expired")
			s.tokenError(w, http.StatusUnauthorized, "invalid_client", "client credential has expired")
			return
		}
		presentedSecret := r.PostFormValue("client_secret")
		if hasBasic {
			presentedSecret = basicSecret // client_secret_basic takes precedence
		}
		if !confEntry.VerifySecret(presentedSecret) {
			reason := "confidential-secret-mismatch"
			if presentedSecret == "" {
				reason = "confidential-secret-missing"
			}
			lg.Warn("oauth token rejected",
				"grant_type", "authorization_code", "client_id", clientID,
				"error", "invalid_client", "reason", reason)
			s.tokenError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
			return
		}
		scope = confEntry.Scope
	}
	// A domain-mode token carries its domain entry's configured scope (default "*"); a confidential
	// token carries its pre-issued scope. The tools/call authz gate reads this scope.
	access, refresh := s.issuanceTTL(rec.ClientID) // per-domain TTL else the global finite default
	series, err := s.store.NewSeries(rec.ClientID, rec.Principal, rec.Resource, scope, "", s.now(), access, refresh)
	if err != nil {
		lg.Error("oauth token issuance failed",
			"grant_type", "authorization_code", "client_id", rec.ClientID, "reason", "series-create-failed")
		s.tokenError(w, http.StatusInternalServerError, "server_error", "could not issue tokens")
		return
	}
	// Issued: log for client_id with the TTLs — the access/refresh token VALUES are
	// NEVER logged. PKCE matched (implied by reaching issuance).
	lg.Info("oauth token issued",
		"grant_type", "authorization_code", "client_id", rec.ClientID,
		"pkce_result", "match",
		// B-54 discriminator: a one-way fingerprint of the issued access token (never
		// the value). The same fingerprint on a later "auth rejected" line proves the
		// SAME token reached Lookup (→ store reset/split); a different/absent one proves
		// a different value arrived (→ proxy/stale token).
		"token_fingerprint", tokenfp.Fingerprint(series.AccessToken),
		"access_ttl_seconds", int(access/time.Second),
		"refresh_ttl_seconds", int(refresh/time.Second))
	s.writeTokens(w, series)
}

func (s *AuthServer) grantRefreshToken(w http.ResponseWriter, r *http.Request) {
	lg := s.reqLogger(r)
	refresh := r.PostFormValue("refresh_token")
	if refresh == "" {
		lg.Warn("oauth token rejected",
			"grant_type", "refresh_token", "error", "invalid_request", "reason", "refresh_token-missing")
		s.tokenError(w, http.StatusBadRequest, "invalid_request", "refresh_token is required")
		return
	}
	// Per-domain TTL on rotation too: resolve the series' client (without rotating) to find
	// its domain's EffectiveTTL, else the global finite defaults (B-71 Stage 2c).
	rotateClientID, _ := s.store.RefreshClientID(refresh)
	// B-71 Stage 3 — a confidential client re-authenticates on EVERY /token request, refresh
	// included: verify its secret (Basic or body) and that the credential has not expired before
	// rotating. A missing/wrong secret, or an expired credential, is invalid_client (401).
	if confEntry, ok := s.store.ConfidentialClient(rotateClientID); ok {
		if confEntry.CredentialExpired(s.now()) {
			lg.Warn("oauth token rejected",
				"grant_type", "refresh_token", "client_id", rotateClientID,
				"error", "invalid_client", "reason", "confidential-credential-expired")
			s.tokenError(w, http.StatusUnauthorized, "invalid_client", "client credential has expired")
			return
		}
		presentedSecret := r.PostFormValue("client_secret")
		if _, basicSecret, hasBasic := r.BasicAuth(); hasBasic {
			presentedSecret = basicSecret
		}
		if !confEntry.VerifySecret(presentedSecret) {
			lg.Warn("oauth token rejected",
				"grant_type", "refresh_token", "client_id", rotateClientID,
				"error", "invalid_client", "reason", "confidential-secret-mismatch")
			s.tokenError(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
			return
		}
	}
	accessTTL, refreshTTL := s.issuanceTTL(rotateClientID)
	series, err := s.store.Rotate(refresh, s.now(), accessTTL, refreshTTL)
	if err != nil {
		// Unknown, already-rotated, revoked, or expired — all invalid_grant on the
		// wire; logged with a discrete category (expired vs unknown/rotated).
		reason := "refresh-unknown-or-rotated"
		if errors.Is(err, oauthstore.ErrExpired) {
			reason = "refresh-expired"
		}
		lg.Warn("oauth token rejected",
			"grant_type", "refresh_token", "error", "invalid_grant", "reason", reason)
		s.tokenError(w, http.StatusBadRequest, "invalid_grant", "refresh token is invalid or expired")
		return
	}
	// Rotated: a fresh access+refresh pair issued in the same series (token VALUES
	// never logged).
	lg.Info("oauth token issued",
		"grant_type", "refresh_token", "client_id", series.ClientID,
		"token_fingerprint", tokenfp.Fingerprint(series.AccessToken), // B-54 discriminator (one-way; never the value)
		"access_ttl_seconds", int(accessTTL/time.Second),
		"refresh_ttl_seconds", int(refreshTTL/time.Second))
	s.writeTokens(w, series)
}

func (s *AuthServer) writeTokens(w http.ResponseWriter, series oauthstore.SeriesRecord) {
	// expires_in reflects the ACTUAL issued access lifetime (the per-domain EffectiveTTL,
	// B-71 Stage 2c), derived from the series' own AccessExpiry rather than the global default.
	expiresIn := int(series.AccessExpiry.Sub(s.now()) / time.Second)
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
