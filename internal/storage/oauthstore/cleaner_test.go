package oauthstore

import (
	"encoding/json"
	"testing"
	"time"
)

// The 2026-06-15 authz/lifecycle foundation: the Scope field round-trips, an
// old-shaped record (no scope key) decodes to an empty Scope (backward compatible),
// and the dead-series cleaner deletes fully-dead series the moment refresh expires
// (B-71 Stage 5 removed the grace window).

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

// TestDeleteDeadSeries_DeletesRefreshExpiredImmediately proves the no-grace semantics
// (B-71 Stage 5): a series is swept the moment its refresh is past expiry — there is no
// grace window. A live (refresh-future) series and an access-expired-but-refresh-live
// series (a usable connection) are kept; a series whose refresh expired a minute ago is
// deleted (under the old 24h grace it would have lingered a day).
func TestDeleteDeadSeries_DeletesRefreshExpiredImmediately(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	p := Principal{Name: "Op"}

	// live: refresh far in the future.
	live, err := s.NewSeries("https://c/meta", p, "res", "*", now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries live: %v", err)
	}
	// accessExpiredRefreshLive: access expired, refresh still valid — a usable connection.
	accLive, err := s.NewSeries("https://c/meta", p, "res", "*",
		now.Add(-accessTTL-time.Minute), accessTTL, refreshTTL) // access-exp = now-1m, refresh-exp future
	if err != nil {
		t.Fatalf("NewSeries accessExpiredRefreshLive: %v", err)
	}
	// dead: refresh expired one minute ago — with NO grace, swept immediately.
	dead, err := s.NewSeries("https://c/meta", p, "res", "*",
		now.Add(-refreshTTL-time.Minute), accessTTL, refreshTTL) // refresh-exp = now-1m
	if err != nil {
		t.Fatalf("NewSeries dead: %v", err)
	}

	deleted, err := s.DeleteDeadSeries(now)
	if err != nil {
		t.Fatalf("DeleteDeadSeries: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1 (only the refresh-expired series, swept with no grace)", deleted)
	}
	remaining := remainingIDs(t, s)
	if remaining[dead.SeriesID] {
		t.Fatalf("refresh-expired series %s should be swept immediately (no grace)", dead.SeriesID)
	}
	if !remaining[live.SeriesID] || !remaining[accLive.SeriesID] {
		t.Fatalf("live / access-expired-but-refresh-live series should remain; remaining=%v", remaining)
	}
}

// TestDeleteDeadSeries_BoundaryIsRefreshExpiry pins the predicate boundary at exactly
// RefreshExpiry (now.After(RefreshExpiry)) with NO added grace: a refresh expiry one ns
// in the past is dead; exactly at now is NOT yet dead (strict After); one ns in the
// future is kept. RED proof for the grace removal: were the predicate still
// RefreshExpiry.Add(grace) for any grace>0, the just-past-expiry series would survive →
// this test fails.
func TestDeleteDeadSeries_BoundaryIsRefreshExpiry(t *testing.T) {
	s := openTemp(t)
	now := time.Unix(1_700_000_000, 0).UTC()
	p := Principal{Name: "Op"}

	atBoundary, err := s.NewSeries("https://c/meta", p, "res", "*",
		now.Add(-refreshTTL), accessTTL, refreshTTL) // refresh-exp == now exactly
	if err != nil {
		t.Fatalf("NewSeries atBoundary: %v", err)
	}
	justPast, err := s.NewSeries("https://c/meta", p, "res", "*",
		now.Add(-refreshTTL-time.Nanosecond), accessTTL, refreshTTL) // refresh-exp = now-1ns
	if err != nil {
		t.Fatalf("NewSeries justPast: %v", err)
	}
	justFuture, err := s.NewSeries("https://c/meta", p, "res", "*",
		now.Add(-refreshTTL+time.Nanosecond), accessTTL, refreshTTL) // refresh-exp = now+1ns
	if err != nil {
		t.Fatalf("NewSeries justFuture: %v", err)
	}

	if _, err := s.DeleteDeadSeries(now); err != nil {
		t.Fatalf("DeleteDeadSeries: %v", err)
	}
	remaining := remainingIDs(t, s)
	if remaining[justPast.SeriesID] {
		t.Fatalf("refresh expired (1ns past) must be swept with no grace")
	}
	if !remaining[atBoundary.SeriesID] {
		t.Fatalf("refresh-expiry exactly == now must be kept (boundary is strict After, no grace)")
	}
	if !remaining[justFuture.SeriesID] {
		t.Fatalf("refresh still in the future (1ns) must be kept")
	}
}

