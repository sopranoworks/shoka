package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSource struct{}

func (fakeSource) WALPending() int                  { return 3 }
func (fakeSource) WALPendingBytes() int64           { return 4096 }
func (fakeSource) WALOldestEntryAge() time.Duration { return 2500 * time.Millisecond }
func (fakeSource) WALMaxEntries() int               { return 1000 }
func (fakeSource) WALWriteDisabled() bool           { return false }
func (fakeSource) CommitStats() (int64, int64)      { return 42, 1 }
func (fakeSource) LockStats() (int, int64)          { return 2, 5 }
func (fakeSource) ProjectStates() map[string]string {
	return map[string]string{"shoka/maintenance": "healthy", "rohrpost/dev": "corrupted"}
}
func (fakeSource) CatalogCounters() (int64, int64, int64, int64, int64, int64, int64) {
	// updateFailedWrite, updateFailedDelete, invariantViolations,
	// rebuildMissing, rebuildCorrupt, rebuildSchema, rebuildUnreadable
	return 7, 3, 9, 2, 1, 0, 4
}
func (fakeSource) CatalogFileCounts() map[string][2]int {
	return map[string][2]int{"shoka/maintenance": {12, 4}}
}

// Class-A sources (the 2026-06-05 M1 directive).
func (fakeSource) QuarantineStats() (int64, int64)      { return 6, 2 }
func (fakeSource) IndexCounters() (int64, int64, int64) { return 8, 5, 13 } // failW, failD, rebuilds total
func (fakeSource) IndexRebuildCounters() (int64, int64) { return 10, 3 }    // stale, recreated
func (fakeSource) LazyRescanCount() int64               { return 11 }

// Class-B index-line sources (the 2026-06-05 M2 directive).
func (fakeSource) IndexSweepRuns() int64 { return 14 }
func (fakeSource) IndexHealthStates() map[string]bool {
	return map[string]bool{"shoka/maintenance": true, "rohrpost/dev": false}
}
func (fakeSource) SearchFastpathStats() (int64, int64)     { return 15, 4 } // fastpath, fallback
func (fakeSource) FixLinksKickStats() (int64, int64)       { return 20, 1 } // enqueued, dropped
func (fakeSource) FixLinksWriteStats() (int64, int64)      { return 18, 2 } // rewrites, conflicts
func (fakeSource) FixLinksReferrerLookups() (int64, int64) { return 16, 3 } // index, truthscan

// fakeNotifyDrops satisfies the NotifyDropSource bridge capability.
type fakeNotifyDrops struct{ n int64 }

func (f fakeNotifyDrops) NotifyDrops() int64 { return f.n }

