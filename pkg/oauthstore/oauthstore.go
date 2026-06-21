// Package oauthstore is the durable, go-git-free store for Shoka's built-in
// OAuth 2.1 authorization server (the 2026-06-03 MCP OAuth (b) directive). It
// holds the transient OAuth state that is NOT versioned project content —
// authorization codes and the SET of issued token series — so it must never go
// through the go-git storage layer (Architectural Anchor 1). It is a sibling of
// the per-project catalog, reusing the same embedded DB technology (bbolt) at a
// single global database <base_dir>/oauth.db.
//
// The store models MULTIPLE simultaneous connections: several MCP clients
// (Claude web/desktop/mobile, Claude Code, other devices) each run their own
// OAuth flow, so multiple token series exist at once — all currently bound to
// the same one current-mode principal, but each series individually enumerable
// and revocable, with refresh rotation per series and independent across
// connections. There is deliberately NO client-registration table: CIMD-only
// means the client_id is the client's metadata URL, not a stored registration —
// only code and token state persists here.
//
// Bucket layout (bbolt buckets are flat/top-level):
//
//	"codes"   code handle   -> JSON CodeRecord       (single-use, short TTL)
//	"series"  series id      -> JSON SeriesRecord     (one live token pair per connection)
//	"access"  access handle  -> series id              (O(1) bearer validation)
//	"refresh" refresh handle -> series id              (O(1) refresh-grant lookup)
//	"clients" client id      -> JSON RegisteredClient  (DCR-registered public clients, B-63)
//
// A series owns exactly one live access token and one live refresh token at a
// time; a refresh rotation atomically swaps both and deletes the predecessors,
// so a rotated refresh invalidates only its own predecessor and leaves every
// other series untouched.
//
// The "clients" bucket holds Dynamic Client Registration records (RFC 7591, the
// 2026-06-12 B-63 directive): claude.ai's official connector docs require DCR for
// OAuth servers, so a client POSTs its metadata to /register and Shoka issues +
// PERSISTS a public client_id here (no secret — public client + PKCE). This is
// ADDITIVE to CIMD, which remains stateless (the CIMD client_id is the metadata
// URL, never stored). A DCR client_id is an opaque handle (NewHandle), so it is
// trivially distinguishable from a CIMD client_id (an https URL) at /authorize and
// /token; only the DCR path consults this bucket.
package oauthstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Errors returned by the package.
var (
	// ErrNotFound is returned by Lookup/Rotate/Revoke when the handle or series
	// does not exist (or has already been consumed/revoked/rotated away).
	ErrNotFound = errors.New("oauthstore: not found")
	// ErrExpired is returned when a record exists but is past its expiry.
	ErrExpired = errors.New("oauthstore: expired")
)

const (
	codesBucket         = "codes"
	seriesBucket        = "series"
	accessBucket        = "access"
	refreshBucket       = "refresh"
	clientsBucket       = "clients"       // DCR-registered public clients (B-63, RFC 7591)
	metaBucket          = "meta"          // store metadata (schema/migration markers, B-71 Stage 0)
	registrationsBucket = "registrations" // dynamic OAuth registration entries (B-71 Stage 1)
)

// migrationKeyTokensHashed marks that the one-time B-71 Stage 0 re-key (raw token →
// hash at rest) has completed for this store. Its presence in metaBucket makes the
// migration idempotent — it never re-runs once done.
const migrationKeyTokensHashed = "tokens_hashed_v1"

// hashHandle is the one-way at-rest representation of an OAuth handle (access token,
// refresh token, or authorization code). The buckets are keyed by hashHandle(raw) and
// SeriesRecord persists only the hash, so a read of oauth.db never yields a usable
// credential (B-71 Stage 0). Handles are 256-bit cryptographically-random (NewHandle),
// so a single fast SHA-256 is sufficient — there is nothing to brute-force, unlike a
// user-chosen password (no argon2 needed). It uses the same one-way SHA-256 primitive
// as internal/tokenfp.Fingerprint, but the FULL 64-hex digest (no truncation) because
// this is a lookup key where collisions must not occur. The empty handle maps to the
// empty string (an absent handle is never a stored key).
func hashHandle(handle string) string {
	if handle == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(handle))
	return hex.EncodeToString(sum[:])
}

