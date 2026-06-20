package oauthstore

import (
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// B-71 Stage 1: the registration_mode axis + dynamic registration-entry store.

// TestRegistrations_CRUDPerEntryMode: create/list/get/update/delete entries, and the
// round-trip preserves registration_mode PER ENTRY — a "domain" entry and a "confidential"
// entry coexist with distinct modes.
func TestRegistrations_CRUDPerEntryMode(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	dom, err := s.CreateRegistration(RegistrationModeDomain, "connector.example", now)
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	conf, err := s.CreateRegistration(RegistrationModeConfidential, "cli-integration", now)
	if err != nil {
		t.Fatalf("create confidential: %v", err)
	}
	if dom.ID == "" || dom.ID == conf.ID {
		t.Fatal("each entry must get a distinct opaque id")
	}

	// List carries both, each with its own mode.
	modeByID := map[string]string{}
	infos, err := s.ListRegistrations()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, e := range infos {
		modeByID[e.ID] = e.RegistrationMode
	}
	if modeByID[dom.ID] != RegistrationModeDomain || modeByID[conf.ID] != RegistrationModeConfidential {
		t.Fatalf("per-entry modes not preserved: %v", modeByID)
	}

	// Get round-trips fields.
	got, err := s.GetRegistration(dom.ID)
	if err != nil || got.Identifier != "connector.example" || got.RegistrationMode != RegistrationModeDomain {
		t.Fatalf("get domain: %+v err=%v", got, err)
	}
	if _, err := s.GetRegistration("no-such-id"); err != ErrNotFound {
		t.Fatalf("get unknown: err=%v, want ErrNotFound", err)
	}

	// Update changes the identifier; mode stays per-entry.
	got.Identifier = "renamed.example"
	if err := s.UpdateRegistration(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	reread, _ := s.GetRegistration(dom.ID)
	if reread.Identifier != "renamed.example" || reread.RegistrationMode != RegistrationModeDomain {
		t.Fatalf("update not persisted: %+v", reread)
	}

	// Delete removes only that entry (idempotent).
	if err := s.DeleteRegistration(dom.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteRegistration(dom.ID); err != nil {
		t.Fatalf("delete is idempotent: %v", err)
	}
	if _, err := s.GetRegistration(dom.ID); err != ErrNotFound {
		t.Fatalf("deleted entry must be gone: err=%v", err)
	}
	if _, err := s.GetRegistration(conf.ID); err != nil {
		t.Fatalf("the other entry must survive: %v", err)
	}
}

// TestRegistrations_InvalidModeRejectedNoWrite: an invalid mode is rejected and writes
// nothing (atomic-tx discipline — no partial entry). Update on an unknown id is ErrNotFound
// and also writes nothing.
func TestRegistrations_InvalidModeRejectedNoWrite(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	if _, err := s.CreateRegistration("bogus-mode", "x", now); err == nil {
		t.Fatal("an invalid registration_mode must be rejected")
	}
	// "dcr" is the registration PROCEDURE, NOT a management-axis value (B-71 Stage 1
	// value-set correction): it must be rejected. The axis values are "domain"/"confidential"
	// (both accepted — proven in TestRegistrations_CRUDPerEntryMode). RED proof: leave the
	// validation accepting "dcr" and this assertion fails.
	if _, err := s.CreateRegistration("dcr", "connector.example", now); err == nil {
		t.Fatal(`"dcr" is a procedure, not a management-axis mode — CreateRegistration("dcr") must be rejected`)
	}
	if _, err := s.CreateRegistration(RegistrationModeDomain, "  ", now); err == nil {
		t.Fatal("an empty identifier must be rejected")
	}
	infos, _ := s.ListRegistrations()
	if len(infos) != 0 {
		t.Fatalf("a rejected create must write nothing; got %d entries", len(infos))
	}

	// Update on a non-existent id: ErrNotFound, nothing written.
	err := s.UpdateRegistration(RegistrationEntry{ID: "ghost", RegistrationMode: RegistrationModeDomain, Identifier: "d", CreatedAt: now})
	if err != ErrNotFound {
		t.Fatalf("update unknown id: err=%v, want ErrNotFound", err)
	}
	infos, _ = s.ListRegistrations()
	if len(infos) != 0 {
		t.Fatalf("a failed update must write nothing; got %d entries", len(infos))
	}
}

// TestRegistrations_LenientDecodeReservedFields proves the TTL/Consent fields (now typed,
// B-71 Stage 2b) and the still-reserved Secret field round-trip, an entry without them
// decodes with them nil (decode-safe — Stage 1/2a vintage), and a "future" record with an
// UNKNOWN extra key still decodes (lenient), core intact.
//
// RED proof: make GetRegistration's decode strict (DisallowUnknownFields) → the future
// record fails to decode → this test fails.
func TestRegistrations_LenientDecodeReservedFields(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	// Minimal record (no TTL/Consent/Secret) → they decode to nil.
	e, err := s.CreateRegistration(RegistrationModeDomain, "d.example", now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := s.GetRegistration(e.ID)
	if got.TTL != nil || got.Consent != nil || got.Secret != nil {
		t.Fatalf("TTL/Consent/Secret must decode to nil when absent: %+v", got)
	}

	// Populated fields round-trip (TTL/Consent/Secret all typed since B-71 Stage 3).
	got.TTL = &EntryTTL{AccessSeconds: 3600, RefreshSeconds: 7_776_000}
	got.SetConsent("the-domain-consent-secret")
	got.SetSecret("the-confidential-client-secret")
	if err := s.UpdateRegistration(got); err != nil {
		t.Fatalf("update with typed fields: %v", err)
	}
	reread, _ := s.GetRegistration(e.ID)
	if reread.TTL == nil || reread.TTL.AccessSeconds != 3600 || reread.TTL.RefreshSeconds != 7_776_000 {
		t.Fatalf("TTL not preserved: %+v", reread.TTL)
	}
	if reread.Consent == nil || reread.Consent.Hash == "" {
		t.Fatalf("Consent hash not preserved: %+v", reread.Consent)
	}
	// The secret round-trips as a HASH only (never the raw value) and verifies constant-time.
	if reread.Secret == nil || reread.Secret.Hash == "" {
		t.Fatalf("Secret hash not preserved: %+v", reread.Secret)
	}
	if strings.Contains(reread.Secret.Hash, "the-confidential-client-secret") {
		t.Fatalf("the raw secret must never appear at rest")
	}
	if !reread.VerifySecret("the-confidential-client-secret") || reread.VerifySecret("wrong") {
		t.Fatalf("VerifySecret must accept the secret and reject a wrong one")
	}

	// A "future" record with an unknown key, written directly, still decodes (forward-compat).
	futureJSON := []byte(`{"id":"future-id","registration_mode":"confidential","identifier":"fc","created_at":"2026-06-20T00:00:00Z","a_stage9_field":{"x":1}}`)
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(registrationsBucket)).Put([]byte("future-id"), futureJSON)
	}); err != nil {
		t.Fatalf("seed future record: %v", err)
	}
	fut, err := s.GetRegistration("future-id")
	if err != nil {
		t.Fatalf("a record with an unknown field must still decode (lenient): %v", err)
	}
	if fut.RegistrationMode != RegistrationModeConfidential || fut.Identifier != "fc" {
		t.Fatalf("forward-compat decode lost core fields: %+v", fut)
	}
}

// TestRegistrations_SeedIdempotent: seeding domain entries from a static domain list creates
// one "domain"-mode entry per domain, is idempotent (a re-run adds nothing), and is safe on
// an empty list. RED proof: drop the marker guard in SeedDomainRegistrationsFromDomains → a
// second run duplicates the entries → this test fails.
func TestRegistrations_SeedIdempotent(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	const consent = "the-global-consent-secret"
	if err := s.SeedDomainRegistrationsFromDomains([]string{"a.example", "b.example", "  "}, consent, time.Hour, 90*24*time.Hour, now); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first, _ := s.ListRegistrations()
	if len(first) != 2 {
		t.Fatalf("seed must create one domain entry per non-empty domain; got %d", len(first))
	}
	for _, e := range first {
		if e.RegistrationMode != RegistrationModeDomain {
			t.Fatalf("seeded entries must be domain mode: %+v", e)
		}
		// Consent carry: each seeded domain inherits the global consent (hashed) and verifies it.
		if !e.VerifyConsent(consent) {
			t.Fatalf("seeded domain %q must carry the global consent (hashed)", e.Identifier)
		}
		// TTL seed: each seeded domain carries the global finite defaults.
		if e.TTL == nil || e.TTL.AccessSeconds != 3600 || e.TTL.RefreshSeconds != int64((90*24*time.Hour)/time.Second) {
			t.Fatalf("seeded domain %q must carry the global TTL defaults: %+v", e.Identifier, e.TTL)
		}
	}
	ids := map[string]bool{}
	for _, e := range first {
		ids[e.Identifier] = true
	}
	if !ids["a.example"] || !ids["b.example"] {
		t.Fatalf("seeded identifiers wrong: %v", ids)
	}

	// Idempotent: a re-run (even with different domains) adds nothing — the marker guards it.
	if err := s.SeedDomainRegistrationsFromDomains([]string{"a.example", "c.example"}, consent, time.Hour, 90*24*time.Hour, now); err != nil {
		t.Fatalf("seed rerun: %v", err)
	}
	second, _ := s.ListRegistrations()
	if len(second) != 2 {
		t.Fatalf("a re-run must not duplicate/add entries; got %d, want 2", len(second))
	}
}

// TestRegistrations_SeedEmptyConfigSafe: seeding an empty domain list is a no-op that still
// marks the store seeded (so it stays a no-op), with no entries created.
func TestRegistrations_SeedEmptyConfigSafe(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	if err := s.SeedDomainRegistrationsFromDomains(nil, "", time.Hour, 90*24*time.Hour, now); err != nil {
		t.Fatalf("seed empty: %v", err)
	}
	infos, _ := s.ListRegistrations()
	if len(infos) != 0 {
		t.Fatalf("empty-config seed must create no entries; got %d", len(infos))
	}
	// Marker set: a later call is still a no-op (the static config remains the source until Stage 2).
	if err := s.SeedDomainRegistrationsFromDomains([]string{"late.example"}, "", time.Hour, 90*24*time.Hour, now); err != nil {
		t.Fatalf("seed after empty: %v", err)
	}
	infos, _ = s.ListRegistrations()
	if len(infos) != 0 {
		t.Fatalf("seed is once-only; got %d entries after a second call", len(infos))
	}
}
