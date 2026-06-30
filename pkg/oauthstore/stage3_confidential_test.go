package oauthstore

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// B-71 Stage 3 — confidential client storage: issue mints a client_id + secret, stores only the
// HASH (never the raw), returns the raw ONCE; lookup/expiry/scope; revoke cascade.

// TestStage3_IssueConfidentialClient_HashedNotRaw: the raw secret is returned once and stored only
// hashed. RED proof: store the raw secret (e.g. set entry.Secret.Hash = raw) → it appears at rest →
// this test fails.
func TestStage3_IssueConfidentialClient_HashedNotRaw(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	entry, raw, err := s.IssueConfidentialClient("namespace:foo:rw", "", time.Hour, now)
	if err != nil {
		t.Fatalf("IssueConfidentialClient: %v", err)
	}
	if raw == "" {
		t.Fatal("the raw secret must be returned once at issuance")
	}
	if entry.RegistrationMode != RegistrationModeConfidential || entry.Identifier == "" {
		t.Fatalf("confidential entry shape wrong: %+v", entry)
	}
	if entry.Scope != "namespace:foo:rw" {
		t.Fatalf("scope not stored: %q", entry.Scope)
	}
	if entry.ExpiresAt.IsZero() || !entry.ExpiresAt.Equal(now.UTC().Add(time.Hour)) {
		t.Fatalf("finite expiry not set: %v", entry.ExpiresAt)
	}
	// Only the HASH is stored, and it is not the raw value.
	if entry.Secret == nil || entry.Secret.Hash == "" {
		t.Fatalf("secret hash must be set: %+v", entry.Secret)
	}
	if entry.Secret.Hash == raw {
		t.Fatal("the stored secret must be a HASH, not the raw value")
	}

	// At rest (the persisted JSON) must not contain the raw secret anywhere.
	reread, err := s.GetRegistration(entry.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	blob, _ := json.Marshal(reread)
	if strings.Contains(string(blob), raw) {
		t.Fatalf("the raw secret must NEVER appear at rest:\n%s", blob)
	}
	// The persisted entry verifies the secret (constant-time) and rejects a wrong one.
	if !reread.VerifySecret(raw) || reread.VerifySecret("wrong-secret") {
		t.Fatal("VerifySecret must accept the issued secret and reject a wrong one")
	}

	// validity must be positive (no indefinite).
	if _, _, err := s.IssueConfidentialClient("namespace:foo:rw", "", 0, now); err == nil {
		t.Fatal("a non-positive validity must error (no indefinite)")
	}
}

// TestStage3_ConfidentialClientLookupAndExpiry: resolve by client_id; expiry is reported.
func TestStage3_ConfidentialClientLookupAndExpiry(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()

	entry, _, err := s.IssueConfidentialClient("*", "", time.Hour, now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	got, ok := s.ConfidentialClient(entry.Identifier)
	if !ok || got.ID != entry.ID {
		t.Fatalf("ConfidentialClient must resolve the issued client_id: ok=%v", ok)
	}
	if _, ok := s.ConfidentialClient("not-a-client"); ok {
		t.Fatal("an unknown client_id must not resolve")
	}
	// Not expired before ExpiresAt; expired at/after it.
	if got.CredentialExpired(now.Add(time.Minute)) {
		t.Fatal("must not be expired before ExpiresAt")
	}
	if !got.CredentialExpired(now.Add(2 * time.Hour)) {
		t.Fatal("must be expired after ExpiresAt")
	}
}

// TestStage3_RevokeByClientID_Cascade: revoking a confidential credential cuts the tokens it
// issued and leaves other clients' tokens alone.
func TestStage3_RevokeByClientID_Cascade(t *testing.T) {
	s := openTemp(t)
	now := time.Now()
	p := Principal{Name: "Op"}

	conf, _, err := s.IssueConfidentialClient("namespace:foo:r", "", time.Hour, now)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	mine, _ := s.NewSeries(conf.Identifier, p, "r", "namespace:foo:r", "", now, time.Hour, time.Hour)
	other, _ := s.NewSeries("https://elsewhere.example/meta", p, "r", "*", "", now, time.Hour, time.Hour)

	n, err := s.RevokeByClientID(conf.Identifier)
	if err != nil {
		t.Fatalf("RevokeByClientID: %v", err)
	}
	if n != 1 {
		t.Fatalf("revoked = %d, want 1", n)
	}
	if _, err := s.Lookup(mine.AccessToken, now); err == nil {
		t.Fatal("the confidential client's token must be revoked")
	}
	if _, err := s.Lookup(other.AccessToken, now); err != nil {
		t.Fatal("another client's token must survive")
	}
}
