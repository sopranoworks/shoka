package oauth

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// B-71 Stage 2c regression tests: the live verifier/consent/TTL now read the dynamic "domain"
// store, seeded from static config. The risk is regression — these prove parity + the
// seed-skip RED. (The CIMD-switch is covered here by Go regression with the in-package SSRF
// relaxation, never the shipped binary; DCR + self-issued are covered by the real-browser
// Playwright spec — the CIMD metadata fetch is SSRF-hardened and cannot be hermetically
// real-browser-tested, a standing finding.)

// TestStage2c_CIMDTrustFromDynamicStore: after the switch, a CIMD host is trusted iff a
// "domain" entry covers it. Seed-skip RED: with the dynamic source set but NO entry, the
// previously-trusted host no longer authorizes — proving the seed is what preserves behaviour.
func TestStage2c_CIMDTrustFromDynamicStore(t *testing.T) {
	t.Run("seed skipped, untrusted, connect rejected", func(t *testing.T) {
		h := newTestAS(t)
		h.useDynamicTrust() // production trust source, but the store is unseeded
		_, challenge := pkcePair()
		form := baseAuthForm(h.clientID, challenge)
		form.Set("approve", "1")
		form.Set("consent_credential", testCredential)
		rec := h.authorize(t, http.MethodPost, form)
		if rec.Code == http.StatusFound {
			t.Fatal("without the seed, an untrusted CIMD domain must NOT authorize (seed-skip regression)")
		}
	})

	t.Run("seeded, trusted, connect authorizes + issues", func(t *testing.T) {
		h := newTestAS(t)
		h.useDynamicTrust()
		h.trustDomain(t, h.host) // the seed: the domain becomes trusted via the store

		verifier, challenge := pkcePair()
		form := baseAuthForm(h.clientID, challenge)
		form.Set("approve", "1")
		form.Set("consent_credential", testCredential)
		rec := h.authorize(t, http.MethodPost, form)
		if rec.Code != http.StatusFound {
			t.Fatalf("a seeded/trusted CIMD domain must authorize: %d (%s)", rec.Code, rec.Body.String())
		}
		loc, _ := url.Parse(rec.Header().Get("Location"))
		code := loc.Query().Get("code")
		tf := url.Values{
			"grant_type": {"authorization_code"}, "code": {code},
			"client_id": {h.clientID}, "redirect_uri": {testRedirectURI}, "code_verifier": {verifier},
		}
		rec = h.token(t, tf, "application/x-www-form-urlencoded")
		if rec.Code != http.StatusOK {
			t.Fatalf("a seeded CIMD token exchange must succeed: %d (%s)", rec.Code, rec.Body.String())
		}
	})
}

// TestStage2c_DCRUntrustedDomainRejected: the deferred DCR trust gate now rejects a
// registration whose redirect host is not a trusted "domain" entry (consistent with CIMD's
// untrusted-domain rejection). A trusted host registers (covered by registerClient elsewhere).
func TestStage2c_DCRUntrustedDomainRejected(t *testing.T) {
	h := newTestAS(t)
	// Do NOT trust untrusted.example. Register directly (registerClient would trust it).
	body := `{"redirect_uris":["https://untrusted.example/cb"],"token_endpoint_auth_method":"none",` +
		`"grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`
	rec := h.register(t, body, "application/json")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("an untrusted DCR domain must be rejected: got %d (%s)", rec.Code, rec.Body.String())
	}
	// A trusted host succeeds.
	h.trustDomain(t, "trusted.example")
	ok := `{"redirect_uris":["https://trusted.example/cb"],"token_endpoint_auth_method":"none",` +
		`"grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`
	if rec := h.register(t, ok, "application/json"); rec.Code != http.StatusCreated {
		t.Fatalf("a trusted DCR domain must register: got %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestStage2c_PerDomainConsent: the /authorize consent gate uses the domain's per-domain
// consent, distinct from the global. Presenting the per-domain secret authorizes; presenting
// the (different) global one is rejected — proving the per-domain path is live, not the global.
func TestStage2c_PerDomainConsent(t *testing.T) {
	h := newTestAS(t)
	h.useDynamicTrust()
	const perDomain = "the-per-domain-consent-DISTINCT-from-global"
	// Seed the CIMD host as a "domain" entry carrying the per-domain consent (and a TTL).
	if err := h.store.SeedDomainRegistrationsFromDomains([]string{h.host}, perDomain, time.Hour, 90*24*time.Hour, h.as.now()); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The per-domain secret authorizes.
	_, challenge := pkcePair()
	form := baseAuthForm(h.clientID, challenge)
	form.Set("approve", "1")
	form.Set("consent_credential", perDomain)
	if rec := h.authorize(t, http.MethodPost, form); rec.Code != http.StatusFound {
		t.Fatalf("the per-domain consent must authorize: %d (%s)", rec.Code, rec.Body.String())
	}
	// The GLOBAL consent (different value) is rejected — the per-domain path is authoritative.
	_, challenge2 := pkcePair()
	form2 := baseAuthForm(h.clientID, challenge2)
	form2.Set("approve", "1")
	form2.Set("consent_credential", testCredential) // the global secret, != perDomain
	if rec := h.authorize(t, http.MethodPost, form2); rec.Code == http.StatusFound {
		t.Fatal("once a domain has per-domain consent, the global secret must NOT authorize it")
	}
}

// TestStage2c_PerDomainTTL: token issuance uses the domain's EffectiveTTL (here distinct from
// the global default), proving the issuance TTL is now per-domain.
func TestStage2c_PerDomainTTL(t *testing.T) {
	h := newTestAS(t)
	h.useDynamicTrust()
	const domainAccess = 2 * time.Hour // != the global 1h default
	if err := h.store.SeedDomainRegistrationsFromDomains([]string{h.host}, testCredential, domainAccess, 30*24*time.Hour, h.as.now()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	verifier, challenge := pkcePair()
	form := baseAuthForm(h.clientID, challenge)
	form.Set("approve", "1")
	form.Set("consent_credential", testCredential)
	rec := h.authorize(t, http.MethodPost, form)
	if rec.Code != http.StatusFound {
		t.Fatalf("authorize: %d (%s)", rec.Code, rec.Body.String())
	}
	loc, _ := url.Parse(rec.Header().Get("Location"))
	tf := url.Values{
		"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"client_id": {h.clientID}, "redirect_uri": {testRedirectURI}, "code_verifier": {verifier},
	}
	rec = h.token(t, tf, "application/x-www-form-urlencoded")
	if rec.Code != http.StatusOK {
		t.Fatalf("token: %d (%s)", rec.Code, rec.Body.String())
	}
	var tr tokenResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &tr)
	// expires_in reflects the per-domain 2h, not the global 1h.
	if tr.ExpiresIn < int((domainAccess-time.Minute)/time.Second) {
		t.Fatalf("issued access TTL must be the per-domain %v, got expires_in=%d", domainAccess, tr.ExpiresIn)
	}
}
