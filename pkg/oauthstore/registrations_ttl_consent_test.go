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

// TestEntryConsent_PlaintextAndVerify (2026-06-20 model): SetConsent stores the value PLAINTEXT
// (operator-readable — the threat model found consent secrecy redundant), ConsentValue returns it,
// and it IS present at rest; VerifyConsent (constant-time) accepts the correct value and rejects a
// wrong/empty one; a no-consent entry denies (explicit). RED proof: hash it / hide it from
// ConsentValue → the value is not readable → this fails.
func TestEntryConsent_PlaintextAndVerify(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	const value = "the-per-domain-consent-value"

	e, err := s.CreateRegistration(RegistrationModeDomain, "d.example", now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	e.SetConsent(value)
	if e.Consent == nil || e.Consent.Value != value || e.ConsentValue() != value {
		t.Fatalf("SetConsent must store the value plaintext, got %+v", e.Consent)
	}
	if err := s.UpdateRegistration(e); err != nil {
		t.Fatalf("update: %v", err)
	}

	// The value IS readable at rest (intentional, the whole point) and survives the round-trip.
	var stored string
	_ = s.db.View(func(tx *bolt.Tx) error {
		stored = string(tx.Bucket([]byte(registrationsBucket)).Get([]byte(e.ID)))
		return nil
	})
	if !strings.Contains(stored, value) {
		t.Fatalf("the plaintext consent value must be stored readable")
	}

	reread, _ := s.GetRegistration(e.ID)
	if reread.ConsentValue() != value {
		t.Fatalf("the consent value must be readable back, got %q", reread.ConsentValue())
	}
	if !reread.VerifyConsent(value) {
		t.Fatal("VerifyConsent must accept the correct presented value")
	}
	if reread.VerifyConsent("wrong-value") {
		t.Fatal("VerifyConsent must reject a wrong value")
	}
	if reread.VerifyConsent("") {
		t.Fatal("VerifyConsent must reject an empty presented value")
	}

	// No-consent policy: an entry without per-domain consent denies any presented value.
	noConsent, _ := s.CreateRegistration(RegistrationModeDomain, "nc.example", now)
	if noConsent.VerifyConsent(value) || noConsent.VerifyConsent("") {
		t.Fatal("a no-consent entry must deny (never silently allow)")
	}
	// SetConsent("") clears it back to no-consent.
	e.SetConsent("")
	if e.Consent != nil || e.VerifyConsent(value) {
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

// TestGenerateDomainConsent (2026-06-20): mints a plaintext value, persists it readable, re-rolls
// on a second call, and errors on an unknown/non-domain id. RED proof: have it hash the value (or
// not persist) → the value is not readable back / VerifyConsent fails → this fails.
func TestGenerateDomainConsent(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	dom, err := s.CreateRegistration(RegistrationModeDomain, "connector.example", now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	v1, err := s.GenerateDomainConsent(dom.ID)
	if err != nil || v1 == "" {
		t.Fatalf("generate: v=%q err=%v", v1, err)
	}
	// Persisted PLAINTEXT and readable; verifies the generated value.
	reread, _ := s.GetRegistration(dom.ID)
	if reread.ConsentValue() != v1 {
		t.Fatalf("generated value must be readable back: got %q want %q", reread.ConsentValue(), v1)
	}
	if !reread.VerifyConsent(v1) {
		t.Fatal("the generated value must verify at /authorize")
	}

	// Regenerate re-rolls: a different value, and the OLD value stops verifying.
	v2, err := s.GenerateDomainConsent(dom.ID)
	if err != nil || v2 == v1 {
		t.Fatalf("regenerate must re-roll: v2=%q v1=%q err=%v", v2, v1, err)
	}
	reread, _ = s.GetRegistration(dom.ID)
	if reread.VerifyConsent(v1) || !reread.VerifyConsent(v2) {
		t.Fatal("after re-roll the old value must stop verifying and the new one must verify")
	}

	// Unknown id → ErrNotFound; a confidential entry → error (not a domain).
	if _, err := s.GenerateDomainConsent("no-such-id"); err != ErrNotFound {
		t.Fatalf("unknown id: err=%v, want ErrNotFound", err)
	}
	conf, _, _ := s.IssueConfidentialClient("*", time.Hour, now)
	if _, err := s.GenerateDomainConsent(conf.ID); err == nil {
		t.Fatal("GenerateDomainConsent on a confidential entry must error")
	}
}

// TestEntryConsent_HashedRecordMigratesToNoValue (2026-06-20 transition): a pre-change record that
// stored only a hash decodes with Value "" (the hash cannot be un-hashed) — so the domain shows no
// readable consent and denies at /authorize until the operator regenerates. Acceptable per the
// threat model.
func TestEntryConsent_HashedRecordMigratesToNoValue(t *testing.T) {
	s := openTemp(t)
	old := `{"id":"h","registration_mode":"domain","identifier":"h.example","created_at":"2026-06-19T00:00:00Z","consent":{"hash":"deadbeef"}}`
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(registrationsBucket)).Put([]byte("h"), []byte(old))
	}); err != nil {
		t.Fatalf("seed hashed entry: %v", err)
	}
	e, err := s.GetRegistration("h")
	if err != nil {
		t.Fatalf("a hashed-vintage entry must still decode: %v", err)
	}
	if e.ConsentValue() != "" {
		t.Fatalf("a hash-only consent must decode with no readable value, got %q", e.ConsentValue())
	}
	if e.VerifyConsent("anything") {
		t.Fatal("a hash-only (unmigrated) consent must deny until regenerated")
	}
	// Regenerating gives it a readable value again.
	if v, err := s.GenerateDomainConsent("h"); err != nil || v == "" {
		t.Fatalf("regenerate after migration: v=%q err=%v", v, err)
	}
}
