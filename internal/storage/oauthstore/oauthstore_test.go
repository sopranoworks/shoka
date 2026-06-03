package oauthstore

import (
	"path/filepath"
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

// Multiple connections produce multiple independent series, each enumerable and
// individually revocable; revoking one leaves the others intact (premise 2).
func TestMultipleSeries_EnumerateAndRevokeIndividually(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	p := Principal{Name: "Op", Email: "op@example.test"}

	var recs []SeriesRecord
	for i := 0; i < 3; i++ {
		r, err := s.NewSeries("https://client.example/meta", p, "https://rs.example/mcp", now, accessTTL, refreshTTL)
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

	a, err := s.NewSeries("https://client.example/meta", p, "https://rs.example/mcp", now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries a: %v", err)
	}
	b, err := s.NewSeries("https://client.example/meta", p, "https://rs.example/mcp", now, accessTTL, refreshTTL)
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
	r, err := s.NewSeries("c", Principal{}, "res", now, accessTTL, refreshTTL)
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
	r, err := s.NewSeries("c", Principal{}, "res", now, accessTTL, refreshTTL)
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
	r, err := s.NewSeries("https://c/meta", p, "https://rs/mcp", now, accessTTL, refreshTTL)
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
	r, err := s1.NewSeries("c", Principal{Name: "Op"}, "res", now, accessTTL, refreshTTL)
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