func TestMetrics_Exposition(t *testing.T) {
	srv := httptest.NewServer(Handler(fakeSource{}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out := string(body)

	// Gauges with current values.
	assert.Contains(t, out, "shoka_wal_pending_entries 3")
	assert.Contains(t, out, "shoka_wal_pending_bytes 4096")
	assert.Contains(t, out, "shoka_wal_oldest_entry_age_seconds 2.5")
	assert.Contains(t, out, "shoka_wal_max_entries 1000")
	assert.Contains(t, out, "shoka_wal_write_disabled 0")
	assert.Contains(t, out, "shoka_filelock_active_leases 2")

	// Counters.
	assert.Contains(t, out, `shoka_wal_commits_total{result="success"} 42`)
	assert.Contains(t, out, `shoka_wal_commits_total{result="failure"} 1`)
	assert.Contains(t, out, "shoka_filelock_forced_release_total 5")

	// Per-project state: exactly the current state is 1.
	assert.Contains(t, out, `shoka_project_state{namespace="shoka",project="maintenance",state="healthy"} 1`)
	assert.Contains(t, out, `shoka_project_state{namespace="shoka",project="maintenance",state="corrupted"} 0`)
	assert.Contains(t, out, `shoka_project_state{namespace="rohrpost",project="dev",state="corrupted"} 1`)

	// Catalog counters (§10).
	assert.Contains(t, out, "shoka_catalog_invariant_violations_total 9")
	assert.Contains(t, out, `shoka_catalog_update_failed_total{operation="write"} 7`)
	assert.Contains(t, out, `shoka_catalog_update_failed_total{operation="delete"} 3`)
	assert.Contains(t, out, `shoka_catalog_rebuild_total{reason="missing"} 2`)
	assert.Contains(t, out, `shoka_catalog_rebuild_total{reason="corrupt"} 1`)
	assert.Contains(t, out, `shoka_catalog_rebuild_total{reason="schema_mismatch"} 0`)
	assert.Contains(t, out, `shoka_catalog_rebuild_total{reason="unreadable"} 4`)

	// Catalog per-project gauges (§10).
	assert.Contains(t, out, `shoka_catalog_files{namespace="shoka",project="maintenance"} 12`)
	assert.Contains(t, out, `shoka_catalog_dirs{namespace="shoka",project="maintenance"} 4`)

	// Class-A families (M1).
	assert.Contains(t, out, "shoka_wal_quarantined_total 6")
	assert.Contains(t, out, "shoka_wal_quarantine_failed_total 2")
	assert.Contains(t, out, `shoka_index_update_failed_total{operation="write"} 8`)
	assert.Contains(t, out, `shoka_index_update_failed_total{operation="delete"} 5`)
	assert.Contains(t, out, `shoka_index_rebuilds_total{reason="stale"} 10`)
	assert.Contains(t, out, `shoka_index_rebuilds_total{reason="recreated"} 3`)
	assert.Contains(t, out, "shoka_lazy_rescans_total 11")

	// Class-B index-line families (M2).
	assert.Contains(t, out, "shoka_index_sweep_runs_total 14")
	// Per-project index health: exactly the project's current health is emitted.
	assert.Contains(t, out, `shoka_index_healthy{namespace="shoka",project="maintenance"} 1`)
	assert.Contains(t, out, `shoka_index_healthy{namespace="rohrpost",project="dev"} 0`)
	assert.Contains(t, out, `shoka_search_fastpath_total{outcome="fastpath"} 15`)
	assert.Contains(t, out, `shoka_search_fastpath_total{outcome="fallback"} 4`)
	assert.Contains(t, out, `shoka_fixlinks_kicks_total{outcome="enqueued"} 20`)
	assert.Contains(t, out, `shoka_fixlinks_kicks_total{outcome="dropped"} 1`)
	assert.Contains(t, out, "shoka_fixlinks_rewrites_total 18")
	assert.Contains(t, out, "shoka_fixlinks_conflicts_total 2")
	assert.Contains(t, out, `shoka_fixlinks_referrer_lookups_total{source="index"} 16`)
	assert.Contains(t, out, `shoka_fixlinks_referrer_lookups_total{source="truthscan"} 3`)

	// With no bridge extra, the notify-drop family is absent and the endpoint
	// still serves the rest.
	assert.NotContains(t, out, "shoka_notify_subscriber_drops_total")
}

// TestMetrics_Bridge_NotifyDrops asserts the collector bridge: a supplied
// NotifyDropSource extra surfaces shoka_notify_subscriber_drops_total, while the
// storage families continue to serve.
func TestMetrics_Bridge_NotifyDrops(t *testing.T) {
	srv := httptest.NewServer(Handler(fakeSource{}, fakeNotifyDrops{n: 17}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out := string(body)

	assert.Contains(t, out, "shoka_notify_subscriber_drops_total 17")
	// Storage families still present alongside the bridge family.
	assert.Contains(t, out, "shoka_wal_quarantined_total 6")
}

// TestMetrics_Bridge_NilExtraSafe asserts a nil extra is ignored and the endpoint
// serves the primary families without the notify-drop family.
func TestMetrics_Bridge_NilExtraSafe(t *testing.T) {
	srv := httptest.NewServer(Handler(fakeSource{}, nil))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	out := string(body)

	assert.Contains(t, out, "shoka_wal_pending_entries 3")
	assert.NotContains(t, out, "shoka_notify_subscriber_drops_total")
}