// seedSeriesByAge seeds three series relative to real wall-clock time (StartCleaner
// sweeps with time.Now(), not an injected clock): a live one (refresh far in the
// future), an access-expired-but-refresh-live one (a usable connection — kept), and a
// dead one (refresh expired a minute ago — swept immediately, no grace). Returns their
// series IDs.
func seedSeriesByAge(t *testing.T, s *Store) (liveID, accessExpiredID, deadID string) {
	t.Helper()
	now := time.Now()
	p := Principal{Name: "Op"}

	live, err := s.NewSeries("https://c/meta", p, "res", "*", now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries live: %v", err)
	}
	// access expired (now-1m), refresh still in the future — a usable connection, kept.
	accLive, err := s.NewSeries("https://c/meta", p, "res", "*",
		now.Add(-accessTTL-time.Minute), accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries accessExpired: %v", err)
	}
	// refresh expired a minute ago — dead, swept immediately (no grace).
	dead, err := s.NewSeries("https://c/meta", p, "res", "*",
		now.Add(-refreshTTL-time.Minute), accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries dead: %v", err)
	}
	return live.SeriesID, accLive.SeriesID, dead.SeriesID
}

func remainingIDs(t *testing.T, s *Store) map[string]bool {
	t.Helper()
	infos, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := map[string]bool{}
	for _, i := range infos {
		ids[i.SeriesID] = true
	}
	return ids
}

// TestStartCleaner_BootSweepRemovesDeadSeriesWithoutTick is the boot-sweep proof: a
// fully-dead series is removed at startup, WITHOUT waiting a full Interval. The
// Interval is an hour so the ticker cannot fire during the test — the deletion is
// purely the synchronous boot sweep StartCleaner runs before the ticker loop.
// RED proof: remove the `sweep()` boot call in StartCleaner and the dead series is
// still present immediately after StartCleaner returns → this test fails.
func TestStartCleaner_BootSweepRemovesDeadSeriesWithoutTick(t *testing.T) {
	s := openTemp(t)
	liveID, accID, deadID := seedSeriesByAge(t, s)

	// Interval=time.Hour: the first tick is an hour away, so any deletion observed
	// immediately after StartCleaner returns is the boot sweep, not a tick.
	s.StartCleaner(t.Context(), CleanerConfig{Enabled: true, Interval: time.Hour})

	remaining := remainingIDs(t, s)
	if remaining[deadID] {
		t.Fatalf("boot sweep did not remove the refresh-expired series %s (no tick has fired yet)", deadID)
	}
	// Safety preserved in the boot path: live and access-expired-but-refresh-live untouched.
	if !remaining[liveID] || !remaining[accID] {
		t.Fatalf("boot sweep removed a non-dead series; remaining=%v (want live %s and refresh-live %s kept)",
			remaining, liveID, accID)
	}
}

// TestStartCleaner_DefaultConfigSweepsAtBoot ties the config defaults to a real
// sweep: with storage.oauth_cleaner unset, config applies Enabled=true / Interval=1h
// (no grace — B-71 Stage 5; proven in config.TestOAuthCleanerConfig_Defaults), and
// main.go wires exactly that into StartCleaner. Starting the cleaner with that config
// purges an already-dead series at boot — so the default path starts a working
// cleaner, not a dormant one.
func TestStartCleaner_DefaultConfigSweepsAtBoot(t *testing.T) {
	s := openTemp(t)
	// The documented config defaults (Enabled=true, 1h; no grace).
	defaults := CleanerConfig{Enabled: true, Interval: time.Hour}
	_, _, deadID := seedSeriesByAge(t, s)

	s.StartCleaner(t.Context(), defaults)

	if remainingIDs(t, s)[deadID] {
		t.Fatalf("default-config cleaner left the refresh-expired series %s present after boot", deadID)
	}
}

// TestStartCleaner_DisabledOrZeroIntervalNoOp proves the Enabled/Interval guard
// covers the boot sweep too: with Enabled=false or Interval<=0, NOTHING runs — not
// even the boot sweep — so a fully-dead series survives. RED proof: move the boot
// `sweep()` above the guard (so it always runs) and the dead series is deleted here
// → this test fails.
func TestStartCleaner_DisabledOrZeroIntervalNoOp(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  CleanerConfig
	}{
		{"disabled", CleanerConfig{Enabled: false, Interval: time.Hour}},
		{"zero interval", CleanerConfig{Enabled: true, Interval: 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTemp(t)
			_, _, deadID := seedSeriesByAge(t, s)

			// Must not start a goroutine, panic, block — or run the boot sweep.
			s.StartCleaner(t.Context(), tc.cfg)

			if !remainingIDs(t, s)[deadID] {
				t.Fatalf("disabled cleaner ran the boot sweep: fully-dead series %s was removed", deadID)
			}
		})
	}
}
