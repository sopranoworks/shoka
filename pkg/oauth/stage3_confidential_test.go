package oauth

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/pkg/authz"
)

// B-71 Stage 3 — confidential-client (Client ID + Secret) auth at /token, REQUIRED IN ADDITION to
// PKCE. A confidential client is pre-issued (operator UI); it authorizes through the real
// /authorize consent (granted on the bare approve — no consent credential, the secret is the
// gate) and authenticates at /token with its secret (client_secret_basic OR client_secret_post)
// plus the mandatory PKCE verifier. The pre-issued scope rides onto the issued token.

// issueConfidential mints a confidential client straight into the store (the operator-UI path is
// covered by the manager + E2E tests) and returns its client_id + raw secret.
func (h testAS) issueConfidential(t *testing.T, scope string, validity time.Duration) (clientID, secret string) {
	t.Helper()
	entry, raw, err := h.store.IssueConfidentialClient(scope, "", validity, h.as.now())
	if err != nil {
		t.Fatalf("IssueConfidentialClient: %v", err)
	}
	return entry.Identifier, raw
}

// confidentialCode drives /authorize for a confidential client (approve, no consent credential)
// and returns the issued code + the PKCE verifier.
func (h testAS) confidentialCode(t *testing.T, clientID string) (code, verifier string) {
	t.Helper()
	v, challenge := pkcePair()
	form := baseAuthForm(clientID, challenge)
	form.Set("approve", "1")
	rec := h.authorize(t, http.MethodPost, form)
	if rec.Code != http.StatusFound {
		t.Fatalf("confidential authorize: want 302, got %d (%s)", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	c := loc.Query().Get("code")
	if c == "" {
		t.Fatalf("no code in confidential redirect: %s", loc)
	}
	return c, v
}

// tokenBasic POSTs /token with HTTP Basic client authentication (client_secret_basic).
func (h testAS) tokenBasic(t *testing.T, clientID, secret string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(clientID+":"+secret)))
	h.as.handleToken(rec, req)
	return rec
}

