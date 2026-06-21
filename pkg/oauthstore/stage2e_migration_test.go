package oauthstore

import (
	"testing"
	"time"
)

// B-71 Stage 2e: retiring the static keys from the active config surface must NOT strand a
// deployment that has not yet seeded. The one-time, marker-guarded migration still consumes the
// (now-deprecated) keys on first start, so a not-yet-seeded upgrade ends up with its trusted
// domains + per-domain consent in the store — the verifier/consent paths then read the store.
//
// RED proof: break the one-time migration (SeedDomainRegistrationsFromDomains stops consuming the
// domains, or cmd/shoka stops calling it) and an unseeded upgrade has no trusted domains —
// TrustedDomain is false below — so this fails. This is the hazard Option A avoids.
func TestStage2e_MigrationNotStranded(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	// Precondition: a fresh (unseeded) store trusts no domain yet.
	if s.TrustedDomain("connector.example") {
		t.Fatal("a fresh store must trust no domain before the migration seed")
	}

	// The one-time migration from the deprecated keys (as cmd/shoka runs at startup when the
	// marker registrations_seeded_v1 is unset).
	const consent = "the-deprecated-global-consent"
	if err := s.SeedDomainRegistrationsFromDomains([]string{"connector.example"}, consent, time.Hour, 90*24*time.Hour, now); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// After migration the domain (and its subdomains) is trusted via the store, and carries its
	// per-domain consent — so existing connections keep working without the config keys.
	if !s.TrustedDomain("connector.example") || !s.TrustedDomain("sub.connector.example") {
		t.Fatal("after the migration the domain (and its subdomains) must be trusted via the store")
	}
	entry, ok := s.DomainEntryForHost("connector.example")
	if !ok || !entry.VerifyConsent(consent) {
		t.Fatalf("the migrated domain must carry the per-domain consent (ok=%v)", ok)
	}

	// Idempotent: a later start (marker set) does not re-consume the keys or duplicate the entry.
	if err := s.SeedDomainRegistrationsFromDomains([]string{"connector.example"}, consent, time.Hour, 90*24*time.Hour, now); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	regs, _ := s.ListRegistrations()
	n := 0
	for _, e := range regs {
		if e.RegistrationMode == RegistrationModeDomain && e.Identifier == "connector.example" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("migration must be once-only; got %d entries for the domain", n)
	}
}
