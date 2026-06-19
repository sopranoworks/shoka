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

// seedSeriesByAge seeds three series relative to real wall-clock time (StartCleaner
// sweeps with time.Now(), not an injected clock): a live one (refresh far in the
// future), an access-expired-but-refresh-live "within grace" one (refresh just
// expired, still inside the grace window), and a fully-dead one (refresh past expiry
// + grace). It returns their series IDs. grace must match the CleanerConfig.Grace
// the test then starts the cleaner with.
func seedSeriesByAge(t *testing.T, s *Store, grace time.Duration) (liveID, withinID, deadID string) {
	t.Helper()
	now := time.Now()
	p := Principal{Name: "Op"}

	live, err := s.NewSeries("https://c/meta", p, "res", "*", now, accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries live: %v", err)
	}
	// refresh-expiry = now-1h: expired, but well inside the 24h grace window.
	within, err := s.NewSeries("https://c/meta", p, "res", "*",
		now.Add(-refreshTTL-time.Hour), accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries withinGrace: %v", err)
	}
	// refresh-expiry = now-grace-1h: fully dead, past the grace window.
	dead, err := s.NewSeries("https://c/meta", p, "res", "*",
		now.Add(-refreshTTL-grace-time.Hour), accessTTL, refreshTTL)
	if err != nil {
		t.Fatalf("NewSeries dead: %v", err)
	}
	return live.SeriesID, within.SeriesID, dead.SeriesID
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
	grace := 24 * time.Hour
	liveID, withinID, deadID := seedSeriesByAge(t, s, grace)

	// Interval=time.Hour: the first tick is an hour away, so any deletion observed
	// immediately after StartCleaner returns is the boot sweep, not a tick.
	s.StartCleaner(t.Context(), CleanerConfig{Enabled: true, Interval: time.Hour, Grace: grace})

	remaining := remainingIDs(t, s)
	if remaining[deadID] {
		t.Fatalf("boot sweep did not remove fully-dead series %s (no tick has fired yet)", deadID)
	}
	// Safety preserved in the boot path: live and refresh-live (within-grace) untouched.
	if !remaining[liveID] || !remaining[withinID] {
		t.Fatalf("boot sweep removed a non-dead series; remaining=%v (want live %s and within-grace %s kept)",
			remaining, liveID, withinID)
	}
}

// TestStartCleaner_DefaultConfigSweepsAtBoot ties the config defaults to a real
// sweep: with storage.oauth_cleaner unset, config applies Enabled=true / Interval=1h
// / Grace=24h (proven in config.TestOAuthCleanerConfig_Defaults), and main.go wires
// exactly that triple into StartCleaner. Starting the cleaner with that default
// triple purges an already-dead series at boot — so the default path does start a
// working cleaner, not a dormant one.
func TestStartCleaner_DefaultConfigSweepsAtBoot(t *testing.T) {
	s := openTemp(t)
	// The documented config defaults (Enabled=true, 1h, 24h).
	defaults := CleanerConfig{Enabled: true, Interval: time.Hour, Grace: 24 * time.Hour}
	_, _, deadID := seedSeriesByAge(t, s, defaults.Grace)

	s.StartCleaner(t.Context(), defaults)

	if remainingIDs(t, s)[deadID] {
		t.Fatalf("default-config cleaner left fully-dead series %s present after boot", deadID)
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
		{"disabled", CleanerConfig{Enabled: false, Interval: time.Hour, Grace: time.Hour}},
		{"zero interval", CleanerConfig{Enabled: true, Interval: 0, Grace: time.Hour}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTemp(t)
			_, _, deadID := seedSeriesByAge(t, s, tc.cfg.Grace)

			// Must not start a goroutine, panic, block — or run the boot sweep.
			s.StartCleaner(t.Context(), tc.cfg)

			if !remainingIDs(t, s)[deadID] {
				t.Fatalf("disabled cleaner ran the boot sweep: fully-dead series %s was removed", deadID)
			}
		})
	}
}
