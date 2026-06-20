package oauthstore

import (
	"crypto/subtle"
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
	TTL              *EntryTTL       `json:"ttl,omitempty"`     // B-71 Stage 2b: per-domain access/refresh TTL
	Consent          *EntryConsent   `json:"consent,omitempty"` // B-71 Stage 2b: per-domain consent (HASHED)
	Secret           json.RawMessage `json:"secret,omitempty"`  // RESERVED — Stage 3 confidential material (hashed)
}

// EntryTTL is a "domain" entry's per-domain token lifetime (B-71 Stage 2b), in whole
// seconds (JSON-friendly, decoupled from config's Duration). A zero/absent field means
// "unset" — EffectiveTTL falls back to the global finite default, never to infinity (Stage
// 5's no-indefinite rule). The code TTL stays global this stage.
type EntryTTL struct {
	AccessSeconds  int64 `json:"access_seconds,omitempty"`
	RefreshSeconds int64 `json:"refresh_seconds,omitempty"`
}

// EntryConsent is a "domain" entry's per-domain consent (B-71 Stage 2b). Only the HASH of
// the consent secret is stored (Stage 0 discipline — hashHandle/SHA-256); the raw value is
// never persisted. It is presented at consent time and compared by hash (constant-time).
type EntryConsent struct {
	Hash string `json:"hash,omitempty"` // hashHandle(secret); NEVER the raw consent value
}

// EffectiveTTL resolves a domain entry's per-domain access/refresh TTL against the global
// finite defaults (B-71 Stage 2b; reuses Stage 5's finite-floor): a positive per-domain
// value wins; a 0/unset/negative per-domain value falls back to the global default; the
// result is NEVER non-positive (no-indefinite). Pure — no runtime caller this stage (2c
// wires it). The defaults passed in are assumed finite (the config layer guarantees them).
func (e RegistrationEntry) EffectiveTTL(defaultAccess, defaultRefresh time.Duration) (access, refresh time.Duration) {
	access, refresh = defaultAccess, defaultRefresh
	if e.TTL != nil {
		// The floor: a positive per-domain value overrides; a 0/unset/negative one falls back
		// to the finite default — so the result is never the per-domain 0 (immediate) nor
		// infinite. Defaults are assumed finite (the config layer guarantees them).
		if d := time.Duration(e.TTL.AccessSeconds) * time.Second; d > 0 {
			access = d
		}
		if d := time.Duration(e.TTL.RefreshSeconds) * time.Second; d > 0 {
			refresh = d
		}
	}
	return access, refresh
}

// SetConsent sets a domain entry's per-domain consent secret, stored HASHED (hashHandle;
// Stage 0). An empty secret CLEARS the per-domain consent. The raw value is never persisted.
func (e *RegistrationEntry) SetConsent(secret string) {
	if secret == "" {
		e.Consent = nil
		return
	}
	e.Consent = &EntryConsent{Hash: hashHandle(secret)}
}

// VerifyConsent reports whether presented matches the entry's stored per-domain consent
// hash (constant-time). NO-CONSENT POLICY (explicit, never silently allow): an entry with no
// per-domain consent set returns FALSE for any presented value — this helper grants ONLY on
// an explicit, matching per-domain consent. Whether a domain WITHOUT per-domain consent
// should instead inherit the global consent or require none is the live-path fallback Stage
// 2c decides; this helper never grants against an unset consent. An empty presented value
// never verifies.
func (e RegistrationEntry) VerifyConsent(presented string) bool {
	if e.Consent == nil || e.Consent.Hash == "" {
		return false
	}
	if presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashHandle(presented)), []byte(e.Consent.Hash)) == 1
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
