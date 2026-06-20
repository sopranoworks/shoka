package oauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/sopranoworks/shoka/internal/storage/oauthstore"
)

// register POSTs a raw client-metadata body to /register and returns the recorder.
func (h testAS) register(t *testing.T, body, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	h.as.handleRegister(rec, req)
	return rec
}

// trustDomain seeds a trusted "domain" RegistrationEntry (B-71 Stage 2c) so a DCR client whose
// redirect_uris host matches can register — mirroring how production seeds the dynamic store
// from trusted_client_metadata_domains at startup.
func (h testAS) trustDomain(t *testing.T, domain string) {
	t.Helper()
	if _, err := h.store.CreateRegistration(oauthstore.RegistrationModeDomain, domain, h.as.now()); err != nil {
		t.Fatalf("trustDomain(%q): %v", domain, err)
	}
}

// trustDomainWithConsent makes domain a trusted "domain" entry that can actually grant
// /authorize consent: it creates the entry (if absent) and sets its per-domain consent to
// testCredential. B-71 Stage 2e retired the global consent_credential fallback, so an
// approve-path test must give the connecting domain its OWN consent — mirroring the operator
// setting it in the web UI. Idempotent (re-setting consent on an existing entry is fine).
func (h testAS) trustDomainWithConsent(t *testing.T, domain string) {
	t.Helper()
	entry, ok := h.store.DomainEntryForHost(domain)
	if !ok {
		var err error
		entry, err = h.store.CreateRegistration(oauthstore.RegistrationModeDomain, domain, h.as.now())
		if err != nil {
			t.Fatalf("trustDomainWithConsent(%q): create: %v", domain, err)
		}
	}
	entry.SetConsent(testCredential)
	if err := h.store.UpdateRegistration(entry); err != nil {
		t.Fatalf("trustDomainWithConsent(%q): set consent: %v", domain, err)
	}
}

// registerClient performs a successful DCR registration and returns the issued client_id (an
// opaque handle). It first trusts the redirect host so the Stage 2c DCR gate admits the
// registration (as production does via the startup seed).
func (h testAS) registerClient(t *testing.T, redirectURIs []string) string {
	t.Helper()
	if d := oauthstore.DomainFromRedirectURIs(redirectURIs); d != "" {
		h.trustDomainWithConsent(t, d)
	}
	body, _ := json.Marshal(registrationRequest{
		RedirectURIs:            redirectURIs,
		TokenEndpointAuthMethod: "none",
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		ClientName:              "DCR Client",
	})
	rec := h.register(t, string(body), "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("registerClient: want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	var resp registrationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("registerClient: decode: %v", err)
	}
	return resp.ClientID
}

// /register issues an opaque, persistent, PUBLIC (no secret) client_id per RFC 7591.
func TestRegister_IssuesPersistentPublicClient(t *testing.T) {
	h := newTestAS(t)
	h.trustDomain(t, "app.example") // Stage 2c: the redirect host must be a trusted domain
	body := `{"redirect_uris":["https://app.example/cb"],` +
		`"token_endpoint_auth_method":"none",` +
		`"grant_types":["authorization_code","refresh_token"],` +
		`"response_types":["code"],"client_name":"Claude"}`
	rec := h.register(t, body, "application/json")
	if rec.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("want application/json, got %q", ct)
	}
	// No secret may appear anywhere — public client only.
	if strings.Contains(rec.Body.String(), "client_secret") {
		t.Fatalf("public-client registration must not emit a client_secret: %s", rec.Body.String())
	}
	var resp registrationResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ClientID == "" || strings.HasPrefix(resp.ClientID, "http") {
		t.Fatalf("client_id must be an opaque handle, not a URL: %q", resp.ClientID)
	}
	if !isDCRClientID(resp.ClientID) {
		t.Fatalf("issued client_id must be recognised as a DCR handle: %q", resp.ClientID)
	}
	if resp.TokenEndpointAuthMethod != "none" {
		t.Fatalf("token_endpoint_auth_method must be none, got %q", resp.TokenEndpointAuthMethod)
	}
	if resp.ClientIDIssuedAt == 0 {
		t.Fatalf("client_id_issued_at must be set")
	}
	// Persisted + resolvable on a later request (the B-54 lesson / directive constraint).
	got, err := h.store.GetClient(resp.ClientID)
	if err != nil {
		t.Fatalf("registered client not persisted/resolvable: %v", err)
	}
	if len(got.RedirectURIs) != 1 || got.RedirectURIs[0] != "https://app.example/cb" {
		t.Fatalf("persisted redirect_uris wrong: %+v", got.RedirectURIs)
	}
}

