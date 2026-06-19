package oauthstore

import (
	"encoding/json"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// B-71 Stage 1: the registration_mode axis + dynamic registration-entry store.

// TestRegistrations_CRUDPerEntryMode: create/list/get/update/delete entries, and the
// round-trip preserves registration_mode PER ENTRY — a "dcr" entry and a "confidential"
// entry coexist with distinct modes.
func TestRegistrations_CRUDPerEntryMode(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	dcr, err := s.CreateRegistration(RegistrationModeDCR, "connector.example", now)
	if err != nil {
		t.Fatalf("create dcr: %v", err)
	}
	conf, err := s.CreateRegistration(RegistrationModeConfidential, "cli-integration", now)
	if err != nil {
		t.Fatalf("create confidential: %v", err)
	}
	if dcr.ID == "" || dcr.ID == conf.ID {
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
	if modeByID[dcr.ID] != RegistrationModeDCR || modeByID[conf.ID] != RegistrationModeConfidential {
		t.Fatalf("per-entry modes not preserved: %v", modeByID)
	}

	// Get round-trips fields.
	got, err := s.GetRegistration(dcr.ID)
	if err != nil || got.Identifier != "connector.example" || got.RegistrationMode != RegistrationModeDCR {
		t.Fatalf("get dcr: %+v err=%v", got, err)
	}
	if _, err := s.GetRegistration("no-such-id"); err != ErrNotFound {
		t.Fatalf("get unknown: err=%v, want ErrNotFound", err)
	}

	// Update changes the identifier; mode stays per-entry.
	got.Identifier = "renamed.example"
	if err := s.UpdateRegistration(got); err != nil {
		t.Fatalf("update: %v", err)
	}
	reread, _ := s.GetRegistration(dcr.ID)
	if reread.Identifier != "renamed.example" || reread.RegistrationMode != RegistrationModeDCR {
		t.Fatalf("update not persisted: %+v", reread)
	}

	// Delete removes only that entry (idempotent).
	if err := s.DeleteRegistration(dcr.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.DeleteRegistration(dcr.ID); err != nil {
		t.Fatalf("delete is idempotent: %v", err)
	}
	if _, err := s.GetRegistration(dcr.ID); err != ErrNotFound {
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
	if _, err := s.CreateRegistration(RegistrationModeDCR, "  ", now); err == nil {
		t.Fatal("an empty identifier must be rejected")
	}
	infos, _ := s.ListRegistrations()
	if len(infos) != 0 {
		t.Fatalf("a rejected create must write nothing; got %d entries", len(infos))
	}

	// Update on a non-existent id: ErrNotFound, nothing written.
	err := s.UpdateRegistration(RegistrationEntry{ID: "ghost", RegistrationMode: RegistrationModeDCR, Identifier: "d", CreatedAt: now})
	if err != ErrNotFound {
		t.Fatalf("update unknown id: err=%v, want ErrNotFound", err)
	}
	infos, _ = s.ListRegistrations()
	if len(infos) != 0 {
		t.Fatalf("a failed update must write nothing; got %d entries", len(infos))
	}
}

// TestRegistrations_LenientDecodeReservedFields proves later stages can add/use the
// reserved TTL/Consent/Secret fields (and any field) without a breaking migration:
//   - a minimal record (no reserved fields) decodes with them nil;
//   - a record carrying populated reserved fields round-trips them verbatim;
//   - a "future" record with an UNKNOWN extra key still decodes (lenient), core intact.
//
// RED proof: make GetRegistration's decode strict (DisallowUnknownFields) → the future
// record fails to decode → this test fails.
func TestRegistrations_LenientDecodeReservedFields(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	// Minimal record (no reserved fields) → reserved decode to nil.
	e, err := s.CreateRegistration(RegistrationModeDCR, "d.example", now)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, _ := s.GetRegistration(e.ID)
	if got.TTL != nil || got.Consent != nil || got.Secret != nil {
		t.Fatalf("reserved fields must decode to nil when absent: %+v", got)
	}

	// Populated reserved fields round-trip verbatim (Stage 2/3 will set them).
	got.TTL = json.RawMessage(`{"access_seconds":3600}`)
	got.Consent = json.RawMessage(`{"required":true}`)
	got.Secret = json.RawMessage(`{"hash":"deadbeef"}`)
	if err := s.UpdateRegistration(got); err != nil {
		t.Fatalf("update with reserved fields: %v", err)
	}
	reread, _ := s.GetRegistration(e.ID)
	if string(reread.TTL) != `{"access_seconds":3600}` || string(reread.Consent) != `{"required":true}` || string(reread.Secret) != `{"hash":"deadbeef"}` {
		t.Fatalf("reserved fields not preserved: %+v", reread)
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

// TestRegistrations_SeedIdempotent: seeding dcr entries from a static domain list creates
// one "dcr" entry per domain, is idempotent (a re-run adds nothing), and is safe on an
// empty list. RED proof: drop the marker guard in SeedDCRRegistrationsFromDomains → a
// second run duplicates the entries → this test fails.
func TestRegistrations_SeedIdempotent(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	if err := s.SeedDCRRegistrationsFromDomains([]string{"a.example", "b.example", "  "}, now); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first, _ := s.ListRegistrations()
	if len(first) != 2 {
		t.Fatalf("seed must create one dcr entry per non-empty domain; got %d", len(first))
	}
	for _, e := range first {
		if e.RegistrationMode != RegistrationModeDCR {
			t.Fatalf("seeded entries must be dcr mode: %+v", e)
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
	if err := s.SeedDCRRegistrationsFromDomains([]string{"a.example", "c.example"}, now); err != nil {
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
	if err := s.SeedDCRRegistrationsFromDomains(nil, now); err != nil {
		t.Fatalf("seed empty: %v", err)
	}
	infos, _ := s.ListRegistrations()
	if len(infos) != 0 {
		t.Fatalf("empty-config seed must create no entries; got %d", len(infos))
	}
	// Marker set: a later call is still a no-op (the static config remains the source until Stage 2).
	if err := s.SeedDCRRegistrationsFromDomains([]string{"late.example"}, now); err != nil {
		t.Fatalf("seed after empty: %v", err)
	}
	infos, _ = s.ListRegistrations()
	if len(infos) != 0 {
		t.Fatalf("seed is once-only; got %d entries after a second call", len(infos))
	}
}