// Principal is the bound current-mode principal for a token. Today this is the
// single configured operator; under multi-user mode (a later B-28 leg) different
// series may carry different principals — this type does not assume single-user.
type Principal struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// CodeRecord is a single-use authorization code bound to the PKCE challenge, the
// client, the redirect URI, the RFC 8707 resource (audience), and the principal.
type CodeRecord struct {
	ClientID            string    `json:"client_id"` // the CIMD metadata URL
	RedirectURI         string    `json:"redirect_uri"`
	CodeChallenge       string    `json:"code_challenge"`
	CodeChallengeMethod string    `json:"code_challenge_method"`
	Resource            string    `json:"resource"`
	Principal           Principal `json:"principal"`
	Expiry              time.Time `json:"expiry"`
}

// SeriesRecord is one connection's live token pair plus the binding that every
// rotation preserves. A rotation replaces both tokens.
//
// At rest only the HASHES are persisted (B-71 Stage 0): AccessTokenHash /
// RefreshTokenHash are hashHandle(rawToken), and the access/refresh buckets are keyed
// by those hashes — so a read of oauth.db never yields a usable bearer/refresh token.
// AccessToken/RefreshToken are the RAW handles; they are `json:"-"` (never written to
// the DB) and are populated ONLY in-memory on the record returned by NewSeries/Rotate,
// the one moment the raw value exists, so the caller can hand it to the client once.
// A record decoded from the store has empty AccessToken/RefreshToken and non-empty
// hashes — every lookup hashes the incoming handle and compares against the hash.
type SeriesRecord struct {
	SeriesID         string    `json:"series_id"`
	AccessToken      string    `json:"-"` // raw; in-memory only (issuance return), never persisted
	RefreshToken     string    `json:"-"` // raw; in-memory only (issuance return), never persisted
	AccessTokenHash  string    `json:"access_token_hash"`
	RefreshTokenHash string    `json:"refresh_token_hash"`
	ClientID         string    `json:"client_id"` // the CIMD metadata URL
	Principal        Principal `json:"principal"`
	Resource         string    `json:"resource"`
	IssuedAt         time.Time `json:"issued_at"`
	AccessExpiry     time.Time `json:"access_expiry"`
	RefreshExpiry    time.Time `json:"refresh_expiry"`
	// Scope is the authorization grant the token carries (the 2026-06-15 authz
	// foundation). "*" = all-access (every DCR token). A pre-issued non-DCR token
	// would carry a namespace grant (e.g. "namespace:foo"). A record written before
	// this field existed decodes Scope as "" (JSON omits the absent key), which the
	// validation path and the authz gate both interpret as "*" — so old tokens stay
	// all-access and age out as they expire; no migration is required.
	Scope string `json:"scope,omitempty"`
}

// RegisteredClient is a Dynamic Client Registration record (RFC 7591, B-63): a
// public client (no secret) that POSTed its metadata to /register and was issued
// a persistent ClientID. The redirect_uris bind the /authorize redirect_uri the
// same way CIMD metadata's redirect_uris do; TokenEndpointAuthMethod is always
// "none" (public client + PKCE). No secret is ever stored for a public client.
type RegisteredClient struct {
	ClientID                string    `json:"client_id"` // the issued opaque handle (NOT a URL)
	RedirectURIs            []string  `json:"redirect_uris"`
	TokenEndpointAuthMethod string    `json:"token_endpoint_auth_method"` // always "none"
	ClientName              string    `json:"client_name,omitempty"`
	GrantTypes              []string  `json:"grant_types,omitempty"`
	ResponseTypes           []string  `json:"response_types,omitempty"`
	ClientIDIssuedAt        time.Time `json:"client_id_issued_at"`
	// Domain is the DCR client's attributed trusted domain (B-71 Stage 2a), derived from
	// its redirect_uris host at registration so a DCR-issued series can be grouped under a
	// domain like a CIMD one. omitempty + decode-safe: a record written before Stage 2a (or
	// a multi-host/unparseable client) has "" and is derived lazily by SeriesDomain. No
	// migration. RECORD-ONLY this stage — it gates/groups nothing yet.
	Domain string `json:"domain,omitempty"`
}

// SelfIssuedClientID is the client_id of the operator's own self-issued CLI token
// (cmd/shoka OAUTH_ISSUE_SELF). It is neither a CIMD URL nor a DCR handle — it belongs to
// the pre-issued/confidential world, not a domain, so SeriesDomain reports it unattributed.
const SelfIssuedClientID = "shoka-cli"

// SeriesInfo is the enumerable view of a live connection for the (c) management
// surface — never carries the secret token handles.
type SeriesInfo struct {
	SeriesID     string
	ClientID     string
	Principal    Principal
	Resource     string
	IssuedAt     time.Time
	AccessExpiry time.Time
	// Scope is the token's authorization grant ("*" = all-access). Carried on the
	// enumerable view for the admin surface (the display is a later directive).
	Scope string
}

