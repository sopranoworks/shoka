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
}
