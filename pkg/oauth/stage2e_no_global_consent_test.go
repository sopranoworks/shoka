package oauth

import (
	"net/http"
	"net/url"
	"testing"
)

// B-71 Stage 2e retires the global consent_credential fallback: /authorize consent is now
// PER-DOMAIN only. A client whose domain has no per-domain consent set (Consent == nil), or no
// "domain" entry at all, CANNOT be approved — it is denied (never silently allow). A domain WITH
// per-domain consent still authorizes (parity with a seeded deployment).
//
// These exercise the dynamic-trust path (the production wiring) so the only thing that can grant
// consent is the store entry — there is no global credential anywhere in the AuthServer.
func TestStage2e_ConsentRequiresPerDomain_NoGlobalFallback(t *testing.T) {
	t.Run("trusted domain with NO per-domain consent is denied", func(t *testing.T) {
		h := newTestAS(t)
		h.useDynamicTrust()
		// Trust the host but set NO per-domain consent (Consent == nil) — like a domain the
		// operator added in the UI but has not yet given a consent secret.
		h.trustDomain(t, h.host)

		_, challenge := pkcePair()
		form := baseAuthForm(h.clientID, challenge)
		form.Set("approve", "1")
		form.Set("consent_credential", testCredential) // the FORMER global secret
		rec := h.authorize(t, http.MethodPost, form)
		if rec.Code == http.StatusFound {
			t.Fatal("a trusted domain with no per-domain consent must NOT authorize (Stage 2e: no global fallback)")
		}
	})

	t.Run("trusted domain WITH per-domain consent authorizes", func(t *testing.T) {
		h := newTestAS(t)
		h.useDynamicTrust()
		h.trustDomainWithConsent(t, h.host) // operator set the domain's consent

		verifier, challenge := pkcePair()
		form := baseAuthForm(h.clientID, challenge)
		form.Set("approve", "1")
		form.Set("consent_credential", testCredential)
		rec := h.authorize(t, http.MethodPost, form)
		if rec.Code != http.StatusFound {
			t.Fatalf("a domain WITH per-domain consent must authorize: %d (%s)", rec.Code, rec.Body.String())
		}
		// And it actually issues a working token (parity end to end).
		loc, _ := url.Parse(rec.Header().Get("Location"))
		code := loc.Query().Get("code")
		tf := url.Values{
			"grant_type": {"authorization_code"}, "code": {code},
			"client_id": {h.clientID}, "redirect_uri": {testRedirectURI}, "code_verifier": {verifier},
		}
		if rec := h.token(t, tf, "application/x-www-form-urlencoded"); rec.Code != http.StatusOK {
			t.Fatalf("token exchange after per-domain consent must succeed: %d (%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("wrong per-domain credential is denied", func(t *testing.T) {
		h := newTestAS(t)
		h.useDynamicTrust()
		h.trustDomainWithConsent(t, h.host) // consent = testCredential

		_, challenge := pkcePair()
		form := baseAuthForm(h.clientID, challenge)
		form.Set("approve", "1")
		form.Set("consent_credential", "not-the-domains-consent")
		if rec := h.authorize(t, http.MethodPost, form); rec.Code == http.StatusFound {
			t.Fatal("a wrong per-domain credential must be denied")
		}
	})
}