// Store is the global OAuth state store handle. Safe for concurrent use; bbolt's
// transaction model serialises writes and allows concurrent reads.
type Store struct {
	db   *bolt.DB
	path string

	// Process-lifetime observability counters (the 2026-06-05 M3 metrics directive).
	// tokensIssued counts first-issue token pairs (NewSeries) — NOT rotations, since
	// Rotate mints inline and never calls NewSeries, so this is the "new connections"
	// signal. revocations counts actual series deletions (Revoke past its idempotent
	// no-op). Both are counts only — never a token, series id, or principal — surfaced
	// through the metrics OAuthSource bridge. Counter resets on restart are fine
	// (Prometheus tolerates them); a durable bbolt tally is deliberately out of scope.
	tokensIssued atomic.Int64
	revocations  atomic.Int64
}

// Open opens (creating if absent) the OAuth store at path and ensures its
// buckets exist. The parent directory is created as needed.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("oauthstore: create parent dir: %w", err)
	}
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("oauthstore: open %s: %w", path, err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, b := range []string{codesBucket, seriesBucket, accessBucket, refreshBucket, clientsBucket, metaBucket, registrationsBucket} {
			if _, e := tx.CreateBucketIfNotExists([]byte(b)); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("oauthstore: init buckets: %w", err)
	}
	s := &Store{db: db, path: path}
	// One-time B-71 Stage 0 re-key: any store written by the old plaintext layout is
	// transformed so no raw token remains at rest. Idempotent + crash-safe (a single
	// atomic transaction guarded by a marker), so it is safe to run on every Open.
	if err := s.migrateHashTokensAtRest(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("oauthstore: token-hash migration: %w", err)
	}
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

// --- authorization codes ----------------------------------------------------

// PutCode stores a single-use authorization code. The caller supplies the code
// handle (generated via NewHandle) so /authorize controls the value it returns
// in the redirect.
func (s *Store) PutCode(code string, rec CodeRecord) error {
	val, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("oauthstore: encode code: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		// Keyed by the hash of the code, not the code itself (B-71 Stage 0).
		return tx.Bucket([]byte(codesBucket)).Put([]byte(hashHandle(code)), val)
	})
}

// TakeCode atomically fetches and deletes an authorization code (single-use). It
// returns ErrNotFound if the code is unknown or already consumed, and ErrExpired
// (after deleting it) if it is past its expiry. The delete-on-read guarantees a
// code cannot be replayed even on a concurrent double exchange.
func (s *Store) TakeCode(code string, now time.Time) (CodeRecord, error) {
	var rec CodeRecord
	var expired bool
	// The delete must COMMIT even when the code is expired (so it cannot be
	// replayed), so the transaction returns nil after deleting and expiry is
	// signalled out-of-band — a non-nil return from db.Update rolls the tx back.
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(codesBucket))
		ch := hashHandle(code) // the bucket is keyed by the code's hash (B-71 Stage 0)
		v := b.Get([]byte(ch))
		if v == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(v, &rec); err != nil {
			return fmt.Errorf("oauthstore: decode code: %w", err)
		}
		if derr := b.Delete([]byte(ch)); derr != nil {
			return derr
		}
		expired = now.After(rec.Expiry)
		return nil
	})
	if err != nil {
		return CodeRecord{}, err
	}
	if expired {
		return CodeRecord{}, ErrExpired
	}
	return rec, nil
}

// --- token series -----------------------------------------------------------

// NewSeries issues the first access+refresh pair for a fresh connection and
// persists the series. It returns the new SeriesRecord (carrying the freshly
// generated handles). Each call is an independent series — multiple concurrent
// connections produce multiple independent series.
func (s *Store) NewSeries(clientID string, p Principal, resource, scope string, now time.Time, accessTTL, refreshTTL time.Duration) (SeriesRecord, error) {
	seriesID, err := NewHandle()
	if err != nil {
		return SeriesRecord{}, err
	}
	access, err := NewHandle()
	if err != nil {
		return SeriesRecord{}, err
	}
	refresh, err := NewHandle()
	if err != nil {
		return SeriesRecord{}, err
	}
	rec := SeriesRecord{
		SeriesID:         seriesID,
		AccessToken:      access,  // raw, in-memory only — returned to the caller once
		RefreshToken:     refresh, // raw, in-memory only — returned to the caller once
		AccessTokenHash:  hashHandle(access),
		RefreshTokenHash: hashHandle(refresh),
		ClientID:         clientID,
		Principal:        p,
		Resource:         resource,
		IssuedAt:         now,
		AccessExpiry:     now.Add(accessTTL),
		RefreshExpiry:    now.Add(refreshTTL),
		Scope:            scope,
	}
	if err := s.putSeries(rec, "", ""); err != nil {
		return SeriesRecord{}, err
	}
	s.tokensIssued.Add(1) // first-issue only; Rotate mints inline and is not counted here
	return rec, nil
}

