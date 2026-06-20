package oauth

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// B-62: pin Shoka's actual /token responses to RFC 6749 §5.1 (success) and §5.2
// (error) header-by-header and field-by-field. The B-62 audit confirmed the
// responses are CONFORMANT (no product change); this test locks that shape so a
// regression — e.g. dropping Cache-Control: no-store, changing the Content-Type, or
// breaking refresh-token predecessor-invalidation — FAILS here, locally, under
// `go test ./...`. It is the runnable companion to the B-61 Docker harness's
// strict-client assertions.
//
// Audit note on Content-Type: §5.1's normative text requires only the
// `application/json` media type; the `;charset=UTF-8` in the RFC's examples is
// non-normative, and RFC 8259 §11 gives the charset parameter no defined meaning
// for application/json (JSON is always UTF-8). Shoka sends `application/json`,
// which this test asserts exactly — adding charset is neither required nor the
// cause of the live failure (claude.ai's python-httpx consumer ignores
// Content-Type when decoding JSON).

// issueTokens runs authorize -> code -> token and returns the parsed success
// response plus the raw recorder, so a test can assert both body and headers.
func (h testAS) issueTokens(t *testing.T) (tokenResponse, *http.Header) {
	t.Helper()
	h.trustDomainWithConsent(t, h.host) // B-71 Stage 2e: the connecting domain needs its own consent
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
		t.Fatalf("no code in redirect: %s", loc)
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
	var tr tokenResponse
	if err := json.Unmarshal(trec.Body.Bytes(), &tr); err != nil {
		t.Fatalf("decode token resp: %v", err)
	}
	hdr := trec.Header()
	return tr, &hdr
}

// assertSuccessHeaders pins the RFC 6749 §5.1 success-response headers Shoka
// actually sends: Content-Type application/json, Cache-Control: no-store, Pragma:
// no-cache.
func assertSuccessHeaders(t *testing.T, h *http.Header, where string) {
	t.Helper()
	if got := h.Get("Content-Type"); got != "application/json" {
		t.Errorf("%s: Content-Type = %q, want exactly %q (§5.1)", where, got, "application/json")
	}
	if got := h.Get("Cache-Control"); !strings.Contains(strings.ToLower(got), "no-store") {
		t.Errorf("%s: Cache-Control = %q, want it to contain no-store (§5.1)", where, got)
	}
	if got := h.Get("Pragma"); strings.ToLower(got) != "no-cache" {
		t.Errorf("%s: Pragma = %q, want no-cache (§5.1)", where, got)
	}
}

// TestTokenSuccessResponse_RFC6749_5_1 asserts the success response shape on both
// the authorization_code and the refresh_token grants.
func TestTokenSuccessResponse_RFC6749_5_1(t *testing.T) {
	h := newTestAS(t)

	tr, hdr := h.issueTokens(t)
	assertSuccessHeaders(t, hdr, "authorization_code")
	if tr.AccessToken == "" {
		t.Errorf("authorization_code: empty access_token (§5.1 REQUIRED)")
	}
	if tr.TokenType != "Bearer" {
		t.Errorf("authorization_code: token_type = %q, want Bearer (§5.1/§7.1)", tr.TokenType)
	}
	if tr.ExpiresIn <= 0 {
		t.Errorf("authorization_code: expires_in = %d, want positive (§5.1 RECOMMENDED)", tr.ExpiresIn)
	}
	if tr.RefreshToken == "" {
		t.Errorf("authorization_code: empty refresh_token (public-client rotation needs one)")
	}

	// refresh_token grant returns the same conformant shape.
	rf := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tr.RefreshToken}}
	rrec := h.token(t, rf, "application/x-www-form-urlencoded")
	if rrec.Code != http.StatusOK {
		t.Fatalf("refresh: want 200, got %d (%s)", rrec.Code, rrec.Body.String())
	}
	rhdr := rrec.Header()
	assertSuccessHeaders(t, &rhdr, "refresh_token")
}

// TestRefreshRotation_InvalidatesPredecessor_SameResponse asserts the documented
// OAuth 2.1 public-client requirement: rotating a refresh token returns a NEW
// access+refresh pair in the same response AND invalidates the old refresh token.
// (claude.ai docs: "rotate ... return the new refresh token in the same response
// that invalidates the old one.")
func TestRefreshRotation_InvalidatesPredecessor_SameResponse(t *testing.T) {
	h := newTestAS(t)
	tr, _ := h.issueTokens(t)

	rf := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tr.RefreshToken}}
	rrec := h.token(t, rf, "application/x-www-form-urlencoded")
	if rrec.Code != http.StatusOK {
		t.Fatalf("refresh: want 200, got %d (%s)", rrec.Code, rrec.Body.String())
	}
	var tr2 tokenResponse
	if err := json.Unmarshal(rrec.Body.Bytes(), &tr2); err != nil {
		t.Fatalf("decode rotated resp: %v", err)
	}
	// New pair returned in the SAME response.
	if tr2.AccessToken == "" || tr2.RefreshToken == "" {
		t.Fatalf("rotation returned an empty handle: %+v", tr2)
	}
	if tr2.AccessToken == tr.AccessToken || tr2.RefreshToken == tr.RefreshToken {
		t.Fatalf("rotation did not change handles: old=%s.. new=%s..",
			tr.RefreshToken[:6], tr2.RefreshToken[:6])
	}
	// The OLD refresh token is invalidated by that same rotation.
	old := h.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tr.RefreshToken}}, "application/x-www-form-urlencoded")
	if old.Code != http.StatusBadRequest || !strings.Contains(old.Body.String(), "invalid_grant") {
		t.Fatalf("old refresh reuse: want 400 invalid_grant, got %d (%s)", old.Code, old.Body.String())
	}
}

// TestTokenErrorResponse_RFC6749_5_2 asserts the error-response shape: 400 status,
// application/json, Cache-Control: no-store, and a body with a recognised RFC 6749
// `error` code.
func TestTokenErrorResponse_RFC6749_5_2(t *testing.T) {
	h := newTestAS(t)
	rec := h.token(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {"bogus-refresh"}}, "application/x-www-form-urlencoded")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("error status = %d, want 400 (§5.2)", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("error Content-Type = %q, want application/json (§5.2)", got)
	}
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(strings.ToLower(got), "no-store") {
		t.Errorf("error Cache-Control = %q, want no-store", got)
	}
	var body struct {
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not JSON: %v (%s)", err, rec.Body.String())
	}
	switch body.Error {
	case "invalid_request", "invalid_client", "invalid_grant",
		"unauthorized_client", "unsupported_grant_type", "invalid_scope":
		// a recognised §5.2 code
	default:
		t.Errorf("error code = %q, not an RFC 6749 §5.2 code", body.Error)
	}
}
