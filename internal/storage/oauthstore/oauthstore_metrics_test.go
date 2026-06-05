package oauthstore

import (
	"testing"
	"time"
)

// The 2026-06-05 M3 metrics directive: OAuth observability counts, surfaced
// through the metrics OAuthSource bridge. These pin the three counts-only methods:
// active connections (len of the live set), tokens issued (first-issue ONLY — not
// rotations), and revocations (actual deletions, not the idempotent no-op).

func TestOAuthMetrics_TokensIssuedCountsFirstIssueNotRotation(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	p := Principal{Name: "Op", Email: "op@example.test"}

	if got := s.OAuthTokensIssued(); got != 0 {
		t.Fatalf("expected 0 issued initially, got %d", got)
	}

	a, err := s.NewSeries("https://client.example/meta", p, "https://rs.example/mcp", now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries a: %v", err)
	}
	if _, err := s.NewSeries("https://client.example/meta", p, "https://rs.example/mcp", now, accessTTL, refreshTTL); err != nil {
		t.Fatalf("NewSeries b: %v", err)
	}
	if got := s.OAuthTokensIssued(); got != 2 {
		t.Fatalf("expected 2 issued after two NewSeries, got %d", got)
	}

	// A rotation mints a fresh pair inline (it does NOT route through NewSeries), so
	// it is NOT a new connection and must NOT increment tokens_issued — the count is
	// the "new connections" signal, not a "tokens minted" signal.
	if _, err := s.Rotate(a.RefreshToken, now, accessTTL, refreshTTL); err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if got := s.OAuthTokensIssued(); got != 2 {
		t.Fatalf("rotation must not count as an issuance; expected 2, got %d", got)
	}
}

func TestOAuthMetrics_RevocationsCountActualDeletionsOnly(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	p := Principal{Name: "Op", Email: "op@example.test"}

	a, err := s.NewSeries("https://client.example/meta", p, "https://rs.example/mcp", now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries: %v", err)
	}

	if got := s.OAuthRevocations(); got != 0 {
		t.Fatalf("expected 0 revocations initially, got %d", got)
	}
	// A real revocation counts.
	if err := s.Revoke(a.SeriesID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got := s.OAuthRevocations(); got != 1 {
		t.Fatalf("expected 1 revocation after revoking a live series, got %d", got)
	}
	// The idempotent no-op (already-revoked / unknown series) must NOT count.
	if err := s.Revoke(a.SeriesID); err != nil {
		t.Fatalf("idempotent Revoke: %v", err)
	}
	if err := s.Revoke("no-such-series"); err != nil {
		t.Fatalf("unknown Revoke: %v", err)
	}
	if got := s.OAuthRevocations(); got != 1 {
		t.Fatalf("no-op revokes must not count; expected 1, got %d", got)
	}
}

func TestOAuthMetrics_ActiveConnectionsTracksLiveSet(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	p := Principal{Name: "Op", Email: "op@example.test"}

	if got := s.OAuthActiveConnections(); got != 0 {
		t.Fatalf("expected 0 active initially, got %d", got)
	}
	var first SeriesRecord
	for i := 0; i < 3; i++ {
		r, err := s.NewSeries("https://client.example/meta", p, "https://rs.example/mcp", now, accessTTL, refreshTTL)
		if err != nil {
			t.Fatalf("NewSeries: %v", err)
		}
		if i == 0 {
			first = r
		}
	}
	if got := s.OAuthActiveConnections(); got != 3 {
		t.Fatalf("expected 3 active connections, got %d", got)
	}
	// Revoking shrinks the live set the gauge reads.
	if err := s.Revoke(first.SeriesID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if got := s.OAuthActiveConnections(); got != 2 {
		t.Fatalf("expected 2 active after one revoke, got %d", got)
	}
}

// A nil *Store (the OAuth-disabled shape) is safe to call — defence-in-depth, even
// though cmd/server drops a nil store before boxing it as a metrics extra.
func TestOAuthMetrics_NilReceiverSafe(t *testing.T) {
	var s *Store
	if got := s.OAuthActiveConnections(); got != 0 {
		t.Fatalf("nil store active: want 0, got %d", got)
	}
	if got := s.OAuthTokensIssued(); got != 0 {
		t.Fatalf("nil store issued: want 0, got %d", got)
	}
	if got := s.OAuthRevocations(); got != 0 {
		t.Fatalf("nil store revocations: want 0, got %d", got)
	}
}
