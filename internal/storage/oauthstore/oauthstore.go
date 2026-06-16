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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	codesBucket   = "codes"
	seriesBucket  = "series"
	accessBucket  = "access"
	refreshBucket = "refresh"
	clientsBucket = "clients" // DCR-registered public clients (B-63, RFC 7591)
)

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
// rotation preserves. AccessToken/RefreshToken are the currently-valid handles;
// a rotation replaces both.
type SeriesRecord struct {
	SeriesID      string    `json:"series_id"`
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token"`
	ClientID      string    `json:"client_id"` // the CIMD metadata URL
	Principal     Principal `json:"principal"`
	Resource      string    `json:"resource"`
	IssuedAt      time.Time `json:"issued_at"`
	AccessExpiry  time.Time `json:"access_expiry"`
	RefreshExpiry time.Time `json:"refresh_expiry"`
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
}

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
		for _, b := range []string{codesBucket, seriesBucket, accessBucket, refreshBucket, clientsBucket} {
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
	return &Store{db: db, path: path}, nil
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
		return tx.Bucket([]byte(codesBucket)).Put([]byte(code), val)
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
		v := b.Get([]byte(code))
		if v == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(v, &rec); err != nil {
			return fmt.Errorf("oauthstore: decode code: %w", err)
		}
		if derr := b.Delete([]byte(code)); derr != nil {
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
		SeriesID:      seriesID,
		AccessToken:   access,
		RefreshToken:  refresh,
		ClientID:      clientID,
		Principal:     p,
		Resource:      resource,
		IssuedAt:      now,
		AccessExpiry:  now.Add(accessTTL),
		RefreshExpiry: now.Add(refreshTTL),
		Scope:         scope,
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
		rb := tx.Bucket([]byte(refreshBucket))
		sidRaw := rb.Get([]byte(oldRefresh))
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
		// rotate. A stale handle (already rotated away) is rejected.
		if rec.RefreshToken != oldRefresh {
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
		oldAccess := rec.AccessToken
		rec.AccessToken = newAccess
		rec.RefreshToken = newRefresh
		rec.AccessExpiry = now.Add(accessTTL)
		rec.RefreshExpiry = now.Add(refreshTTL)
		if err := putSeriesTx(tx, rec, oldAccess, oldRefresh); err != nil {
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
		ab := tx.Bucket([]byte(accessBucket))
		sidRaw := ab.Get([]byte(accessToken))
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
		// Defend against a stale access pointer that outlived a rotation.
		if rec.AccessToken != accessToken {
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

// DeleteDeadSeries removes every series that is FULLY dead — both its access and
// its refresh token are unusable — past a grace period. A series is fully dead
// once now is after its RefreshExpiry (the refresh token is the longer-lived of
// the pair, so a passed refresh-expiry means neither token can be used again);
// grace defers the deletion by that much past refresh-expiry so an operator can
// still see a recently-disconnected connection on the admin surface before it is
// swept. It returns the number of series deleted. An access-expired series whose
// refresh is still live is NOT deleted (it remains a usable connection).
//
// It is the cleaner sweep's one cycle (StartCleaner drives it on a ticker); it is
// exported so a cycle can be run directly in a test without synthetic timing.
// Deletion reuses deleteSeries — the same path Revoke/Rotate use — so the access
// and refresh handle buckets are cleaned alongside the series row. Keys to delete
// are collected during the ForEach scan and removed after it, never mutating the
// bucket mid-iteration.
func (s *Store) DeleteDeadSeries(now time.Time, grace time.Duration) (int, error) {
	var deleted int
	err := s.db.Update(func(tx *bolt.Tx) error {
		var dead []SeriesRecord
		err := tx.Bucket([]byte(seriesBucket)).ForEach(func(_, v []byte) error {
			var rec SeriesRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("oauthstore: decode series: %w", err)
			}
			if now.After(rec.RefreshExpiry.Add(grace)) {
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
func (s *Store) putSeries(rec SeriesRecord, oldAccess, oldRefresh string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return putSeriesTx(tx, rec, oldAccess, oldRefresh)
	})
}

// putSeriesTx is the in-transaction form: write the series row, point the new
// access/refresh handles at it, and delete the predecessor handles.
func putSeriesTx(tx *bolt.Tx, rec SeriesRecord, oldAccess, oldRefresh string) error {
	val, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("oauthstore: encode series: %w", err)
	}
	if err := tx.Bucket([]byte(seriesBucket)).Put([]byte(rec.SeriesID), val); err != nil {
		return err
	}
	ab := tx.Bucket([]byte(accessBucket))
	rb := tx.Bucket([]byte(refreshBucket))
	if oldAccess != "" && oldAccess != rec.AccessToken {
		if err := ab.Delete([]byte(oldAccess)); err != nil {
			return err
		}
	}
	if oldRefresh != "" && oldRefresh != rec.RefreshToken {
		if err := rb.Delete([]byte(oldRefresh)); err != nil {
			return err
		}
	}
	if err := ab.Put([]byte(rec.AccessToken), []byte(rec.SeriesID)); err != nil {
		return err
	}
	return rb.Put([]byte(rec.RefreshToken), []byte(rec.SeriesID))
}

// deleteSeries removes a series row and its current access/refresh handles.
func deleteSeries(tx *bolt.Tx, rec SeriesRecord) error {
	if err := tx.Bucket([]byte(seriesBucket)).Delete([]byte(rec.SeriesID)); err != nil {
		return err
	}
	if err := tx.Bucket([]byte(accessBucket)).Delete([]byte(rec.AccessToken)); err != nil {
		return err
	}
	return tx.Bucket([]byte(refreshBucket)).Delete([]byte(rec.RefreshToken))
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
