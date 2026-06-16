// Package userstore is the durable, go-git-free store for Shoka's multi-user
// authentication (the B-28 multi-user foundation, stage 1: user DB + login +
// first-run). It holds the user accounts and their live web sessions — server-level
// identity state that is NOT versioned project content — so, like the OAuth store
// (internal/storage/oauthstore), it must never go through the go-git storage layer
// (Architectural Anchor 1). It is a sibling of the per-project catalog and the OAuth
// store, reusing the same embedded DB technology (bbolt) at a single global database
// <base_dir>/users.db.
//
// Bucket layout (bbolt buckets are flat/top-level):
//
//	"users"    email          -> JSON UserRecord     (the account; email = PK = git author)
//	"sessions" session handle -> JSON SessionRecord  (one logged-in browser session)
//
// Records are JSON-encoded, so a field added in a later stage decodes an older
// record with its zero value — the store evolves with NO migration (the oauthstore
// SeriesRecord.Scope lesson). The first-admin create and the session create re-check
// their precondition INSIDE the write transaction (the oauthstore "collect/decide in
// the tx" discipline) so two concurrent first registrants cannot both become admin.
//
// The store deliberately carries only authentication state. Authorization is the
// user's Scope (the same grant grammar as auth.Principal.Scope / oauthstore); the
// first user is created "*:admin". The role-aware enforcement sweep is a later stage
// — stage 1 stores and transports the scope and binds it to the session principal.
package userstore

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	bolt "go.etcd.io/bbolt"
)

// AdminScope is the authorization grant the first registrant receives: wildcard
// admin (manage users + all namespaces R/W). It is stored on the user and carried
// onto the session principal; the role-aware authorize() that parses the ":admin"
// suffix is a later stage (stage 1 transports it, the enforcement sweep consumes it).
const AdminScope = "*:admin"

// Errors returned by the package.
var (
	// ErrNotFound is returned when a user or session handle does not exist.
	ErrNotFound = errors.New("userstore: not found")
	// ErrExists is returned by CreateUser when the email is already registered.
	ErrExists = errors.New("userstore: user already exists")
	// ErrUsersExist is returned by CreateFirstAdmin when any user already exists —
	// the first-run window is closed. The in-tx re-check makes this race-safe.
	ErrUsersExist = errors.New("userstore: users already exist")
	// ErrExpired is returned by LookupSession when the session is past its expiry.
	ErrExpired = errors.New("userstore: expired")
)

const (
	usersBucket    = "users"
	sessionsBucket = "sessions"
)

// UserRecord is one account. Email is the primary key AND the git Author identity
// (the hard invariant email = account = git author). Only PUBLIC WebAuthn material
// is stored (the go-webauthn Credential carries the credential id, public key, sign
// count and transports — never a private key). The TOTP secret is stored ENCRYPTED
// (TOTPSecretEnc); the plaintext base32 secret never rests on disk.
type UserRecord struct {
	Email         string                `json:"email"`
	DisplayName   string                `json:"display_name"`
	Handle        []byte                `json:"handle"`                    // opaque WebAuthn user handle (random, not PII)
	PasswordHash  string                `json:"password_hash"`             // argon2id PHC string (the required floor)
	TOTPSecretEnc []byte                `json:"totp_secret_enc,omitempty"` // AES-256-GCM(base32 secret); empty = TOTP not enrolled
	Credentials   []webauthn.Credential `json:"credentials,omitempty"`     // zero+ registered passkeys (public material only)
	Scope         string                `json:"scope"`                     // authorization grant; first user = AdminScope
	CreatedAt     time.Time             `json:"created_at"`
	UpdatedAt     time.Time             `json:"updated_at"`
}

// WebAuthnID implements webauthn.User: the opaque user handle (not the email, to
// avoid putting PII in the credential).
func (u *UserRecord) WebAuthnID() []byte { return u.Handle }

// WebAuthnName implements webauthn.User: the human-palatable account name.
func (u *UserRecord) WebAuthnName() string { return u.Email }

// WebAuthnDisplayName implements webauthn.User.
func (u *UserRecord) WebAuthnDisplayName() string { return u.DisplayName }

// WebAuthnCredentials implements webauthn.User: the user's registered passkeys.
func (u *UserRecord) WebAuthnCredentials() []webauthn.Credential { return u.Credentials }

// HasTOTP reports whether the user has enrolled a TOTP secret.
func (u *UserRecord) HasTOTP() bool { return len(u.TOTPSecretEnc) > 0 }

