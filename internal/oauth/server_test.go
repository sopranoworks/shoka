package oauth

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shoka/mcp-server/internal/storage/oauthstore"
)

const (
	testCredential  = "sekret-consent"
	testRedirectURI = "https://app.example/cb"
	testResource    = "https://rs.example/mcp"
)

func pkcePair() (verifier, challenge string) {
	verifier = "verifier-0123456789-ABCDEFGHIJKLMNOPQRSTUVWXYZ-abcdefghij"
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

type testAS struct {
	as       *AuthServer
	store    *oauthstore.Store
	clientID string
}

func newTestAS(t *testing.T) testAS {
	t.Helper()
	// The client's CIMD metadata server.
	cimd := httptest.NewTLSServer(nil)
	t.Cleanup(cimd.Close)
	clientID := cimd.URL + "/client-meta"
	cimd.Config.Handler = docHandler(t, ClientMetadata{
		ClientID:                clientID,
		ClientName:              "Test Client",
		RedirectURIs:            []string{testRedirectURI},
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethod: "none",
	})
	host, _, _ := net.SplitHostPort(strings.TrimPrefix(cimd.URL, "https://"))

	v := NewVerifier([]string{host})
	v.isBlockedIP = func(net.IP) bool { return false }
	v.tlsConfig = &tls.Config{RootCAs: cimd.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}

	store, err := oauthstore.Open(filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	as := NewAuthServer(store, v, AuthServerConfig{
		ExternalURL: "https://rs.example",
		PrincipalAuth: ConsentCredentialAuth{
			Credential: testCredential,
			Principal:  oauthstore.Principal{Name: "Operator", Email: "op@example.test"},
		},
	})
	return testAS{as: as, store: store, clientID: clientID}
}

func (h testAS) authorize(t *testing.T, method string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	var req *http.Request
	if method == http.MethodGet {
		req = httptest.NewRequest(method, "/authorize?"+form.Encode(), nil)
	} else {
		req = httptest.NewRequest(method, "/authorize", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	h.as.handleAuthorize(rec, req)
	return rec
}

func (h testAS) token(t *testing.T, form url.Values, contentType string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	h.as.handleToken(rec, req)
	return rec
}

func baseAuthForm(clientID, challenge string) url.Values {
	return url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {testRedirectURI},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"resource":              {testResource},
		"state":                 {"state-xyz"},
	}
}

// Full happy path: authorize (consent) -> code -> token -> access+refresh ->
// rotate refresh. Verifies the principal + resource binding and rotation.
func TestFullFlow(t *testing.T) {
	h := newTestAS(t)
	verifier, challenge := pkcePair()

	form := baseAuthForm(h.clientID, challenge)
	form.Set("approve", "1")
	form.Set("consent_credential", testCredential)
	rec := h.authorize(t, http.MethodPost, form)
	if rec.Code != http.StatusFound {
		t.Fatalf("authorize: want 302, got %d (%s)", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("state") != "state-xyz" {
		t.Fatalf("state not echoed: %s", loc)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in redirect: %s", loc)
	}

	// Exchange the code.
	tf := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {h.clientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {verifier},
	}
	rec = h.token(t, tf, "application/x-www-form-urlencoded")
	if rec.Code != http.StatusOK {
		t.Fatalf("token: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var tr tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &tr); err != nil {
		t.Fatalf("decode token resp: %v", err)
	}
	if tr.AccessToken == "" || tr.RefreshToken == "" || tr.TokenType != "Bearer" || tr.ExpiresIn <= 0 {
		t.Fatalf("bad token response: %+v", tr)
	}

	// The access token resolves to the bound principal + resource.
	series, err := h.store.Lookup(tr.AccessToken, h.as.now())
	if err != nil {
		t.Fatalf("lookup access token: %v", err)
	}
	if series.Principal.Name != "Operator" || series.Resource != testResource || series.ClientID != h.clientID {
		t.Fatalf("binding wrong: %+v", series)
	}

	// Rotate the refresh token.
	rf := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tr.RefreshToken}}
	rec = h.token(t, rf, "application/x-www-form-urlencoded")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var tr2 tokenResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &tr2)
	if tr2.RefreshToken == tr.RefreshToken || tr2.AccessToken == tr.AccessToken {
		t.Fatalf("rotation did not change handles")
	}
	// Old refresh is dead.
	rec = h.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tr.RefreshToken}}, "application/x-www-form-urlencoded")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Fatalf("old refresh reuse: want 400 invalid_grant, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestAuthorize_GETRendersConsent(t *testing.T) {
	h := newTestAS(t)
	_, challenge := pkcePair()
	rec := h.authorize(t, http.MethodGet, baseAuthForm(h.clientID, challenge))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Approve") {
		t.Fatalf("consent page: got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAuthorize_WrongCredentialReRenders(t *testing.T) {
	h := newTestAS(t)
	_, challenge := pkcePair()
	form := baseAuthForm(h.clientID, challenge)
	form.Set("approve", "1")
	form.Set("consent_credential", "wrong")
	rec := h.authorize(t, http.MethodPost, form)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	if rec.Header().Get("Location") != "" {
		t.Fatalf("must NOT redirect on bad credential")
	}
	if !strings.Contains(rec.Body.String(), "Incorrect consent credential") {
		t.Fatalf("expected error notice, got %s", rec.Body.String())
	}
}

func TestAuthorize_MissingPKCERedirectsError(t *testing.T) {
	h := newTestAS(t)
	form := baseAuthForm(h.clientID, "")
	form.Del("code_challenge")
	form.Set("approve", "1")
	form.Set("consent_credential", testCredential)
	rec := h.authorize(t, http.MethodPost, form)
	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("error") != "invalid_request" {
		t.Fatalf("want error=invalid_request, got %s", loc)
	}
}

func TestAuthorize_UntrustedClientOnPageError(t *testing.T) {
	h := newTestAS(t)
	_, challenge := pkcePair()
	form := baseAuthForm("https://untrusted.example/meta", challenge)
	rec := h.authorize(t, http.MethodGet, form)
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
		t.Fatalf("untrusted client: want 400 on-page, got %d loc=%s", rec.Code, rec.Header().Get("Location"))
	}
	if !strings.Contains(rec.Body.String(), "invalid_client") {
		t.Fatalf("want invalid_client, got %s", rec.Body.String())
	}
}

func TestAuthorize_BadRedirectURIOnPageError(t *testing.T) {
	h := newTestAS(t)
	_, challenge := pkcePair()
	form := baseAuthForm(h.clientID, challenge)
	form.Set("redirect_uri", "https://attacker.example/steal")
	rec := h.authorize(t, http.MethodGet, form)
	if rec.Code != http.StatusBadRequest || rec.Header().Get("Location") != "" {
		t.Fatalf("bad redirect_uri must be on-page 400, got %d loc=%s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestAuthorize_BadResourceRedirectsInvalidTarget(t *testing.T) {
	h := newTestAS(t)
	_, challenge := pkcePair()
	form := baseAuthForm(h.clientID, challenge)
	form.Set("resource", "https://someone-else.example/mcp")
	rec := h.authorize(t, http.MethodGet, form)
	if rec.Code != http.StatusFound {
		t.Fatalf("want 302, got %d", rec.Code)
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	if loc.Query().Get("error") != "invalid_target" {
		t.Fatalf("want invalid_target, got %s", loc)
	}
}

func TestToken_RejectsJSONContentType(t *testing.T) {
	h := newTestAS(t)
	rec := h.token(t, url.Values{"grant_type": {"authorization_code"}}, "application/json")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_request") {
		t.Fatalf("want 400 invalid_request for JSON, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestToken_UnsupportedGrant(t *testing.T) {
	h := newTestAS(t)
	rec := h.token(t, url.Values{"grant_type": {"password"}}, "application/x-www-form-urlencoded")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "unsupported_grant_type") {
		t.Fatalf("want unsupported_grant_type, got %d (%s)", rec.Code, rec.Body.String())
	}
}

// Helper that drives authorize->code and returns the issued code.
func (h testAS) issueCode(t *testing.T, challenge string) string {
	t.Helper()
	form := baseAuthForm(h.clientID, challenge)
	form.Set("approve", "1")
	form.Set("consent_credential", testCredential)
	rec := h.authorize(t, http.MethodPost, form)
	if rec.Code != http.StatusFound {
		t.Fatalf("issueCode authorize: %d (%s)", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	return loc.Query().Get("code")
}

func TestToken_WrongPKCEVerifier(t *testing.T) {
	h := newTestAS(t)
	_, challenge := pkcePair()
	code := h.issueCode(t, challenge)
	rec := h.token(t, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {h.clientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {"the-wrong-verifier"},
	}, "application/x-www-form-urlencoded")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Fatalf("want invalid_grant, got %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestToken_CodeReplay(t *testing.T) {
	h := newTestAS(t)
	verifier, challenge := pkcePair()
	code := h.issueCode(t, challenge)
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {h.clientID},
		"redirect_uri":  {testRedirectURI},
		"code_verifier": {verifier},
	}
	if rec := h.token(t, form, "application/x-www-form-urlencoded"); rec.Code != http.StatusOK {
		t.Fatalf("first exchange should succeed: %d (%s)", rec.Code, rec.Body.String())
	}
	if rec := h.token(t, form, "application/x-www-form-urlencoded"); rec.Code != http.StatusBadRequest {
		t.Fatalf("code replay must fail: %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestToken_RedirectURIMismatch(t *testing.T) {
	h := newTestAS(t)
	verifier, challenge := pkcePair()
	code := h.issueCode(t, challenge)
	rec := h.token(t, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {h.clientID},
		"redirect_uri":  {"https://app.example/different"},
		"code_verifier": {verifier},
	}, "application/x-www-form-urlencoded")
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid_grant") {
		t.Fatalf("want invalid_grant, got %d (%s)", rec.Code, rec.Body.String())
	}
}
