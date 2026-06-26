package oauthstore

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

const (
	accessTTL  = time.Hour
	refreshTTL = 24 * time.Hour
)

func TestNewHandle_UniqueAndOpaque(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		h, err := NewHandle()
		if err != nil {
			t.Fatalf("NewHandle: %v", err)
		}
		if len(h) < 40 {
			t.Fatalf("handle too short to be 256-bit: %q", h)
		}
		if seen[h] {
			t.Fatalf("duplicate handle %q", h)
		}
		seen[h] = true
	}
}

// RevokeByPrincipalEmail revokes every series AND pending auth code for one principal
// (case-insensitive), leaves other principals untouched, and is idempotent — the
// cross-store access cut for user disable/delete (B-28).
func TestRevokeByPrincipalEmail(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	alice := Principal{Name: "Alice", Email: "alice@example.com"}
	bob := Principal{Name: "Bob", Email: "bob@example.com"}
	a1, _ := s.NewSeries("https://c/cimd", alice, "https://shoka/mcp", "*", now, accessTTL, refreshTTL)
	a2, _ := s.NewSeries("https://c/cimd", alice, "https://shoka/mcp", "*", now, accessTTL, refreshTTL)
	b1, _ := s.NewSeries("https://c/cimd", bob, "https://shoka/mcp", "*", now, accessTTL, refreshTTL)
	if err := s.PutCode("alice-code", CodeRecord{ClientID: "x", Principal: alice, Expiry: now.Add(time.Minute)}); err != nil {
		t.Fatalf("PutCode: %v", err)
	}

	n, err := s.RevokeByPrincipalEmail("ALICE@example.com") // case-insensitive match
	if err != nil {
		t.Fatalf("RevokeByPrincipalEmail: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 series revoked, got %d", n)
	}
	// Alice's access tokens no longer authorize.
	if _, err := s.Lookup(a1.AccessToken, now); err != ErrNotFound {
		t.Fatalf("alice a1 should be gone, got %v", err)
	}
	if _, err := s.Lookup(a2.AccessToken, now); err != ErrNotFound {
		t.Fatalf("alice a2 should be gone, got %v", err)
	}
	// Alice's pending code is gone (cannot mint a fresh token).
	if _, err := s.TakeCode("alice-code", now); err != ErrNotFound {
		t.Fatalf("alice code should be gone, got %v", err)
	}
	// Bob's series is untouched.
	if _, err := s.Lookup(b1.AccessToken, now); err != nil {
		t.Fatalf("bob's token must still authorize, got %v", err)
	}
	// Idempotent: revoking again revokes nothing.
	n2, err := s.RevokeByPrincipalEmail("alice@example.com")
	if err != nil || n2 != 0 {
		t.Fatalf("re-revoke should be 0,nil; got %d,%v", n2, err)
	}
}

// A DCR-registered client (B-63) round-trips: PutClient persists it, GetClient
// resolves it, and an unknown id is ErrNotFound (the /token re-register signal).
func TestRegisteredClient_PutGetAndUnknown(t *testing.T) {
	s := openTemp(t)
	issued := time.Unix(1_700_000_000, 0).UTC()
	rec := RegisteredClient{
		ClientID:                "dcr-handle-abc",
		RedirectURIs:            []string{"https://app.example/callback"},
		TokenEndpointAuthMethod: "none",
		ClientName:              "Example App",
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
		ClientIDIssuedAt:        issued,
	}
	if err := s.PutClient(rec); err != nil {
		t.Fatalf("PutClient: %v", err)
	}
	got, err := s.GetClient("dcr-handle-abc")
	if err != nil {
		t.Fatalf("GetClient: %v", err)
	}
	if got.ClientID != rec.ClientID || got.TokenEndpointAuthMethod != "none" ||
		len(got.RedirectURIs) != 1 || got.RedirectURIs[0] != "https://app.example/callback" ||
		got.ClientName != "Example App" || !got.ClientIDIssuedAt.Equal(issued) {
		t.Fatalf("GetClient round-trip mismatch: %+v", got)
	}
	if _, err := s.GetClient("never-registered"); err != ErrNotFound {
		t.Fatalf("unknown client must be ErrNotFound, got %v", err)
	}
}