// Rotate consumes a refresh token and issues a new access+refresh pair in the
// SAME series, invalidating the predecessor pair in the same transaction (OAuth
// 2.1 public-client rotation). It returns ErrNotFound if the refresh handle is
// unknown, already rotated away, or revoked; ErrExpired if the refresh token is
// past its expiry (the series is then revoked). Rotation touches only this
// series; every other series is untouched.
func (s *Store) Rotate(oldRefresh string, now time.Time, accessTTL, refreshTTL time.Duration) (SeriesRecord, error) {
	var out SeriesRecord
	var expired bool
	err := s.db.Update(func(tx *bolt.Tx) error {
		oldRefreshHash := hashHandle(oldRefresh)
		rb := tx.Bucket([]byte(refreshBucket))
		sidRaw := rb.Get([]byte(oldRefreshHash))
		if sidRaw == nil {
			return ErrNotFound
		}
		seriesID := string(sidRaw)
		sb := tx.Bucket([]byte(seriesBucket))
		sraw := sb.Get([]byte(seriesID))
		if sraw == nil {
			return ErrNotFound
		}
		var rec SeriesRecord
		if err := json.Unmarshal(sraw, &rec); err != nil {
			return fmt.Errorf("oauthstore: decode series: %w", err)
		}
		// Predecessor invalidation: only the series' CURRENT refresh token may
		// rotate. A stale handle (already rotated away) is rejected. Compared by hash,
		// since the raw refresh is never stored (B-71 Stage 0).
		if rec.RefreshTokenHash != oldRefreshHash {
			return ErrNotFound
		}
		if now.After(rec.RefreshExpiry) {
			// Expired refresh: revoke the whole series. The deletion must COMMIT,
			// so return nil and signal expiry out-of-band (a non-nil return from
			// db.Update would roll the deletion back).
			expired = true
			return deleteSeries(tx, rec)
		}
		newAccess, err := NewHandle()
		if err != nil {
			return err
		}
		newRefresh, err := NewHandle()
		if err != nil {
			return err
		}
		oldAccessHash := rec.AccessTokenHash
		rec.AccessToken = newAccess   // raw, in-memory only — returned to the caller once
		rec.RefreshToken = newRefresh // raw, in-memory only — returned to the caller once
		rec.AccessTokenHash = hashHandle(newAccess)
		rec.RefreshTokenHash = hashHandle(newRefresh)
		rec.AccessExpiry = now.Add(accessTTL)
		rec.RefreshExpiry = now.Add(refreshTTL)
		if err := putSeriesTx(tx, rec, oldAccessHash, oldRefreshHash); err != nil {
			return err
		}
		out = rec
		return nil
	})
	if err != nil {
		return SeriesRecord{}, err
	}
	if expired {
		return SeriesRecord{}, ErrExpired
	}
	return out, nil
}

// Lookup resolves a bearer access token to its series, for enforcement on the
// MCP path. It returns ErrNotFound if the handle is unknown/revoked/rotated away
// and ErrExpired if the access token is past its expiry. O(1) (two gets, no
// scan).
func (s *Store) Lookup(accessToken string, now time.Time) (SeriesRecord, error) {
	var rec SeriesRecord
	err := s.db.View(func(tx *bolt.Tx) error {
		ah := hashHandle(accessToken) // the bucket is keyed by the token's hash (B-71 Stage 0)
		ab := tx.Bucket([]byte(accessBucket))
		sidRaw := ab.Get([]byte(ah))
		if sidRaw == nil {
			return ErrNotFound
		}
		sraw := tx.Bucket([]byte(seriesBucket)).Get(sidRaw)
		if sraw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(sraw, &rec); err != nil {
			return fmt.Errorf("oauthstore: decode series: %w", err)
		}
		// Defend against a stale access pointer that outlived a rotation (by hash —
		// the raw access token is never stored).
		if rec.AccessTokenHash != ah {
			return ErrNotFound
		}
		if now.After(rec.AccessExpiry) {
			return ErrExpired
		}
		return nil
	})
	if err != nil {
		return SeriesRecord{}, err
	}
	return rec, nil
}

