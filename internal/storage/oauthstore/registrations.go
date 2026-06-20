package oauthstore

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// B-71 Stage 1 (the spine): registration_mode is a FIRST-CLASS, PER-ENTRY MANAGEMENT axis
// — DOMAIN-based vs CONFIDENTIAL — driving how the OAuth management screen composes. It is
// the management AXIS, NOT the registration PROCEDURE: cimd vs dcr (HOW a client registers)
// is only a discovery posture / procedure and is a per-series sub-detail (Stage 2,
// derivable via isDCRClientID), never a value here. The two axis values:
const (
	// RegistrationModeDomain — a trusted-DOMAIN entry: it configures the COMMON
	// {domain, TTL, consent} for that domain, under which BOTH CIMD- and DCR-registered
	// clients/tokens are managed (per-domain management + TTL + consent land in Stage 2;
	// tokens group under their domain).
	RegistrationModeDomain = "domain"
	// RegistrationModeConfidential — pre-issued / confidential client (Client ID + Secret;
	// the secret + /token auth land in Stage 3).
	RegistrationModeConfidential = "confidential"
)

// migrationKeyRegistrationsSeeded marks that the one-time bridge seed (dcr entries from the
// static trusted_client_metadata_domains) has run. Lives in metaBucket, making the seed
// idempotent — exactly the Stage 0 marker pattern.
const migrationKeyRegistrationsSeeded = "registrations_seeded_v1"

// RegistrationEntry is one dynamic OAuth registration entry (B-71 Stage 1). Its
// RegistrationMode is the per-entry management axis ("domain" | "confidential"). Stage 1
// only persists and CRUDs entries — it attaches NO runtime behaviour (verification/consent/
// token issuance are unchanged; nothing reads this store yet).
//
// TTL / Consent / Secret are RESERVED for later stages (Stage 2 attaches per-entry TTL +
// consent; Stage 3 attaches confidential-client material — a HASHED secret, never raw, per
// Stage 0). They are json.RawMessage + omitempty, so an entry written without them decodes
// to nil and later stages can give them concrete shapes without a breaking migration — the
// Scope-field precedent (absent JSON key ⇒ zero value; unknown keys are ignored by the
// lenient decoder).
type RegistrationEntry struct {
	ID               string          `json:"id"`
	RegistrationMode string          `json:"registration_mode"` // "dcr" | "confidential"
	Identifier       string          `json:"identifier"`        // domain mode: the trusted domain; confidential: the client identifier
	CreatedAt        time.Time       `json:"created_at"`
	TTL              json.RawMessage `json:"ttl,omitempty"`     // RESERVED — Stage 2 per-entry token lifetimes
	Consent          json.RawMessage `json:"consent,omitempty"` // RESERVED — Stage 2 per-entry consent
	Secret           json.RawMessage `json:"secret,omitempty"`  // RESERVED — Stage 3 confidential material (hashed)
}

func validRegistrationMode(m string) bool {
	return m == RegistrationModeDomain || m == RegistrationModeConfidential
}

// CreateRegistration mints a new entry (opaque random ID) for the given per-entry mode and
// identifier, persists it in one atomic transaction, and returns it. It errors on an
// invalid mode or an empty identifier.
func (s *Store) CreateRegistration(mode, identifier string, now time.Time) (RegistrationEntry, error) {
	if !validRegistrationMode(mode) {
		return RegistrationEntry{}, fmt.Errorf("oauthstore: invalid registration_mode %q", mode)
	}
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return RegistrationEntry{}, fmt.Errorf("oauthstore: registration identifier required")
	}
	id, err := NewHandle()
	if err != nil {
		return RegistrationEntry{}, err
	}
	entry := RegistrationEntry{ID: id, RegistrationMode: mode, Identifier: identifier, CreatedAt: now.UTC()}
	val, err := json.Marshal(entry)
	if err != nil {
		return RegistrationEntry{}, fmt.Errorf("oauthstore: encode registration: %w", err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(registrationsBucket)).Put([]byte(id), val)
	}); err != nil {
		return RegistrationEntry{}, err
	}
	return entry, nil
}

