package oauth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sopranoworks/shoka/internal/storage/oauthstore"
	"github.com/sopranoworks/shoka/internal/tokenfp"
)

// B-52: the WHOLE OAuth flow must be legible — /authorize (request, consent, code
// issuance) and /token (request, code/PKCE validation, issuance) — with discrete
// reason categories on every failure branch, and NEVER a secret in any log line.

// loggedFlowAS builds an AuthServer with a working TLS CIMD verifier AND a
// buffer-backed logger, so a test can drive the full authorize→token flow and
// assert the structured log lines.
func loggedFlowAS(t *testing.T) (testAS, *bytes.Buffer) {
	t.Helper()
	cimd := newTLSCIMD(t)
	store, err := oauthstore.Open(filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logger, buf := bufLogger()
	as := NewAuthServer(store, cimd.verifier, AuthServerConfig{
		ExternalURL:    "https://rs.example",
		BoundPrincipal: oauthstore.Principal{Name: "Operator", Email: "op@example.test"},
		Logger:         logger,
	})
	h := testAS{as: as, store: store, clientID: cimd.clientID, host: cimd.host, verifier: cimd.verifier}
	// B-71 Stage 2e: a successful connect needs the connecting domain's own per-domain consent
	// (the global consent_credential fallback is retired).
	h.trustDomainWithConsent(t, cimd.host)
	return h, buf
}

// A full successful connect is legible end to end: /authorize (request → consent
// approved → code issued) → /token (request → PKCE matched → tokens issued). And
// no secret — code, verifier, challenge value, consent credential, or the issued
// access/refresh tokens — appears anywhere in the log.
func TestFlowLog_SuccessfulFlowLegible(t *testing.T) {
	h, buf := loggedFlowAS(t)
	verifier, challenge := pkcePair()

	form := baseAuthForm(h.clientID, challenge)
	form.Set("approve", "1")
	form.Set("consent_credential", testCredential)
	rec := h.authorize(t, http.MethodPost, form)
	if rec.Code != http.StatusFound {
		t.Fatalf("authorize: want 302, got %d (%s)", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code issued: %s", loc)
	}

	tf := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {h.clientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {verifier},
	}
	trec := h.token(t, tf, "application/x-www-form-urlencoded")
	if trec.Code != http.StatusOK {
		t.Fatalf("token: want 200, got %d (%s)", trec.Code, trec.Body.String())
	}

	// Every stage logged, with the in-scope diagnostic payload.
	authReq := logLineWithMsg(t, buf, "oauth authorize request")
	if authReq["client_id"] != h.clientID {
		t.Errorf("authorize request client_id: got %v", authReq["client_id"])
	}
	if authReq["code_challenge_present"] != true {
		t.Errorf("authorize request code_challenge_present: got %v", authReq["code_challenge_present"])
	}
	if authReq["state_present"] != true {
		t.Errorf("authorize request state_present: got %v", authReq["state_present"])
	}
	issuedCode := logLineWithMsg(t, buf, "oauth authorize code issued")
	if issuedCode["client_id"] != h.clientID {
		t.Errorf("code issued client_id: got %v", issuedCode["client_id"])
	}
	tokReq := logLineWithMsg(t, buf, "oauth token request")
	if tokReq["grant_type"] != "authorization_code" {
		t.Errorf("token request grant_type: got %v", tokReq["grant_type"])
	}
	if tokReq["code_present"] != true || tokReq["code_verifier_present"] != true {
		t.Errorf("token request presence bools: got code=%v verifier=%v", tokReq["code_present"], tokReq["code_verifier_present"])
	}
	issued := logLineWithMsg(t, buf, "oauth token issued")
	if issued["pkce_result"] != "match" {
		t.Errorf("token issued pkce_result: got %v", issued["pkce_result"])
	}
	if issued["client_id"] != h.clientID {
		t.Errorf("token issued client_id: got %v", issued["client_id"])
	}
	if issued["access_ttl_seconds"] == nil {
		t.Errorf("token issued must carry access_ttl_seconds")
	}

	// No secret leaks anywhere in the log.
	var tokenResp struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(trec.Body.Bytes(), &tokenResp); err != nil {
		t.Fatalf("decode token resp: %v", err)
	}

	// B-54 discriminator: the issuance line carries an 8-hex one-way fingerprint of
	// the issued access token under the SAME field name (token_fingerprint) used at
	// the auth-reject site, so the operator greps one field across both lines and
	// reads match/differ at a glance. Assert it equals the issued token's fingerprint
	// (so equal input → equal fingerprint holds at issuance too).
	wantFP := tokenfp.Fingerprint(tokenResp.AccessToken)
	if len(wantFP) != 8 {
		t.Fatalf("issued access token fingerprint must be 8 hex chars, got %q", wantFP)
	}
	if issued["token_fingerprint"] != wantFP {
		t.Errorf("token issued token_fingerprint: got %v want %s", issued["token_fingerprint"], wantFP)
	}

	secrets := map[string]string{
		"authorization code": code,
		"code_verifier":      verifier,
		"code_challenge":     challenge,
		"consent credential": testCredential,
		"access token":       tokenResp.AccessToken,
		"refresh token":      tokenResp.RefreshToken,
	}
	for name, secret := range secrets {
		if secret != "" && strings.Contains(buf.String(), secret) {
			t.Errorf("%s leaked into the log:\n%s", name, buf.String())
		}
	}
}

// A wrong consent credential logs the discrete consent-credential-mismatch
// category (so an empty/wrong credential is visible) and never the credential.
func TestFlowLog_ConsentCredentialMismatch(t *testing.T) {
	h, buf := loggedFlowAS(t)
	_, challenge := pkcePair()
	form := baseAuthForm(h.clientID, challenge)
	form.Set("approve", "1")
	form.Set("consent_credential", "wrong-credential-value")
	rec := h.authorize(t, http.MethodPost, form)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 re-render, got %d", rec.Code)
	}
	line := logLineWithMsg(t, buf, "oauth authorize consent rejected")
	if line["reason"] != "consent-credential-mismatch" {
		t.Errorf("reason: want consent-credential-mismatch, got %v", line["reason"])
	}
	for _, secret := range []string{"wrong-credential-value", testCredential} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("a consent credential value leaked into the log:\n%s", buf.String())
		}
	}
}

