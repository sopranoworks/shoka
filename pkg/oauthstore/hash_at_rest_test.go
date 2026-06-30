package oauthstore

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// B-71 Stage 0: OAuth access/refresh tokens + authorization codes are stored HASHED at
// rest (the buckets are keyed by hashHandle(raw); SeriesRecord persists only the hashes).
// A read of oauth.db must never yield a usable bearer/refresh token or code.

// dbContainsRaw reports whether the raw string appears anywhere at rest — in ANY bucket
// key or value. Used to prove a raw credential is not recoverable from the store.
func dbContainsRaw(t *testing.T, s *Store, raw string) bool {
	t.Helper()
	if raw == "" {
		t.Fatal("dbContainsRaw called with empty string")
	}
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		for _, b := range []string{codesBucket, seriesBucket, accessBucket, refreshBucket, clientsBucket, metaBucket} {
			bk := tx.Bucket([]byte(b))
			if bk == nil {
				continue
			}
			if err := bk.ForEach(func(k, v []byte) error {
				if strings.Contains(string(k), raw) || strings.Contains(string(v), raw) {
					found = true
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan db: %v", err)
	}
	return found
}

// TestHashAtRest_StoredValueIsHashNotRawToken: after issuing a series, neither raw token
// appears anywhere at rest, but their hashes do (as the bucket keys). RED proof: revert
// putSeriesTx/NewSeries to store the raw handle → the raw token is found in the db → fail.
func TestHashAtRest_StoredValueIsHashNotRawToken(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	rec, err := s.NewSeries("https://c/meta", Principal{Name: "Op"}, "res", "*", "", now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries: %v", err)
	}
	// Issuance returns the raw handles in-memory (the one moment they exist).
	if rec.AccessToken == "" || rec.RefreshToken == "" {
		t.Fatal("NewSeries must return the raw access/refresh handles to the caller")
	}
	if dbContainsRaw(t, s, rec.AccessToken) {
		t.Fatal("raw access token is present at rest — must be stored hashed only")
	}
	if dbContainsRaw(t, s, rec.RefreshToken) {
		t.Fatal("raw refresh token is present at rest — must be stored hashed only")
	}
	// The hashes ARE stored (the access/refresh bucket keys).
	if !dbContainsRaw(t, s, hashHandle(rec.AccessToken)) {
		t.Fatal("access-token hash is not stored — lookup would be impossible")
	}
	if !dbContainsRaw(t, s, hashHandle(rec.RefreshToken)) {
		t.Fatal("refresh-token hash is not stored")
	}
	// The series JSON must not carry the raw tokens either.
	var seriesJSON string
	_ = s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(seriesBucket)).ForEach(func(_, v []byte) error {
			seriesJSON += string(v)
			return nil
		})
	})
	if strings.Contains(seriesJSON, rec.AccessToken) || strings.Contains(seriesJSON, rec.RefreshToken) {
		t.Fatal("series JSON embeds a raw token — must persist hashes only")
	}
	if !strings.Contains(seriesJSON, "access_token_hash") {
		t.Fatal("series JSON should carry access_token_hash")
	}
}

// TestHashAtRest_LookupAndRotateByRawToken: a client-held raw token still resolves
// (hash-then-compare), a forged one does not, and rotation invalidates the predecessor.
// RED proof: break the hashing on the lookup side (look up by the raw handle) → a valid
// raw token no longer resolves → fail.
func TestHashAtRest_LookupAndRotateByRawToken(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	rec, err := s.NewSeries("https://c/meta", Principal{Name: "Op"}, "res", "*", "", now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries: %v", err)
	}

	got, err := s.Lookup(rec.AccessToken, now)
	if err != nil || got.SeriesID != rec.SeriesID {
		t.Fatalf("Lookup by raw access token must resolve: sid=%q err=%v", got.SeriesID, err)
	}
	if _, err := s.Lookup("forged-token-not-issued", now); err != ErrNotFound {
		t.Fatalf("a forged token must not resolve: err=%v, want ErrNotFound", err)
	}

	rot, err := s.Rotate(rec.RefreshToken, now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("Rotate by raw refresh token must succeed: %v", err)
	}
	if rot.AccessToken == "" || rot.AccessToken == rec.AccessToken {
		t.Fatal("rotation must mint a fresh raw access token")
	}
	// Predecessor invalidated: the old refresh no longer rotates; the new access resolves.
	if _, err := s.Rotate(rec.RefreshToken, now, accessTTL, refreshTTL); err != ErrNotFound {
		t.Fatalf("the rotated-away refresh must be rejected: err=%v, want ErrNotFound", err)
	}
	if _, err := s.Lookup(rot.AccessToken, now); err != nil {
		t.Fatalf("the new access token must resolve after rotation: %v", err)
	}
}

// TestHashAtRest_CodeStoredHashed: an authorization code is keyed by its hash; the raw
// code is not at rest; it is consumable once by the raw value (TakeCode hashes it).
func TestHashAtRest_CodeStoredHashed(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	code, err := NewHandle()
	if err != nil {
		t.Fatalf("NewHandle: %v", err)
	}
	if err := s.PutCode(code, CodeRecord{ClientID: "c", Expiry: now.Add(time.Minute)}); err != nil {
		t.Fatalf("PutCode: %v", err)
	}
	if dbContainsRaw(t, s, code) {
		t.Fatal("raw authorization code is present at rest — must be stored hashed only")
	}
	if !dbContainsRaw(t, s, hashHandle(code)) {
		t.Fatal("code hash is not stored")
	}
	got, err := s.TakeCode(code, now)
	if err != nil || got.ClientID != "c" {
		t.Fatalf("TakeCode by raw code must resolve: client=%q err=%v", got.ClientID, err)
	}
	if _, err := s.TakeCode(code, now); err != ErrNotFound {
		t.Fatalf("a code is single-use: second TakeCode err=%v, want ErrNotFound", err)
	}
}