// IsAdmin reports whether the user's scope carries a wildcard admin grant. A thin
// helper for the (stage-2) admin seam; stage 1 only the first user is admin.
func (u *UserRecord) IsAdmin() bool {
	for _, raw := range strings.Split(u.Scope, ",") {
		if strings.TrimSpace(raw) == AdminScope {
			return true
		}
	}
	return false
}

// SessionRecord is one logged-in browser session: an opaque server-side handle
// bound to a user's email with an absolute expiry.
type SessionRecord struct {
	ID       string    `json:"id"`
	Email    string    `json:"email"`
	IssuedAt time.Time `json:"issued_at"`
	Expiry   time.Time `json:"expiry"`
}

// Store is the global user/session store handle. Safe for concurrent use; bbolt
// serialises writes and allows concurrent reads.
type Store struct {
	db      *bolt.DB
	path    string
	totpKey [32]byte
}

// Open opens (creating if absent) the user store at path and ensures its buckets
// exist. totpKey is the 32-byte AES-256 key used to encrypt TOTP secrets at rest
// (see ResolveTOTPKey). The parent directory is created as needed.
func Open(path string, totpKey []byte) (*Store, error) {
	if len(totpKey) != 32 {
		return nil, fmt.Errorf("userstore: totp key must be 32 bytes, got %d", len(totpKey))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("userstore: create parent dir: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("userstore: open %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range []string{usersBucket, sessionsBucket} {
			if _, e := tx.CreateBucketIfNotExists([]byte(b)); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("userstore: init buckets: %w", err)
	}
	s := &Store{db: db, path: path}
	copy(s.totpKey[:], totpKey)
	return s, nil
}

// Close closes the underlying bbolt DB.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Path returns the store's filesystem path (useful for logging).
func (s *Store) Path() string { return s.path }

// --- users ------------------------------------------------------------------

// IsEmpty reports whether no users exist — the first-run condition that makes the
// WebUI serve the first-registration wizard and that preserves the no-lockout
// single-operator behaviour while true.
func (s *Store) IsEmpty() (bool, error) {
	empty := true
	err := s.db.View(func(tx *bolt.Tx) error {
		k, _ := tx.Bucket([]byte(usersBucket)).Cursor().First()
		empty = k == nil
		return nil
	})
	return empty, err
}

// GetUser resolves a user by email. Returns ErrNotFound if unknown.
func (s *Store) GetUser(email string) (*UserRecord, error) {
	key := normalizeEmail(email)
	var rec UserRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(usersBucket)).Get([]byte(key))
		if v == nil {
			return ErrNotFound
		}
		return json.Unmarshal(v, &rec)
	})
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// CreateFirstAdmin atomically creates the very first user as a wildcard admin
// (AdminScope). It re-checks emptiness INSIDE the write transaction and returns
// ErrUsersExist if any user already exists, so two concurrent registrants racing
// the first-run wizard cannot both become admin — exactly one wins, the other is
// refused. The caller supplies email/display name/password hash/optional TOTP/0+
// credentials; this sets Scope=AdminScope and the timestamps.
func (s *Store) CreateFirstAdmin(rec *UserRecord) error {
	if rec.Email == "" {
		return errors.New("userstore: email required")
	}
	rec.Email = normalizeEmail(rec.Email)
	rec.Scope = AdminScope
	return s.createWithGuard(rec, func(b *bolt.Bucket) error {
		if k, _ := b.Cursor().First(); k != nil {
			return ErrUsersExist
		}
		return nil
	})
}

// CreateUser creates a non-first user (invite redemption, a later stage). Refuses
// with ErrExists if the email is already registered. The caller sets Scope.
func (s *Store) CreateUser(rec *UserRecord) error {
	if rec.Email == "" {
		return errors.New("userstore: email required")
	}
	rec.Email = normalizeEmail(rec.Email)
	return s.createWithGuard(rec, func(b *bolt.Bucket) error {
		if b.Get([]byte(rec.Email)) != nil {
			return ErrExists
		}
		return nil
	})
}

