package oauthstore

import (
	"encoding/json"
	"testing"
	"time"
)

// The 2026-06-15 authz/lifecycle foundation: the Scope field round-trips, an
// old-shaped record (no scope key) decodes to an empty Scope (backward compatible),
// and the dead-series cleaner deletes only fully-dead series past the grace.

func TestNewSeries_ScopeRoundTrips(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	p := Principal{Name: "Op", Email: "op@example.test"}

	star, err := s.NewSeries("https://c/meta", p, "res", "*", now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries star: %v", err)
	}
	scoped, err := s.NewSeries("https://c/meta", p, "res", "namespace:foo", now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries scoped: %v", err)
	}

	// Lookup returns the stored scope verbatim.
	got, err := s.Lookup(star.AccessToken, now)
	if err != nil || got.Scope != "*" {
		t.Fatalf("star Lookup: scope=%q err=%v, want scope=%q", got.Scope, err, "*")
	}
	got, err = s.Lookup(scoped.AccessToken, now)
	if err != nil || got.Scope != "namespace:foo" {
		t.Fatalf("scoped Lookup: scope=%q err=%v, want %q", got.Scope, err, "namespace:foo")
	}

	// List (the admin-surface view) carries the scope too.
	infos, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	scopes := map[string]bool{}
	for _, i := range infos {
		scopes[i.Scope] = true
	}
	if !scopes["*"] || !scopes["namespace:foo"] {
		t.Fatalf("List scopes = %v, want both * and namespace:foo", scopes)
	}
}

// TestSeriesRecord_OldRecordDecodesEmptyScope proves the (ii) backward-compatible
// migration: a record serialized BEFORE the Scope field existed (its JSON has no
// "scope" key) decodes into the current struct with Scope == "". The validation
// path and the authz gate both interpret "" as "*", so such a token stays
// all-access — no migration is needed.
func TestSeriesRecord_OldRecordDecodesEmptyScope(t *testing.T) {
	// A hand-written pre-field record: note the absence of any "scope" key.
	oldJSON := `{
		"series_id": "sid",
		"access_token": "at",
		"refresh_token": "rt",
		"client_id": "https://c/meta",
		"principal": {"name": "Op", "email": "op@example.test"},
		"resource": "res",
		"issued_at": "2026-06-01T00:00:00Z",
		"access_expiry": "2026-06-01T01:00:00Z",
		"refresh_expiry": "2026-07-01T00:00:00Z"
	}`
	var rec SeriesRecord
	if err := json.Unmarshal([]byte(oldJSON), &rec); err != nil {
		t.Fatalf("decode old record: %v", err)
	}
	if rec.Scope != "" {
		t.Fatalf("old record Scope = %q, want empty (interpreted as * downstream)", rec.Scope)
	}
	if rec.SeriesID != "sid" || rec.Principal.Name != "Op" {
		t.Fatalf("old record decoded wrong: %+v", rec)
	}
}

func TestDeleteDeadSeries_DeletesOnlyFullyDeadPastGrace(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	p := Principal{Name: "Op"}
	grace := 24 * time.Hour

	// live: refresh expires far in the future — never dead.
	live, err := s.NewSeries("https://c/meta", p, "res", "*", now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries live: %v", err)
	}
	// withinGrace: refresh already expired, but not yet past the grace window.
	withinGrace, err := s.NewSeries("https://c/meta", p, "res", "*",
		now.Add(-refreshTTL-time.Hour), accessTTL, refreshTTL) // refresh-expiry = now-1h
	if err != nil {
		t.Fatalf("NewSeries withinGrace: %v", err)
	}
	// dead: refresh expired well past the grace window.
	dead, err := s.NewSeries("https://c/meta", p, "res", "*",
		now.Add(-refreshTTL-grace-time.Hour), accessTTL, refreshTTL) // refresh-expiry = now-grace-1h
	if err != nil {
		t.Fatalf("NewSeries dead: %v", err)
	}

	deleted, err := s.DeleteDeadSeries(now, grace)
	if err != nil {
		t.Fatalf("DeleteDeadSeries: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (only the past-grace dead series)", deleted)
	}

	// dead is gone; live and withinGrace remain (access tokens still resolve the row,
	// modulo their own access expiry — assert via List which enumerates the rows).
	infos, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	remaining := map[string]bool{}
	for _, i := range infos {
		remaining[i.SeriesID] = true
	}
	if remaining[dead.SeriesID] {
		t.Fatalf("dead series %s should have been deleted", dead.SeriesID)
	}
	if !remaining[live.SeriesID] || !remaining[withinGrace.SeriesID] {
		t.Fatalf("live/within-grace series should remain; remaining=%v", remaining)
	}
}

func TestStartCleaner_DisabledOrZeroIntervalNoOp(t *testing.T) {
	s := openTemp(t)
	// Neither of these should start a goroutine or panic; with a cancelled-immediately
	// ctx there is nothing to observe beyond "does not block / does not run a cycle".
	s.StartCleaner(t.Context(), CleanerConfig{Enabled: false, Interval: time.Hour, Grace: time.Hour})
	s.StartCleaner(t.Context(), CleanerConfig{Enabled: true, Interval: 0, Grace: time.Hour})
}