// An unknown/used authorization code at /token logs invalid_grant +
// code-unknown-or-used (distinct from code-expired).
func TestFlowLog_TokenAuthCodeUnknown(t *testing.T) {
	h, buf := loggedFlowAS(t)
	verifier, _ := pkcePair()
	tf := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {"never-issued-code"},
		"client_id":     {h.clientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {verifier},
	}
	rec := h.token(t, tf, "application/x-www-form-urlencoded")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
	line := logLineWithMsg(t, buf, "oauth token rejected")
	if line["error"] != "invalid_grant" {
		t.Errorf("error: want invalid_grant, got %v", line["error"])
	}
	if line["reason"] != "code-unknown-or-used" {
		t.Errorf("reason: want code-unknown-or-used, got %v", line["reason"])
	}
}

// A wrong PKCE verifier at /token logs invalid_grant + pkce-mismatch, never the
// verifier value.
func TestFlowLog_TokenPKCEMismatch(t *testing.T) {
	h, buf := loggedFlowAS(t)
	_, challenge := pkcePair()
	form := baseAuthForm(h.clientID, challenge)
	form.Set("approve", "1")
	form.Set("consent_credential", testCredential)
	rec := h.authorize(t, http.MethodPost, form)
	loc, _ := url.Parse(rec.Header().Get("Location"))
	code := loc.Query().Get("code")

	const wrongVerifier = "wrong-verifier-0000000000-AAAAAAAAAAAAAAAAAAAAA"
	tf := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {h.clientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {wrongVerifier},
	}
	trec := h.token(t, tf, "application/x-www-form-urlencoded")
	if trec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", trec.Code)
	}
	line := logLineWithMsg(t, buf, "oauth token rejected")
	if line["reason"] != "pkce-mismatch" {
		t.Errorf("reason: want pkce-mismatch, got %v", line["reason"])
	}
	if strings.Contains(buf.String(), wrongVerifier) {
		t.Errorf("code_verifier value leaked into the log:\n%s", buf.String())
	}
}

// A missing code_verifier is the §2.2 PKCE "missing" outcome — invalid_request +
// code_verifier-missing.
func TestFlowLog_TokenVerifierMissing(t *testing.T) {
	h, buf := loggedFlowAS(t)
	tf := url.Values{
		"grant_type":   {"authorization_code"},
		"code":         {"some-code"},
		"client_id":    {h.clientID},
		"redirect_uri": {testRedirectURI},
	}
	rec := h.token(t, tf, "application/x-www-form-urlencoded")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", rec.Code)
	}
	line := logLineWithMsg(t, buf, "oauth token rejected")
	if line["error"] != "invalid_request" {
		t.Errorf("error: want invalid_request, got %v", line["error"])
	}
	if line["reason"] != "code_verifier-missing" {
		t.Errorf("reason: want code_verifier-missing, got %v", line["reason"])
	}
}