// createWithGuard generates the user handle, fills timestamps, runs guard inside
// the tx (the precondition re-check) and persists. guard returning non-nil aborts
// the create with that error (no write).
func (s *Store) createWithGuard(rec *UserRecord, guard func(*bolt.Bucket) error) error {
	if len(rec.Handle) == 0 {
		h := make([]byte, 32)
		if _, err := rand.Read(h); err != nil {
			return fmt.Errorf("userstore: generate handle: %w", err)
		}
		rec.Handle = h
	}
	now := time.Now()
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = now
	}
	rec.UpdatedAt = now
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(usersBucket))
		if err := guard(b); err != nil {
			return err
		}
		val, err := json.Marshal(rec)
		if err != nil {
			return fmt.Errorf("userstore: encode user: %w", err)
		}
		return b.Put([]byte(rec.Email), val)
	})
}

// PutUser overwrites an existing user record (e.g. to persist a passkey enrolment
// or an updated sign count after login). It bumps UpdatedAt. It does not guard on
// existence — it is an update of a record the caller already read.
func (s *Store) PutUser(rec *UserRecord) error {
	rec.Email = normalizeEmail(rec.Email)
	rec.UpdatedAt = time.Now()
	val, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("userstore: encode user: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(usersBucket)).Put([]byte(rec.Email), val)
	})
}

// --- TOTP secret sealing (key held by the store) ----------------------------

// SealTOTPSecret encrypts a base32 TOTP secret for storage in UserRecord.
// TOTPSecretEnc. Returns nil for an empty secret.
func (s *Store) SealTOTPSecret(secret string) ([]byte, error) { return s.sealTOTP(secret) }

// OpenTOTPSecret decrypts a sealed TOTP secret. Returns "" for an unenrolled user.
func (s *Store) OpenTOTPSecret(enc []byte) (string, error) { return s.openTOTP(enc) }

// --- sessions ----------------------------------------------------------------

// CreateSession mints an opaque session handle bound to email with an absolute
// expiry now+ttl, and persists it.
func (s *Store) CreateSession(email string, now time.Time, ttl time.Duration) (SessionRecord, error) {
	id, err := NewHandle()
	if err != nil {
		return SessionRecord{}, err
	}
	rec := SessionRecord{ID: id, Email: normalizeEmail(email), IssuedAt: now, Expiry: now.Add(ttl)}
	val, err := json.Marshal(rec)
	if err != nil {
		return SessionRecord{}, fmt.Errorf("userstore: encode session: %w", err)
	}
	if err := s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(sessionsBucket)).Put([]byte(id), val)
	}); err != nil {
		return SessionRecord{}, err
	}
	return rec, nil
}

// LookupSession resolves a session handle. Returns ErrNotFound if unknown and
// ErrExpired (after deleting the row) if past its expiry, so an expired cookie is
// cleaned up on use and never lingers.
func (s *Store) LookupSession(id string, now time.Time) (SessionRecord, error) {
	var rec SessionRecord
	var expired bool
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(sessionsBucket))
		v := b.Get([]byte(id))
		if v == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(v, &rec); err != nil {
			return fmt.Errorf("userstore: decode session: %w", err)
		}
		if now.After(rec.Expiry) {
			expired = true
			return b.Delete([]byte(id))
		}
		return nil
	})
	if err != nil {
		return SessionRecord{}, err
	}
	if expired {
		return SessionRecord{}, ErrExpired
	}
	return rec, nil
}

// DeleteSession removes one session (logout). Idempotent.
func (s *Store) DeleteSession(id string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(sessionsBucket)).Delete([]byte(id))
	})
}

// DeleteExpiredSessions sweeps sessions past their expiry, returning the count
// deleted. Keys are collected during the scan and removed after it (never mutating
// mid-iteration) — the oauthstore DeleteDeadSeries discipline.
func (s *Store) DeleteExpiredSessions(now time.Time) (int, error) {
	var deleted int
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(sessionsBucket))
		var dead [][]byte
		err := b.ForEach(func(k, v []byte) error {
			var rec SessionRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("userstore: decode session: %w", err)
			}
			if now.After(rec.Expiry) {
				dead = append(dead, append([]byte(nil), k...))
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, k := range dead {
			if derr := b.Delete(k); derr != nil {
				return derr
			}
		}
		deleted = len(dead)
		return nil
	})
	return deleted, err
}

// --- helpers -----------------------------------------------------------------

// NewHandle returns a cryptographically-random opaque handle (256 bits, base64url,
// no padding) — the same handle shape oauthstore uses for tokens.
func NewHandle() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("userstore: generate handle: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// normalizeEmail lower-cases and trims the email so it is a stable key (emails are
// case-insensitive in practice; the git author identity stays the normalized form).
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