// ListRegistrations returns every entry (bbolt key order = the opaque id).
func (s *Store) ListRegistrations() ([]RegistrationEntry, error) {
	var out []RegistrationEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(registrationsBucket)).ForEach(func(_, v []byte) error {
			var e RegistrationEntry
			if err := json.Unmarshal(v, &e); err != nil {
				return fmt.Errorf("oauthstore: decode registration: %w", err)
			}
			out = append(out, e)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetRegistration returns one entry by id, or ErrNotFound. Decode is lenient (unknown JSON
// keys are ignored, absent ones are zero), so a record written by a later stage with extra
// fields still reads back here.
func (s *Store) GetRegistration(id string) (RegistrationEntry, error) {
	var e RegistrationEntry
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(registrationsBucket)).Get([]byte(id))
		if v == nil {
			return ErrNotFound
		}
		return json.Unmarshal(v, &e)
	})
	if err != nil {
		return RegistrationEntry{}, err
	}
	return e, nil
}

// UpdateRegistration overwrites an existing entry (matched by ID) in one atomic
// transaction. ErrNotFound if the ID does not exist (and nothing is written); the mode must
// be valid and the identifier non-empty. Reserved TTL/Consent/Secret fields are persisted
// verbatim (later stages set them).
func (s *Store) UpdateRegistration(entry RegistrationEntry) error {
	if !validRegistrationMode(entry.RegistrationMode) {
		return fmt.Errorf("oauthstore: invalid registration_mode %q", entry.RegistrationMode)
	}
	if strings.TrimSpace(entry.Identifier) == "" {
		return fmt.Errorf("oauthstore: registration identifier required")
	}
	val, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("oauthstore: encode registration: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(registrationsBucket))
		if b.Get([]byte(entry.ID)) == nil {
			return ErrNotFound
		}
		return b.Put([]byte(entry.ID), val)
	})
}

// DeleteRegistration removes an entry by id. Idempotent: deleting an unknown id is a no-op
// (nil), like Revoke.
func (s *Store) DeleteRegistration(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(registrationsBucket)).Delete([]byte(id))
	})
}

// SeedDomainRegistrationsFromDomains is the one-time bridge from the static
// trusted_client_metadata_domains to the dynamic store (B-71 Stage 1): it creates a
// "domain"-mode entry per domain (those values ARE trusted domains) so Stage 2 can flip the
// CIMD verifier to read the dynamic store with the entries already present. Idempotent +
// crash-safe: guarded by a marker in metaBucket and run in ONE atomic transaction, so a
// re-run is a no-op and an empty list is safe (the marker is still set, so a later non-empty
// call does not retro-seed).
//
// Stage 1 does NOT call this at runtime — no consumer reads the registrations store yet, so
// runtime behaviour is UNCHANGED. It exists and is tested here so Stage 2 (which flips the
// verifier onto the dynamic store) wires the call alongside its first reader.
func (s *Store) SeedDomainRegistrationsFromDomains(domains []string, now time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		mb := tx.Bucket([]byte(metaBucket))
		if mb.Get([]byte(migrationKeyRegistrationsSeeded)) != nil {
			return nil // already seeded — never duplicate
		}
		rb := tx.Bucket([]byte(registrationsBucket))
		for _, d := range domains {
			d = strings.TrimSpace(d)
			if d == "" {
				continue
			}
			id, err := NewHandle()
			if err != nil {
				return err
			}
			entry := RegistrationEntry{ID: id, RegistrationMode: RegistrationModeDomain, Identifier: d, CreatedAt: now.UTC()}
			val, err := json.Marshal(entry)
			if err != nil {
				return fmt.Errorf("oauthstore: encode registration: %w", err)
			}
			if err := rb.Put([]byte(id), val); err != nil {
				return err
			}
		}
		return mb.Put([]byte(migrationKeyRegistrationsSeeded), []byte("1"))
	})
}