// Bad client metadata is rejected with the RFC 7591 error codes.
func TestRegister_RejectsBadMetadata(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		ct        string
		wantCode  int
		wantError string
	}{
		{"no redirect_uris", `{"token_endpoint_auth_method":"none"}`, "application/json", http.StatusBadRequest, "invalid_redirect_uri"},
		{"malformed redirect_uri", `{"redirect_uris":["not a uri"]}`, "application/json", http.StatusBadRequest, "invalid_redirect_uri"},
		{"relative redirect_uri", `{"redirect_uris":["/callback"]}`, "application/json", http.StatusBadRequest, "invalid_redirect_uri"},
		{"confidential auth method", `{"redirect_uris":["https://a.example/cb"],"token_endpoint_auth_method":"client_secret_basic"}`, "application/json", http.StatusBadRequest, "invalid_client_metadata"},
		{"unsupported grant", `{"redirect_uris":["https://a.example/cb"],"grant_types":["client_credentials"]}`, "application/json", http.StatusBadRequest, "invalid_client_metadata"},
		{"unsupported response_type", `{"redirect_uris":["https://a.example/cb"],"response_types":["token"]}`, "application/json", http.StatusBadRequest, "invalid_client_metadata"},
		{"not json", `redirect_uris=x`, "application/json", http.StatusBadRequest, "invalid_client_metadata"},
		{"wrong content-type", `{"redirect_uris":["https://a.example/cb"]}`, "text/plain", http.StatusBadRequest, "invalid_client_metadata"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newTestAS(t)
			rec := h.register(t, c.body, c.ct)
			if rec.Code != c.wantCode {
				t.Fatalf("want %d, got %d (%s)", c.wantCode, rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), c.wantError) {
				t.Fatalf("want error %q, got %s", c.wantError, rec.Body.String())
			}
		})
	}
}

func TestRegister_RejectsNonPOST(t *testing.T) {
	h := newTestAS(t)
	rec := httptest.NewRecorder()
	h.as.handleRegister(rec, httptest.NewRequest(http.MethodGet, "/register", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("want 405, got %d", rec.Code)
	}
}

// A DCR-registered client completes authorize + token end-to-end — the second
// registration path alongside CIMD (TestFullFlow proves the CIMD path).
func TestDCR_FullFlow_AuthorizeAndToken(t *testing.T) {
	h := newTestAS(t)
	clientID := h.registerClient(t, []string{testRedirectURI})
	verifier, challenge := pkcePair()

	form := baseAuthForm(clientID, challenge)
	form.Set("approve", "1")
	form.Set("consent_credential", testCredential)
	rec := h.authorize(t, http.MethodPost, form)
	if rec.Code != http.StatusFound {
		t.Fatalf("authorize: want 302, got %d (%s)", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in redirect: %s", loc)
	}

	rec = h.token(t, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {verifier},
	}, "application/x-www-form-urlencoded")
	if rec.Code != http.StatusOK {
		t.Fatalf("token: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var tr tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &tr); err != nil {
		t.Fatalf("decode token resp: %v", err)
	}
	if tr.AccessToken == "" || tr.RefreshToken == "" || tr.TokenType != "Bearer" {
		t.Fatalf("bad token response: %+v", tr)
	}
	series, err := h.store.Lookup(tr.AccessToken, h.as.now())
	if err != nil {
		t.Fatalf("lookup access token: %v", err)
	}
	if series.ClientID != clientID || series.Principal.Name != "Operator" {
		t.Fatalf("binding wrong: %+v", series)
	}
}

// The DCR redirect_uri binding holds: a redirect_uri not in the registered set is
// rejected on-page (no redirect to an unregistered target).
func TestDCR_RedirectURINotRegistered(t *testing.T) {
	h := newTestAS(t)
	clientID := h.registerClient(t, []string{testRedirectURI})
	_, challenge := pkcePair()
	form := baseAuthForm(clientID, challenge)
	form.Set("redirect_uri", "https://attacker.example/steal")
	rec := h.authorize(t, http.MethodGet, form)
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
		t.Fatalf("unregistered redirect_uri must be on-page 400, got %d loc=%s", rec.Code, rec.Header().Get("Location"))
	}
}

// An unknown DCR client_id at /authorize is invalid_client, on-page (no redirect).
func TestDCR_UnknownClientAtAuthorize(t *testing.T) {
	h := newTestAS(t)
	_, challenge := pkcePair()
	form := baseAuthForm("unregistered-opaque-handle-xyz", challenge)
	rec := h.authorize(t, http.MethodGet, form)
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
		t.Fatalf("unknown DCR client: want 400 on-page, got %d loc=%s", rec.Code, rec.Header().Get("Location"))
	}
	if !strings.Contains(rec.Body.String(), "invalid_client") {
		t.Fatalf("want invalid_client, got %s", rec.Body.String())
	}
}

// The deleted-client signal: an unknown DCR client_id at /token returns HTTP 401
// invalid_client (per the help-center article + RFC 6749) so claude.ai
// re-registers. The CIMD path (an https client_id) is never store-checked here.
func TestToken_UnknownDCRClientReturns401InvalidClient(t *testing.T) {
	h := newTestAS(t)
	rec := h.token(t, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"any-code"},
		"client_id":     {"unregistered-opaque-handle-xyz"},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {"any-verifier"},
	}, "application/x-www-form-urlencoded")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown DCR client at /token: want 401, got %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "invalid_client") {
		t.Fatalf("want invalid_client, got %s", rec.Body.String())
	}
}

// TestRegister_RecordsDomainFromRedirectURIs (B-71 Stage 2a): a DCR registration records the
// client's trusted domain (the redirect_uris host) on the RegisteredClient, so its series can
// later be grouped under a domain like a CIMD one. Record-only — no gate is added.
func TestRegister_RecordsDomainFromRedirectURIs(t *testing.T) {
	h := newTestAS(t)
	clientID := h.registerClient(t, []string{"https://connector.example/cb"})
	client, err := h.store.GetClient(clientID)
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	if client.Domain != "connector.example" {
		t.Fatalf("DCR /register must record the redirect_uri host as the domain; got %q", client.Domain)
	}
}