func TestStage3_ConfidentialTokenAuth(t *testing.T) {
	t.Run("client_secret_post + PKCE authenticates and issues with the pre-issued scope", func(t *testing.T) {
		h := newTestAS(t)
		clientID, secret := h.issueConfidential(t, "namespace:foo:r", time.Hour)
		code, verifier := h.confidentialCode(t, clientID)
		rec := h.token(t, url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"client_id":     {clientID},
			"redirect_uri":  {testRedirectURI},
			"code_verifier": {verifier},
			"client_secret": {secret},
		}, "application/x-www-form-urlencoded")
		if rec.Code != http.StatusOK {
			t.Fatalf("confidential token (post): want 200, got %d (%s)", rec.Code, rec.Body.String())
		}
		var tr tokenResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &tr)
		series, err := h.store.Lookup(tr.AccessToken, h.as.now())
		if err != nil {
			t.Fatalf("lookup confidential token: %v", err)
		}
		if series.Scope != "namespace:foo:r" {
			t.Fatalf("confidential token must carry the pre-issued scope, got %q", series.Scope)
		}
	})

	t.Run("client_secret_basic + PKCE authenticates and issues", func(t *testing.T) {
		h := newTestAS(t)
		clientID, secret := h.issueConfidential(t, "namespace:foo:r", time.Hour)
		code, verifier := h.confidentialCode(t, clientID)
		rec := h.tokenBasic(t, clientID, secret, url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"redirect_uri":  {testRedirectURI},
			"code_verifier": {verifier},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("confidential token (basic): want 200, got %d (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("a wrong secret is rejected (invalid_client)", func(t *testing.T) {
		h := newTestAS(t)
		clientID, _ := h.issueConfidential(t, "namespace:foo:r", time.Hour)
		code, verifier := h.confidentialCode(t, clientID)
		rec := h.token(t, url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"client_id":     {clientID},
			"redirect_uri":  {testRedirectURI},
			"code_verifier": {verifier},
			"client_secret": {"not-the-real-secret"},
		}, "application/x-www-form-urlencoded")
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid_client") {
			t.Fatalf("a wrong confidential secret must be invalid_client 401, got %d (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("a MISSING secret is rejected EVEN with valid PKCE (secret required)", func(t *testing.T) {
		h := newTestAS(t)
		clientID, _ := h.issueConfidential(t, "namespace:foo:r", time.Hour)
		code, verifier := h.confidentialCode(t, clientID)
		rec := h.token(t, url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"client_id":     {clientID},
			"redirect_uri":  {testRedirectURI},
			"code_verifier": {verifier}, // valid PKCE, but no client_secret
		}, "application/x-www-form-urlencoded")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("a confidential client without a secret must be rejected, got %d (%s)", rec.Code, rec.Body.String())
		}
	})

	// RED proof for PKCE-IN-ADDITION: a CORRECT secret but a WRONG PKCE verifier must STILL be
	// rejected. If the PKCE check were dropped for confidential clients, this secret-with-bad-PKCE
	// request would succeed — this test would then fail, catching the security regression.
	t.Run("a wrong PKCE verifier is rejected EVEN with the correct secret (PKCE in addition)", func(t *testing.T) {
		h := newTestAS(t)
		clientID, secret := h.issueConfidential(t, "namespace:foo:r", time.Hour)
		code, _ := h.confidentialCode(t, clientID)
		rec := h.token(t, url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"client_id":     {clientID},
			"redirect_uri":  {testRedirectURI},
			"code_verifier": {"the-wrong-verifier-value-0000000000000000"},
			"client_secret": {secret},
		}, "application/x-www-form-urlencoded")
		if rec.Code == http.StatusOK {
			t.Fatal("a confidential request with a correct secret but WRONG PKCE must NOT succeed (PKCE required in addition)")
		}
	})

	t.Run("an EXPIRED credential is rejected at /authorize", func(t *testing.T) {
		h := newTestAS(t)
		// validity already elapsed relative to h.as.now() — the credential is expired.
		clientID, _ := h.issueConfidential(t, "namespace:foo:r", time.Nanosecond)
		_, challenge := pkcePair()
		form := baseAuthForm(clientID, challenge)
		form.Set("approve", "1")
		if rec := h.authorize(t, http.MethodPost, form); rec.Code == http.StatusFound {
			t.Fatal("an expired confidential credential must not authorize")
		}
	})
}

// TestStage3_ConfidentialScopeEnforced: a confidential token's pre-issued scope is enforced by the
// authz gate — allowed on its namespace, denied on another. RED proof: if grantAuthorizationCode
// ignored the entry scope (issued "*"), the token would be all-access and "bar" would be allowed —
// this test would fail.
func TestStage3_ConfidentialScopeEnforced(t *testing.T) {
	h := newTestAS(t)
	clientID, secret := h.issueConfidential(t, "namespace:foo:r", time.Hour)
	code, verifier := h.confidentialCode(t, clientID)
	rec := h.token(t, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {verifier},
		"client_secret": {secret},
	}, "application/x-www-form-urlencoded")
	if rec.Code != http.StatusOK {
		t.Fatalf("token: %d (%s)", rec.Code, rec.Body.String())
	}
	var tr tokenResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &tr)
	series, err := h.store.Lookup(tr.AccessToken, h.as.now())
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	// The dormant tools/call gate, now live for this scoped token.
	if err := authz.Authorize(series.Scope, "foo", "", authz.LevelRead); err != nil {
		t.Fatalf("a namespace:foo token must be allowed on foo: %v", err)
	}
	if err := authz.Authorize(series.Scope, "bar", "", authz.LevelRead); err == nil {
		t.Fatal("a namespace:foo token must be DENIED on bar (scope enforced)")
	}
}
