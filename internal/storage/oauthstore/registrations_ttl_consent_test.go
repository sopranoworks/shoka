package oauthstore

import (
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// B-71 Stage 2b: per-domain TTL + consent on a "domain" RegistrationEntry.

// TestEntryTTL_EffectiveTTL: a positive per-domain TTL wins; unset falls back to the global
// finite default; a 0/negative per-domain value does NOT yield 0/infinite — it floors to the
// finite default. RED proof: drop the `d > 0` floor (use the per-domain value unconditionally)
// → a 0 per-domain access TTL yields 0 → this test fails.
func TestEntryTTL_EffectiveTTL(t *testing.T) {
	defA, defR := time.Hour, 90*24*time.Hour

	// Positive per-domain values win.
	e := RegistrationEntry{TTL: &EntryTTL{AccessSeconds: 1800, RefreshSeconds: 3600}}
	if a, r := e.EffectiveTTL(defA, defR); a != 30*time.Minute || r != time.Hour {
		t.Fatalf("set TTL: got %v/%v, want 30m/1h", a, r)
	}
	// Unset (nil TTL) → global defaults.
	if a, r := (RegistrationEntry{}).EffectiveTTL(defA, defR); a != defA || r != defR {
		t.Fatalf("unset: got %v/%v, want %v/%v", a, r, defA, defR)
	}
	// 0 / negative per-domain → floor to the finite default, never non-positive.
	e = RegistrationEntry{TTL: &EntryTTL{AccessSeconds: 0, RefreshSeconds: -5}}
	a, r := e.EffectiveTTL(defA, defR)
	if a != defA || r != defR {
		t.Fatalf("0/negative must floor to the finite default: got %v/%v", a, r)
	}
	if a <= 0 || r <= 0 {
		t.Fatal("effective TTL must never be non-positive (no-indefinite)")
	}
	// Partial: access set, refresh unset → access wins, refresh defaults.
	e = RegistrationEntry{TTL: &EntryTTL{AccessSeconds: 600}}
	if a, r := e.EffectiveTTL(defA, defR); a != 10*time.Minute || r != defR {
		t.Fatalf("partial: got %v/%v, want 10m/%v", a, r, defR)
	}
}

// TestEntryConsent_HashedAndVerify: SetConsent stores a HASH, not the raw secret (no raw
// bytes anywhere in the stored entry); VerifyConsent accepts the correct value and rejects a
// wrong/empty one; a no-consent entry denies (explicit). RED proof: store the raw consent (or
// compare the raw value) → the raw appears at rest / a wrong value is accepted → this fails.
func TestEntryConsent_HashedAndVerify(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	const secret = "the-per-domain-consent-secret"

	e, err := s.CreateRegistration(RegistrationModeDomain, "d.example", now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	e.SetConsent(secret)
	if e.Consent == nil || e.Consent.Hash != hashHandle(secret) {
		t.Fatalf("SetConsent must store hashHandle(secret), got %+v", e.Consent)
	}
	if e.Consent.Hash == secret {
		t.Fatal("the consent hash must not equal the raw secret")
	}
	if err := s.UpdateRegistration(e); err != nil {
		t.Fatalf("update: %v", err)
	}

	// The raw secret must not appear anywhere in the stored entry bytes.
	var stored string
	_ = s.db.View(func(tx *bolt.Tx) error {
		stored = string(tx.Bucket([]byte(registrationsBucket)).Get([]byte(e.ID)))
		return nil
	})
	if strings.Contains(stored, secret) {
		t.Fatalf("the raw consent secret must not be stored at rest")
	}

	reread, _ := s.GetRegistration(e.ID)
	if !reread.VerifyConsent(secret) {
		t.Fatal("VerifyConsent must accept the correct presented secret")
	}
	if reread.VerifyConsent("wrong-secret") {
		t.Fatal("VerifyConsent must reject a wrong secret")
	}
	if reread.VerifyConsent("") {
		t.Fatal("VerifyConsent must reject an empty presented value")
	}

	// No-consent policy: an entry without per-domain consent denies any presented value.
	noConsent, _ := s.CreateRegistration(RegistrationModeDomain, "nc.example", now)
	if noConsent.VerifyConsent(secret) || noConsent.VerifyConsent("") {
		t.Fatal("a no-consent entry must deny (never silently allow)")
	}
	// SetConsent("") clears it back to no-consent.
	e.SetConsent("")
	if e.Consent != nil || e.VerifyConsent(secret) {
		t.Fatal("SetConsent(\"\") must clear the per-domain consent")
	}
}

// TestRegistrations_DecodeSafe_NoTTLConsent: a Stage-1/2a-vintage entry (no ttl/consent keys)
// decodes safely; EffectiveTTL falls back to the global defaults and VerifyConsent denies.
func TestRegistrations_DecodeSafe_NoTTLConsent(t *testing.T) {
	s := openTemp(t)
	old := `{"id":"old","registration_mode":"domain","identifier":"old.example","created_at":"2026-06-19T00:00:00Z"}`
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(registrationsBucket)).Put([]byte("old"), []byte(old))
	}); err != nil {
		t.Fatalf("seed old entry: %v", err)
	}
	e, err := s.GetRegistration("old")
	if err != nil {
		t.Fatalf("a TTL/Consent-less entry must decode: %v", err)
	}
	if e.TTL != nil || e.Consent != nil {
		t.Fatalf("TTL/Consent must be nil on an old entry: %+v", e)
	}
	if a, r := e.EffectiveTTL(time.Hour, 90*24*time.Hour); a != time.Hour || r != 90*24*time.Hour {
		t.Fatalf("EffectiveTTL must fall back to defaults on a no-TTL entry: got %v/%v", a, r)
	}
	if e.VerifyConsent("anything") {
		t.Fatal("VerifyConsent must deny on a no-consent entry")
	}
}