// A registered client survives a store reopen — the directive's hard constraint
// (B-54's lesson: an issued artefact its own later Lookup cannot find is the
// failure class to avoid). /authorize and /token on a later process must resolve it.
func TestRegisteredClient_DurableAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "oauth.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rec := RegisteredClient{
		ClientID:                "dcr-persist-xyz",
		RedirectURIs:            []string{"https://app.example/cb"},
		TokenEndpointAuthMethod: "none",
		ClientIDIssuedAt:        time.Unix(1_700_000_500, 0).UTC(),
	}
	if err := s.PutClient(rec); err != nil {
		t.Fatalf("PutClient: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	got, err := s2.GetClient("dcr-persist-xyz")
	if err != nil {
		t.Fatalf("GetClient after reopen: %v", err)
	}
	if got.ClientID != rec.ClientID || len(got.RedirectURIs) != 1 {
		t.Fatalf("registered client not durable across reopen: %+v", got)
	}
}

// Multiple connections produce multiple independent series, each enumerable and
// individually revocable; revoking one leaves the others intact (premise 2).
func TestMultipleSeries_EnumerateAndRevokeIndividually(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	p := Principal{Name: "Op", Email: "op@example.test"}

	var recs []SeriesRecord
	for i := 0; i < 3; i++ {
		r, err := s.NewSeries("https://client.example/meta", p, "https://rs.example/mcp", "*", now, accessTTL, refreshTTL)
		if err != nil {
			t.Fatalf("NewSeries: %v", err)
		}
		recs = append(recs, r)
	}

	list, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3 live series, got %d", len(list))
	}
	for _, info := range list {
		if info.ClientID == "" || info.Principal != p {
			t.Fatalf("SeriesInfo missing binding: %+v", info)
		}
	}

	// Revoke the middle series; the other two stay valid.
	if err := s.Revoke(recs[1].SeriesID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := s.Lookup(recs[1].AccessToken, now); err != ErrNotFound {
		t.Fatalf("revoked access token: want ErrNotFound, got %v", err)
	}
	for _, i := range []int{0, 2} {
		if _, err := s.Lookup(recs[i].AccessToken, now); err != nil {
			t.Fatalf("series %d should remain valid, got %v", i, err)
		}
	}
	list, _ = s.List()
	if len(list) != 2 {
		t.Fatalf("after one revoke want 2 series, got %d", len(list))
	}

	// Revoke is idempotent.
	if err := s.Revoke(recs[1].SeriesID); err != nil {
		t.Fatalf("idempotent Revoke: %v", err)
	}
}

// A rotated refresh invalidates only its own predecessor and rolls the access
// token; other series are untouched and rotation is per-series (premise 2).
func TestRotate_InvalidatesPredecessorOnly(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	p := Principal{Name: "Op", Email: "op@example.test"}

	a, err := s.NewSeries("https://client.example/meta", p, "https://rs.example/mcp", "*", now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries a: %v", err)
	}
	b, err := s.NewSeries("https://client.example/meta", p, "https://rs.example/mcp", "*", now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries b: %v", err)
	}

	rot, err := s.Rotate(a.RefreshToken, now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if rot.SeriesID != a.SeriesID {
		t.Fatalf("rotation changed series id: %s -> %s", a.SeriesID, rot.SeriesID)
	}
	if rot.AccessToken == a.AccessToken || rot.RefreshToken == a.RefreshToken {
		t.Fatalf("rotation did not change handles")
	}

	// Predecessor refresh is dead; predecessor access is dead.
	if _, err := s.Rotate(a.RefreshToken, now, accessTTL, refreshTTL); err != ErrNotFound {
		t.Fatalf("old refresh reuse: want ErrNotFound, got %v", err)
	}
	if _, err := s.Lookup(a.AccessToken, now); err != ErrNotFound {
		t.Fatalf("old access after rotation: want ErrNotFound, got %v", err)
	}
	// New handles work.
	if _, err := s.Lookup(rot.AccessToken, now); err != nil {
		t.Fatalf("new access invalid: %v", err)
	}
	// Series b is entirely unaffected.
	if _, err := s.Lookup(b.AccessToken, now); err != nil {
		t.Fatalf("series b disturbed by rotation of a: %v", err)
	}
	if _, err := s.Rotate(b.RefreshToken, now, accessTTL, refreshTTL); err != nil {
		t.Fatalf("series b rotate independently: %v", err)
	}
}

func TestRotate_ExpiredRefreshRevokesSeries(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	r, err := s.NewSeries("c", Principal{}, "res", "*", now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries: %v", err)
	}
	later := now.Add(refreshTTL + time.Minute)
	if _, err := s.Rotate(r.RefreshToken, later, accessTTL, refreshTTL); err != ErrExpired {
		t.Fatalf("want ErrExpired, got %v", err)
	}
	// Series is gone after an expired-refresh rotation attempt.
	if _, err := s.Lookup(r.AccessToken, now); err != ErrNotFound {
		t.Fatalf("series should be revoked, got %v", err)
	}
}

func TestLookup_ExpiredAccess(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	r, err := s.NewSeries("c", Principal{}, "res", "*", now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries: %v", err)
	}
	if _, err := s.Lookup(r.AccessToken, now.Add(accessTTL+time.Second)); err != ErrExpired {
		t.Fatalf("want ErrExpired, got %v", err)
	}
}

func TestLookup_BindingReturned(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	p := Principal{Name: "Alice", Email: "alice@example.test"}
	r, err := s.NewSeries("https://c/meta", p, "https://rs/mcp", "*", now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries: %v", err)
	}
	got, err := s.Lookup(r.AccessToken, now)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Principal != p || got.Resource != "https://rs/mcp" || got.ClientID != "https://c/meta" {
		t.Fatalf("binding not preserved: %+v", got)
	}
}

// Authorization codes are single-use: a second exchange of the same code fails,
// even back-to-back (delete-on-read).
func TestCode_SingleUse(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	code, _ := NewHandle()
	rec := CodeRecord{
		ClientID:            "https://c/meta",
		RedirectURI:         "https://c/cb",
		CodeChallenge:       "abc",
		CodeChallengeMethod: "S256",
		Resource:            "https://rs/mcp",
		Principal:           Principal{Name: "Op"},
		Expiry:              now.Add(time.Minute),
	}
	if err := s.PutCode(code, rec); err != nil {
		t.Fatalf("PutCode: %v", err)
	}
	got, err := s.TakeCode(code, now)
	if err != nil {
		t.Fatalf("TakeCode: %v", err)
	}
	if got.CodeChallenge != "abc" || got.RedirectURI != "https://c/cb" {
		t.Fatalf("code record not preserved: %+v", got)
	}
	if _, err := s.TakeCode(code, now); err != ErrNotFound {
		t.Fatalf("code replay: want ErrNotFound, got %v", err)
	}
}

func TestCode_Expired(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	code, _ := NewHandle()
	if err := s.PutCode(code, CodeRecord{Expiry: now.Add(time.Minute)}); err != nil {
		t.Fatalf("PutCode: %v", err)
	}
	if _, err := s.TakeCode(code, now.Add(2*time.Minute)); err != ErrExpired {
		t.Fatalf("want ErrExpired, got %v", err)
	}
	// Even expired, the code is consumed (cannot be replayed).
	if _, err := s.TakeCode(code, now); err != ErrNotFound {
		t.Fatalf("expired code must be consumed: got %v", err)
	}
}

// Series survive reopening the store (durable, go-git-free).
func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oauth.db")
	now := time.Unix(1_700_000_000, 0).UTC()

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	r, err := s1.NewSeries("c", Principal{Name: "Op"}, "res", "*", now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries: %v", err)
	}
	_ = s1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if _, err := s2.Lookup(r.AccessToken, now); err != nil {
		t.Fatalf("series did not survive reopen: %v", err)
	}
}

func TestExtraPermissions_RoundTrip(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	p := Principal{Name: "Op", Email: "op@example.test"}

	ep := map[string]any{
		"allowed_branches": []any{"task-42/", "fix-99/"},
		"rate_limit":       float64(100),
	}
	rec, err := s.NewSeries("shoka-cli", p, "res", "*", now, accessTTL, refreshTTL, ep)
	if err != nil {
		t.Fatalf("NewSeries with extra_permissions: %v", err)
	}

	got, err := s.Lookup(rec.AccessToken, now)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.ExtraPermissions == nil {
		t.Fatal("ExtraPermissions is nil after Lookup, expected the stored map")
	}
	branches, ok := got.ExtraPermissions["allowed_branches"].([]any)
	if !ok || len(branches) != 2 || branches[0] != "task-42/" {
		t.Fatalf("allowed_branches round-trip failed: %v", got.ExtraPermissions["allowed_branches"])
	}
	rate, ok := got.ExtraPermissions["rate_limit"].(float64)
	if !ok || rate != 100 {
		t.Fatalf("rate_limit round-trip failed: %v", got.ExtraPermissions["rate_limit"])
	}
}

func TestExtraPermissions_NilBackwardCompat(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	p := Principal{Name: "Op", Email: "op@example.test"}

	rec, err := s.NewSeries("shoka-cli", p, "res", "*", now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries without extra_permissions: %v", err)
	}

	got, err := s.Lookup(rec.AccessToken, now)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.ExtraPermissions != nil {
		t.Fatalf("ExtraPermissions should be nil for a token without it, got %v", got.ExtraPermissions)
	}
}

func TestExtraPermissions_SurvivesRotation(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	p := Principal{Name: "Op", Email: "op@example.test"}

	ep := map[string]any{"allowed_branches": []any{"task-42/"}}
	rec, err := s.NewSeries("shoka-cli", p, "res", "*", now, accessTTL, refreshTTL, ep)
	if err != nil {
		t.Fatalf("NewSeries: %v", err)
	}

	rotated, err := s.Rotate(rec.RefreshToken, now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	got, err := s.Lookup(rotated.AccessToken, now)
	if err != nil {
		t.Fatalf("Lookup after rotation: %v", err)
	}
	if got.ExtraPermissions == nil {
		t.Fatal("ExtraPermissions lost after rotation")
	}
	branches, ok := got.ExtraPermissions["allowed_branches"].([]any)
	if !ok || len(branches) != 1 || branches[0] != "task-42/" {
		t.Fatalf("allowed_branches round-trip after rotation failed: %v", got.ExtraPermissions)
	}
}

func TestExtraPermissions_OmittedFromJSON_WhenNil(t *testing.T) {
	rec := SeriesRecord{
		SeriesID: "test",
		Scope:    "*",
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "extra_permissions") {
		t.Fatalf("nil ExtraPermissions should be omitted from JSON, got: %s", data)
	}
}