// RefreshClientID resolves a refresh token to its series' ClientID WITHOUT rotating, so the
// /token refresh grant can compute the per-domain issuance TTL before Rotate (B-71 Stage 2c).
// ok=false if the refresh handle is unknown/rotated-away. O(1): two gets, no scan. Compared by
// hash (the raw refresh is never stored).
func (s *Store) RefreshClientID(refreshToken string) (clientID string, ok bool) {
	rh := hashHandle(refreshToken)
	_ = s.db.View(func(tx *bolt.Tx) error {
		sidRaw := tx.Bucket([]byte(refreshBucket)).Get([]byte(rh))
		if sidRaw == nil {
			return nil
		}
		sraw := tx.Bucket([]byte(seriesBucket)).Get(sidRaw)
		if sraw == nil {
			return nil
		}
		var rec SeriesRecord
		if err := json.Unmarshal(sraw, &rec); err != nil {
			return nil
		}
		if rec.RefreshTokenHash != rh { // stale pointer that outlived a rotation
			return nil
		}
		clientID, ok = rec.ClientID, true
		return nil
	})
	return clientID, ok
}

// List enumerates every live series as SeriesInfo (no secret handles), for the
// (c) management surface. Sorted by IssuedAt is the caller's concern; order here
// is bbolt key order (the series id).
func (s *Store) List() ([]SeriesInfo, error) {
	var out []SeriesInfo
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(seriesBucket)).ForEach(func(_, v []byte) error {
			var rec SeriesRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("oauthstore: decode series: %w", err)
			}
			out = append(out, SeriesInfo{
				SeriesID:     rec.SeriesID,
				ClientID:     rec.ClientID,
				Principal:    rec.Principal,
				Resource:     rec.Resource,
				IssuedAt:     rec.IssuedAt,
				AccessExpiry: rec.AccessExpiry,
				Scope:        rec.Scope,
			})
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Revoke deletes one series and its access+refresh handles. Idempotent: revoking
// an unknown series is a no-op (nil). Revoking one series never affects another.
func (s *Store) Revoke(seriesID string) error {
	var revoked bool // a series actually existed and was removed (vs. the idempotent no-op)
	err := s.db.Update(func(tx *bolt.Tx) error {
		sb := tx.Bucket([]byte(seriesBucket))
		sraw := sb.Get([]byte(seriesID))
		if sraw == nil {
			return nil
		}
		revoked = true
		var rec SeriesRecord
		if err := json.Unmarshal(sraw, &rec); err != nil {
			// Even if undecodable, drop the series row so it cannot linger.
			return sb.Delete([]byte(seriesID))
		}
		return deleteSeries(tx, rec)
	})
	// Count only a committed, real revocation — increment after Update returns so a
	// rolled-back tx (non-nil err) never over-counts, and a no-op revoke is not counted.
	if err == nil && revoked {
		s.revocations.Add(1)
	}
	return err
}

// RevokeByPrincipalEmail deletes every token series AND every outstanding authorization
// code bound to the given principal email (case-insensitive), returning the number of
// series revoked. This is the cross-store access cut for user disable/delete (B-28): a
// removed or disabled user's MCP/OAuth tokens must stop authorizing IMMEDIATELY, not age
// out. One write transaction; keys/records are collected during each scan and removed
// after it, never mutating mid-iteration (the DeleteDeadSeries discipline). Idempotent:
// an unknown or empty principal is a no-op (0, nil). Counts toward revocations like Revoke.
func (s *Store) RevokeByPrincipalEmail(email string) (int, error) {
	target := strings.ToLower(strings.TrimSpace(email))
	if target == "" {
		return 0, nil
	}
	var revoked int
	err := s.db.Update(func(tx *bolt.Tx) error {
		// Token series.
		sb := tx.Bucket([]byte(seriesBucket))
		var deadSeries []SeriesRecord
		if err := sb.ForEach(func(_, v []byte) error {
			var rec SeriesRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("oauthstore: decode series: %w", err)
			}
			if strings.EqualFold(strings.TrimSpace(rec.Principal.Email), target) {
				deadSeries = append(deadSeries, rec)
			}
			return nil
		}); err != nil {
			return err
		}
		for _, rec := range deadSeries {
			if err := deleteSeries(tx, rec); err != nil {
				return err
			}
		}
		revoked = len(deadSeries)
		// Outstanding authorization codes for the same principal (not reachable via the
		// series buckets, so handled here so a pending code cannot mint a fresh token).
		cb := tx.Bucket([]byte(codesBucket))
		var deadCodes [][]byte
		if err := cb.ForEach(func(k, v []byte) error {
			var rec CodeRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return nil // skip undecodable
			}
			if strings.EqualFold(strings.TrimSpace(rec.Principal.Email), target) {
				deadCodes = append(deadCodes, append([]byte(nil), k...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, k := range deadCodes {
			if err := cb.Delete(k); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	if revoked > 0 {
		s.revocations.Add(int64(revoked))
	}
	return revoked, nil
}

// DeleteDeadSeries removes every series that is FULLY dead — both its access and
// its refresh token are unusable. A series is dead once now is after its
// RefreshExpiry (the refresh token is the longer-lived of the pair, so a passed
// refresh-expiry means neither token can be used again), and a dead series is
// swept IMMEDIATELY — there is no grace period (B-71 Stage 5: expiry ⇒ revoke,
// matching GitHub PATs, which have no grace). It returns the number of series
// deleted. An access-expired series whose refresh is still live is NOT deleted (it
// remains a usable connection).
//
// It is the cleaner sweep's one cycle (StartCleaner drives it on a ticker); it is
// exported so a cycle can be run directly in a test without synthetic timing.
// Deletion reuses deleteSeries — the same path Revoke/Rotate use — so the access
// and refresh handle buckets are cleaned alongside the series row. Keys to delete
// are collected during the ForEach scan and removed after it, never mutating the
// bucket mid-iteration.
func (s *Store) DeleteDeadSeries(now time.Time) (int, error) {
	var deleted int
	err := s.db.Update(func(tx *bolt.Tx) error {
		var dead []SeriesRecord
		err := tx.Bucket([]byte(seriesBucket)).ForEach(func(_, v []byte) error {
			var rec SeriesRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("oauthstore: decode series: %w", err)
			}
			if now.After(rec.RefreshExpiry) {
				dead = append(dead, rec)
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, rec := range dead {
			if derr := deleteSeries(tx, rec); derr != nil {
				return derr
			}
		}
		deleted = len(dead)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return deleted, nil
}

// RewriteScopes applies fn to every token series' scope, rewriting any series whose
// scope fn changes — the cascade cleanup after a namespace/project delete (B-28 ns/proj
// management). Only the series row's Scope field changes; the access/refresh handle
// buckets carry no scope and are untouched. fn is a pure scope→scope transform (the authz
// prune helpers). It returns the number of series changed. One write transaction; keys
// are collected during the scan and written after it, never mutating mid-iteration (the
// DeleteDeadSeries discipline).
func (s *Store) RewriteScopes(fn func(scope string) string) (int, error) {
	changed := 0
	err := s.db.Update(func(tx *bolt.Tx) error {
		sb := tx.Bucket([]byte(seriesBucket))
		type seriesKV struct {
			k   []byte
			rec SeriesRecord
		}
		var upd []seriesKV
		if err := sb.ForEach(func(k, v []byte) error {
			var rec SeriesRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("oauthstore: decode series: %w", err)
			}
			if ns := fn(rec.Scope); ns != rec.Scope {
				rec.Scope = ns
				upd = append(upd, seriesKV{append([]byte(nil), k...), rec})
			}
			return nil
		}); err != nil {
			return err
		}
		for _, u := range upd {
			val, err := json.Marshal(&u.rec)
			if err != nil {
				return fmt.Errorf("oauthstore: encode series: %w", err)
			}
			if err := sb.Put(u.k, val); err != nil {
				return err
			}
			changed++
		}
		return nil
	})
	return changed, err
}

// --- registered clients (DCR, RFC 7591 — the 2026-06-12 B-63 directive) ------

// PutClient persists a DCR-registered client under its issued ClientID. The
// ClientID is generated by the caller via NewHandle so /register controls the
// value it returns. Overwriting an existing id is not expected (handles are
// unguessable and unique) but is harmless (last write wins).
func (s *Store) PutClient(rec RegisteredClient) error {
	val, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("oauthstore: encode client: %w", err)
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(clientsBucket)).Put([]byte(rec.ClientID), val)
	})
}

// GetClient resolves a DCR-registered client by its issued ClientID, for the
// /authorize redirect_uri binding and the /token deleted-client (401
// invalid_client) signal. It returns ErrNotFound if the client_id is unknown
// (never registered, or its record was removed) — the signal that tells claude.ai
// to re-register (RFC 6749 invalid_client). O(1) (one get, no scan).
func (s *Store) GetClient(clientID string) (RegisteredClient, error) {
	var rec RegisteredClient
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(clientsBucket)).Get([]byte(clientID))
		if v == nil {
			return ErrNotFound
		}
		return json.Unmarshal(v, &rec)
	})
	if err != nil {
		return RegisteredClient{}, err
	}
	return rec, nil
}

// --- metrics (the 2026-06-05 M3 directive) ----------------------------------
//
// These three methods satisfy the metrics OAuthSource bridge capability. They
// return counts/gauge ONLY — no series id, token, principal, or client domain —
// so no OAuth secret or identity can reach a metric label. The store is passed as
// a metrics "extra" only when non-nil (OAuth enabled), so these are never called
// on a nil receiver in production; the nil guards are defence-in-depth.

// OAuthActiveConnections returns the number of live token series (active
// connections) for the shoka_oauth_active_connections gauge — len(List()), read
// at scrape time. One bbolt read tx, bounded by the live series count. A read
// error yields 0 (the gauge degrades to absent-data rather than reporting a stale
// or invented value).
func (s *Store) OAuthActiveConnections() int64 {
	if s == nil || s.db == nil {
		return 0
	}
	infos, err := s.List()
	if err != nil {
		return 0
	}
	return int64(len(infos))
}

// OAuthTokensIssued returns the process-lifetime count of first-issued token pairs
// (new connections) for shoka_oauth_tokens_issued_total. Rotations are not counted.
func (s *Store) OAuthTokensIssued() int64 {
	if s == nil {
		return 0
	}
	return s.tokensIssued.Load()
}

// OAuthRevocations returns the process-lifetime count of revoked series for
// shoka_oauth_revocations_total.
func (s *Store) OAuthRevocations() int64 {
	if s == nil {
		return 0
	}
	return s.revocations.Load()
}

// --- internal helpers -------------------------------------------------------

// putSeries persists rec and re-points its access/refresh handles, deleting the
// given predecessor handles (empty = none). Used by NewSeries.
func (s *Store) putSeries(rec SeriesRecord, oldAccessHash, oldRefreshHash string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return putSeriesTx(tx, rec, oldAccessHash, oldRefreshHash)
	})
}

// putSeriesTx is the in-transaction form: write the series row (hashes only — the raw
// AccessToken/RefreshToken are json:"-" and never serialized), point the new
// access/refresh HASH keys at it, and delete the predecessor hash keys. oldAccessHash
// / oldRefreshHash are the predecessor pair's hashes ("" on first issue).
func putSeriesTx(tx *bolt.Tx, rec SeriesRecord, oldAccessHash, oldRefreshHash string) error {
	val, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("oauthstore: encode series: %w", err)
	}
	if err := tx.Bucket([]byte(seriesBucket)).Put([]byte(rec.SeriesID), val); err != nil {
		return err
	}
	ab := tx.Bucket([]byte(accessBucket))
	rb := tx.Bucket([]byte(refreshBucket))
	if oldAccessHash != "" && oldAccessHash != rec.AccessTokenHash {
		if err := ab.Delete([]byte(oldAccessHash)); err != nil {
			return err
		}
	}
	if oldRefreshHash != "" && oldRefreshHash != rec.RefreshTokenHash {
		if err := rb.Delete([]byte(oldRefreshHash)); err != nil {
			return err
		}
	}
	if err := ab.Put([]byte(rec.AccessTokenHash), []byte(rec.SeriesID)); err != nil {
		return err
	}
	return rb.Put([]byte(rec.RefreshTokenHash), []byte(rec.SeriesID))
}

// deleteSeries removes a series row and its current access/refresh HASH keys.
func deleteSeries(tx *bolt.Tx, rec SeriesRecord) error {
	if err := tx.Bucket([]byte(seriesBucket)).Delete([]byte(rec.SeriesID)); err != nil {
		return err
	}
	if err := tx.Bucket([]byte(accessBucket)).Delete([]byte(rec.AccessTokenHash)); err != nil {
		return err
	}
	return tx.Bucket([]byte(refreshBucket)).Delete([]byte(rec.RefreshTokenHash))
}

// migrateHashTokensAtRest performs the one-time B-71 Stage 0 re-key: a store written by
// the OLD plaintext layout (access/refresh buckets keyed by the RAW handle; raw tokens
// embedded in the series JSON) is transformed so no raw token remains at rest — buckets
// keyed by hashHandle, series JSON carrying only the hashes. Guarded by a marker in
// metaBucket and run in ONE atomic transaction, so it is idempotent (skipped once done)
// and crash-safe (a crash before commit changes nothing; the next Open re-runs cleanly).
//
//   - series: each row carrying a raw token (old layout) is re-keyed — the raw-keyed
//     access/refresh entries are replaced by hash-keyed ones (→ sid) and the series JSON
//     is rewritten with the hashes set and the raw fields cleared.
//   - codes: dropped wholesale — 1m-TTL single-use, and migration runs at Open before the
//     server accepts requests, so nothing live is lost.
//   - clients (DCR RegisteredClient): UNTOUCHED. A client_id is a PUBLIC identifier (sent
//     in /authorize and /token in the clear by design), not a secret, and these public
//     clients carry NO client_secret — there is nothing secret-equivalent to hash.
func (s *Store) migrateHashTokensAtRest() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		mb := tx.Bucket([]byte(metaBucket))
		if mb.Get([]byte(migrationKeyTokensHashed)) != nil {
			return nil // already migrated — never re-run
		}
		sb := tx.Bucket([]byte(seriesBucket))
		ab := tx.Bucket([]byte(accessBucket))
		rb := tx.Bucket([]byte(refreshBucket))

		// rawAwareRecord sees the OLD raw token fields (which the live SeriesRecord drops
		// via json:"-") so the migration can read a raw token still present at rest.
		type rawAwareRecord struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		}
		type rewrite struct {
			sid                      []byte
			val                      []byte
			oldAccess, oldRefresh    string
			newAccessKey, newRefresh string
		}
		var rewrites []rewrite
		if err := sb.ForEach(func(k, v []byte) error {
			var raw rawAwareRecord
			if err := json.Unmarshal(v, &raw); err != nil {
				return fmt.Errorf("oauthstore: migrate decode series: %w", err)
			}
			if raw.AccessToken == "" && raw.RefreshToken == "" {
				return nil // already new layout — nothing to re-key
			}
			var rec SeriesRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("oauthstore: migrate decode series: %w", err)
			}
			rec.AccessTokenHash = hashHandle(raw.AccessToken)
			rec.RefreshTokenHash = hashHandle(raw.RefreshToken)
			rec.AccessToken = ""  // json:"-" already drops it; cleared for clarity
			rec.RefreshToken = "" // json:"-" already drops it; cleared for clarity
			nv, err := json.Marshal(rec)
			if err != nil {
				return fmt.Errorf("oauthstore: migrate encode series: %w", err)
			}
			rewrites = append(rewrites, rewrite{
				sid:          append([]byte(nil), k...),
				val:          nv,
				oldAccess:    raw.AccessToken,
				oldRefresh:   raw.RefreshToken,
				newAccessKey: rec.AccessTokenHash,
				newRefresh:   rec.RefreshTokenHash,
			})
			return nil
		}); err != nil {
			return err
		}
		for _, rw := range rewrites {
			if err := sb.Put(rw.sid, rw.val); err != nil {
				return err
			}
			if rw.oldAccess != "" {
				if err := ab.Delete([]byte(rw.oldAccess)); err != nil {
					return err
				}
			}
			if err := ab.Put([]byte(rw.newAccessKey), rw.sid); err != nil {
				return err
			}
			if rw.oldRefresh != "" {
				if err := rb.Delete([]byte(rw.oldRefresh)); err != nil {
					return err
				}
			}
			if err := rb.Put([]byte(rw.newRefresh), rw.sid); err != nil {
				return err
			}
		}
		// Drop all authorization codes (1m-TTL, single-use; safe before serving).
		cb := tx.Bucket([]byte(codesBucket))
		var codeKeys [][]byte
		if err := cb.ForEach(func(k, _ []byte) error {
			codeKeys = append(codeKeys, append([]byte(nil), k...))
			return nil
		}); err != nil {
			return err
		}
		for _, k := range codeKeys {
			if err := cb.Delete(k); err != nil {
				return err
			}
		}
		return mb.Put([]byte(migrationKeyTokensHashed), []byte("1"))
	})
}

// NewHandle returns a cryptographically-random opaque token handle (256 bits,
// base64url, no padding). Opaque store-backed handles keep revocation immediate
// and avoid key management — preferred over JWTs for a self-contained single-AS
// + single-RS server (directive §2.4).
func NewHandle() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("oauthstore: generate handle: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
