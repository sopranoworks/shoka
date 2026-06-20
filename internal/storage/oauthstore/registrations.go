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
	ID               string        `json:"id"`
	RegistrationMode string        `json:"registration_mode"` // "domain" | "confidential"
	Identifier       string        `json:"identifier"`        // domain mode: the trusted domain; confidential: the issued client_id
	CreatedAt        time.Time     `json:"created_at"`
	TTL              *EntryTTL     `json:"ttl,omitempty"`     // B-71 Stage 2b: per-domain access/refresh TTL
	Consent          *EntryConsent `json:"consent,omitempty"` // B-71 Stage 2b: per-domain consent (HASHED)
	// B-71 Stage 3 — confidential-client material. Secret holds the HASHED client secret (never
	// raw, Stage 0 discipline); Scope is the pre-issued authorization grant that drives the
	// tools/call namespace gate; ExpiresAt is the credential's FINITE validity (no indefinite —
	// /authorize and /token reject the credential after it). All omitempty, so a domain entry (or
	// a pre-Stage-3 record) decodes with them nil/zero — no breaking migration.
	Secret    *EntrySecret `json:"secret,omitempty"`
	Scope     string       `json:"scope,omitempty"`
	ExpiresAt time.Time    `json:"expires_at,omitempty"`
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

// EntrySecret is a confidential entry's client secret (B-71 Stage 3). Only the HASH is stored
// (Stage 0 discipline — hashHandle/SHA-256); the raw secret is shown to the operator once at
// issuance and never persisted or returned again.
type EntrySecret struct {
	Hash string `json:"hash,omitempty"` // hashHandle(secret); NEVER the raw secret
}

// SetSecret stores a confidential entry's client secret HASHED (hashHandle; Stage 0). An empty
// secret clears it. The raw value is never persisted.
func (e *RegistrationEntry) SetSecret(secret string) {
	if secret == "" {
		e.Secret = nil
		return
	}
	e.Secret = &EntrySecret{Hash: hashHandle(secret)}
}

// VerifySecret reports whether presented matches the entry's stored client-secret hash
// (constant-time). An entry with no secret, or an empty presented value, never verifies.
func (e RegistrationEntry) VerifySecret(presented string) bool {
	if e.Secret == nil || e.Secret.Hash == "" || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(hashHandle(presented)), []byte(e.Secret.Hash)) == 1
}

// CredentialExpired reports whether a confidential entry's finite credential validity has passed
// (B-71 Stage 3, no-indefinite). A zero ExpiresAt is treated as expired (a confidential entry must
// always carry a finite expiry — IssueConfidentialClient guarantees it), so a malformed entry
// fails closed.
func (e RegistrationEntry) CredentialExpired(now time.Time) bool {
	return e.ExpiresAt.IsZero() || !now.Before(e.ExpiresAt)
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

// IssueConfidentialClient mints a confidential ("confidential"-mode) registration entry (B-71
// Stage 3): a fresh opaque client_id (the Identifier) + a high-entropy client secret. Only the
// secret HASH is stored (Stage 0); the RAW secret is returned ONCE for the operator to copy and
// is never persisted or retrievable again. scope is the pre-issued authorization grant (drives
// the tools/call gate); validity is the credential's FINITE lifetime (must be > 0 — no
// indefinite). Persisted in one atomic transaction.
func (s *Store) IssueConfidentialClient(scope string, validity time.Duration, now time.Time) (entry RegistrationEntry, rawSecret string, err error) {
	if validity <= 0 {
		return RegistrationEntry{}, "", fmt.Errorf("oauthstore: confidential client validity must be positive (no indefinite)")
	}
	id, err := NewHandle()
	if err != nil {
		return RegistrationEntry{}, "", err
	}
	clientID, err := NewHandle()
	if err != nil {
		return RegistrationEntry{}, "", err
	}
	rawSecret, err = NewHandle()
	if err != nil {
		return RegistrationEntry{}, "", err
	}
	entry = RegistrationEntry{
		ID:               id,
		RegistrationMode: RegistrationModeConfidential,
		Identifier:       clientID,
		CreatedAt:        now.UTC(),
		Scope:            strings.TrimSpace(scope),
		ExpiresAt:        now.UTC().Add(validity),
	}
	entry.SetSecret(rawSecret)
	val, err := json.Marshal(entry)
	if err != nil {
		return RegistrationEntry{}, "", fmt.Errorf("oauthstore: encode confidential entry: %w", err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(registrationsBucket)).Put([]byte(id), val)
	}); err != nil {
		return RegistrationEntry{}, "", err
	}
	return entry, rawSecret, nil
}

// ConfidentialClient resolves an issued client_id to its confidential registration entry (B-71
// Stage 3) — the /authorize resolution + /token authentication seam. Returns false if no
// confidential entry carries that Identifier. (A bucket scan; the confidential set is small —
// operator-issued credentials, not per-connection.)
func (s *Store) ConfidentialClient(clientID string) (RegistrationEntry, bool) {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return RegistrationEntry{}, false
	}
	var found RegistrationEntry
	ok := false
	_ = s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(registrationsBucket))
		return b.ForEach(func(_, v []byte) error {
			var e RegistrationEntry
			if json.Unmarshal(v, &e) != nil {
				return nil
			}
			if e.RegistrationMode == RegistrationModeConfidential && e.Identifier == clientID {
				found, ok = e, true
			}
			return nil
		})
	})
	return found, ok
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

// SeedDomainRegistrationsFromDomains is the one-time bridge from the static config to the
// dynamic store (B-71 Stage 2c wires it at startup): it creates a "domain"-mode entry per
// trusted_client_metadata_domain so the CIMD verifier / DCR gate / consent / TTL paths can
// read the dynamic store with the entries already present and behaviour is preserved:
//   - CONSENT CARRY: each seeded domain inherits the single global consentSecret as its
//     per-domain consent (hashed via SetConsent) — so a connection that satisfied the global
//     consent still satisfies its domain's consent after the switch. An empty consentSecret
//     leaves no per-domain consent (the live path then falls back to the global, also empty).
//   - TTL SEED: each seeded domain gets per-domain access/refresh TTL = the current finite
//     global defaults, so the issued lifetime is identical until the operator edits a domain.
//
// Idempotent + crash-safe: guarded by metaBucket["registrations_seeded_v1"] in ONE atomic
// transaction — a re-run is a no-op, an empty list is safe (the marker is still set).
func (s *Store) SeedDomainRegistrationsFromDomains(domains []string, consentSecret string, accessTTL, refreshTTL time.Duration, now time.Time) error {
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
			entry.SetConsent(consentSecret) // carry the global consent (hashed); "" clears it
			if accessTTL > 0 || refreshTTL > 0 {
				entry.TTL = &EntryTTL{
					AccessSeconds:  int64(accessTTL / time.Second),
					RefreshSeconds: int64(refreshTTL / time.Second),
				}
			}
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