// oldSeriesJSON is the PRE-Stage-0 on-disk shape: raw tokens embedded, no hash fields.
// Used only to seed a store in the old plaintext layout for the migration test.
type oldSeriesJSON struct {
	SeriesID      string    `json:"series_id"`
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token"`
	ClientID      string    `json:"client_id"`
	Principal     Principal `json:"principal"`
	Resource      string    `json:"resource"`
	IssuedAt      time.Time `json:"issued_at"`
	AccessExpiry  time.Time `json:"access_expiry"`
	RefreshExpiry time.Time `json:"refresh_expiry"`
	Scope         string    `json:"scope,omitempty"`
}

// seedOldLayoutSeries writes one series in the OLD plaintext layout directly into the
// buckets (raw-keyed access/refresh, raw tokens in the JSON) and clears the migration
// marker, so migrateHashTokensAtRest will treat the store as un-migrated.
func seedOldLayoutSeries(t *testing.T, s *Store, rec oldSeriesJSON) {
	t.Helper()
	val, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal old series: %v", err)
	}
	err = s.db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket([]byte(seriesBucket)).Put([]byte(rec.SeriesID), val); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(accessBucket)).Put([]byte(rec.AccessToken), []byte(rec.SeriesID)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(refreshBucket)).Put([]byte(rec.RefreshToken), []byte(rec.SeriesID)); err != nil {
			return err
		}
		return tx.Bucket([]byte(metaBucket)).Delete([]byte(migrationKeyTokensHashed))
	})
	if err != nil {
		t.Fatalf("seed old layout: %v", err)
	}
}

// TestHashAtRest_MigrationReKeysOldLayout seeds a store in the OLD plaintext layout —
// including a DCR-issued series plus its clients-bucket RegisteredClient — runs the
// migration, and asserts: no raw token remains at rest, the series still resolve by their
// raw tokens, the DCR clients bucket is UNTOUCHED, and re-running the migration is a no-op.
func TestHashAtRest_MigrationReKeysOldLayout(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	// A CIMD series and a DCR-issued series (client_id is a DCR handle), both old-layout.
	dcrClientID, err := NewHandle()
	if err != nil {
		t.Fatalf("NewHandle: %v", err)
	}
	cimd := oldSeriesJSON{
		SeriesID: "sid-cimd", AccessToken: "RAW-ACCESS-CIMD", RefreshToken: "RAW-REFRESH-CIMD",
		ClientID: "https://client.example/meta", Principal: Principal{Name: "Op"}, Resource: "res",
		IssuedAt: now, AccessExpiry: now.Add(accessTTL), RefreshExpiry: now.Add(refreshTTL), Scope: "*",
	}
	dcr := oldSeriesJSON{
		SeriesID: "sid-dcr", AccessToken: "RAW-ACCESS-DCR", RefreshToken: "RAW-REFRESH-DCR",
		ClientID: dcrClientID, Principal: Principal{Name: "Op"}, Resource: "res",
		IssuedAt: now, AccessExpiry: now.Add(accessTTL), RefreshExpiry: now.Add(refreshTTL), Scope: "*",
	}
	seedOldLayoutSeries(t, s, cimd)
	seedOldLayoutSeries(t, s, dcr)
	// The DCR RegisteredClient (a PUBLIC identifier, no secret) — must survive untouched.
	if err := s.PutClient(RegisteredClient{ClientID: dcrClientID, RedirectURIs: []string{"https://client.example/cb"}, TokenEndpointAuthMethod: "none"}); err != nil {
		t.Fatalf("PutClient: %v", err)
	}

	if err := s.migrateHashTokensAtRest(); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// No raw token remains at rest.
	for _, raw := range []string{cimd.AccessToken, cimd.RefreshToken, dcr.AccessToken, dcr.RefreshToken} {
		if dbContainsRaw(t, s, raw) {
			t.Fatalf("raw token %q still present at rest after migration", raw)
		}
	}
	// Both series still resolve by their raw tokens (hash-then-compare).
	if got, err := s.Lookup(cimd.AccessToken, now); err != nil || got.SeriesID != "sid-cimd" {
		t.Fatalf("CIMD series must resolve by raw access post-migration: sid=%q err=%v", got.SeriesID, err)
	}
	if got, err := s.Lookup(dcr.AccessToken, now); err != nil || got.SeriesID != "sid-dcr" {
		t.Fatalf("DCR series must resolve by raw access post-migration: sid=%q err=%v", got.SeriesID, err)
	}
	if _, err := s.Rotate(dcr.RefreshToken, now, accessTTL, refreshTTL); err != nil {
		t.Fatalf("DCR series must rotate by raw refresh post-migration: %v", err)
	}
	// The DCR clients bucket is untouched (the public client_id still registered).
	if _, err := s.GetClient(dcrClientID); err != nil {
		t.Fatalf("DCR RegisteredClient must survive migration untouched: %v", err)
	}

	// Idempotent: re-running is a no-op and nothing breaks (CIMD still resolves).
	if err := s.migrateHashTokensAtRest(); err != nil {
		t.Fatalf("migrate (rerun): %v", err)
	}
	if got, err := s.Lookup(cimd.AccessToken, now); err != nil || got.SeriesID != "sid-cimd" {
		t.Fatalf("CIMD series must still resolve after a second migration run: sid=%q err=%v", got.SeriesID, err)
	}
	if dbContainsRaw(t, s, cimd.AccessToken) {
		t.Fatal("a second migration run must not reintroduce a raw token")
	}
}
