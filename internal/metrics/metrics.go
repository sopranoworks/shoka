// Package metrics exposes Shoka's storage-redesign metrics in Prometheus text
// exposition format. The collector reads live values from a Source (the storage
// layer) on each scrape, so gauges are always current and counters never
// double-count. The HTTP endpoint is wired in cmd/server: off by default,
// loopback-only when enabled, mirroring the pprof endpoint.
package metrics

import (
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Source provides the live values the collector exports. The storage layer
// implements it.
type Source interface {
	WALPending() int
	WALPendingBytes() int64
	WALOldestEntryAge() time.Duration
	WALMaxEntries() int
	WALWriteDisabled() bool
	CommitStats() (success, failure int64)
	LockStats() (activeLeases int, forcedReleases int64)
	ProjectStates() map[string]string // "namespace/project" -> "healthy|corrupted|dangerous"

	// Catalog metrics (the 2026-05-30 catalog directive §10).
	CatalogCounters() (updateFailedWrite, updateFailedDelete, invariantViolations, rebuildMissing, rebuildCorrupt, rebuildSchema, rebuildUnreadable int64)
	CatalogFileCounts() map[string][2]int // "namespace/project" -> {files, dirs}

	// Class-A wiring (the 2026-06-05 M1 directive): counters that already exist on
	// the storage layer, now exported.
	QuarantineStats() (quarantined, failed int64) // WAL-entry quarantine (D3)
	IndexCounters() (updateFailedWrite, updateFailedDelete, rebuilds int64)
	IndexRebuildCounters() (stale, recreated int64) // reason-split rebuilds (I1)
	LazyRescanCount() int64                         // D1/B-25 self-extinguishing cost

	// Class-B wiring (the 2026-06-05 M2 directive): the index line — subsystems
	// that had no counter before. All storage-reachable (no bridge).
	IndexSweepRuns() int64                           // index repair-sweep passes (I1)
	IndexHealthStates() map[string]bool              // "namespace/project" -> index healthy (I1)
	SearchFastpathStats() (fastpath, fallback int64) // I2 content-search engage/fallback
}

// NotifyDropSource is the bridge capability for the UI manager's slow-subscriber
// drop counter. ui.Manager satisfies it via NotifyDrops() with no change. It is
// one of the optional "extra" sources Handler accepts beyond the storage Source;
// the collector type-asserts each extra to the capabilities it knows. Every
// bridge capability returns counts only — never an identity, token, or secret —
// so no secret can reach a metric label by construction (the 2026-06-05 M1
// directive's structural protection, carried into M3's OAuth extra).
type NotifyDropSource interface {
	NotifyDrops() int64
}

var projectStateLabels = []string{"healthy", "corrupted", "dangerous"}

type collector struct {
	src Source

	// notifyDropSrc is the optional bridge extra for the notify slow-subscriber
	// drop counter (nil when no such extra was supplied — e.g. tests or a build
	// that wires only storage). Resolved from Handler's variadic extras.
	notifyDropSrc NotifyDropSource

	walPending       *prometheus.Desc
	walPendingBytes  *prometheus.Desc
	walOldestAge     *prometheus.Desc
	walMaxEntries    *prometheus.Desc
	walWriteDisabled *prometheus.Desc
	commitsTotal     *prometheus.Desc
	activeLeases     *prometheus.Desc
	forcedReleases   *prometheus.Desc
	projectState     *prometheus.Desc

	catalogRebuild      *prometheus.Desc
	catalogInvariant    *prometheus.Desc
	catalogUpdateFailed *prometheus.Desc
	catalogFiles        *prometheus.Desc
	catalogDirs         *prometheus.Desc

	// Class-A families (the 2026-06-05 M1 directive).
	walQuarantined      *prometheus.Desc
	walQuarantineFailed *prometheus.Desc
	indexUpdateFailed   *prometheus.Desc
	indexRebuilds       *prometheus.Desc
	lazyRescans         *prometheus.Desc
	notifyDrops         *prometheus.Desc // emitted only when notifyDropSrc != nil

	// Class-B index-line families (the 2026-06-05 M2 directive).
	indexSweepRuns *prometheus.Desc
	indexHealthy   *prometheus.Desc
	searchFastpath *prometheus.Desc
}

func newCollector(src Source, extras ...any) *collector {
	c := &collector{
		src:                 src,
		walPending:          prometheus.NewDesc("shoka_wal_pending_entries", "WAL entries awaiting a background git commit.", nil, nil),
		walPendingBytes:     prometheus.NewDesc("shoka_wal_pending_bytes", "Summed content size of pending WAL entries.", nil, nil),
		walOldestAge:        prometheus.NewDesc("shoka_wal_oldest_entry_age_seconds", "Age of the oldest pending WAL entry, in seconds.", nil, nil),
		walMaxEntries:       prometheus.NewDesc("shoka_wal_max_entries", "Configured WAL write-disabled threshold.", nil, nil),
		walWriteDisabled:    prometheus.NewDesc("shoka_wal_write_disabled", "1 when writes are disabled because the WAL is full, else 0.", nil, nil),
		commitsTotal:        prometheus.NewDesc("shoka_wal_commits_total", "Background git commits by result.", []string{"result"}, nil),
		activeLeases:        prometheus.NewDesc("shoka_filelock_active_leases", "Currently-held file locks.", nil, nil),
		forcedReleases:      prometheus.NewDesc("shoka_filelock_forced_release_total", "Stale file-lock leases reaped.", nil, nil),
		projectState:        prometheus.NewDesc("shoka_project_state", "Per-project state (1 for the current state, 0 otherwise).", []string{"namespace", "project", "state"}, nil),
		catalogRebuild:      prometheus.NewDesc("shoka_catalog_rebuild_total", "Per-project catalogs rebuilt at startup, by reason.", []string{"reason"}, nil),
		catalogInvariant:    prometheus.NewDesc("shoka_catalog_invariant_violations_total", "Times read_file found a path in the catalog but not in the working tree.", nil, nil),
		catalogUpdateFailed: prometheus.NewDesc("shoka_catalog_update_failed_total", "Catalog updates that failed on an otherwise-successful operation, by operation.", []string{"operation"}, nil),
		catalogFiles:        prometheus.NewDesc("shoka_catalog_files", "Files recorded in each project's catalog.", []string{"namespace", "project"}, nil),
		catalogDirs:         prometheus.NewDesc("shoka_catalog_dirs", "Directory buckets in each project's catalog.", []string{"namespace", "project"}, nil),

		walQuarantined:      prometheus.NewDesc("shoka_wal_quarantined_total", "WAL entries quarantined to lost+found and removed from the WAL.", nil, nil),
		walQuarantineFailed: prometheus.NewDesc("shoka_wal_quarantine_failed_total", "Quarantine attempts whose lost+found deposit failed (entry kept).", nil, nil),
		indexUpdateFailed:   prometheus.NewDesc("shoka_index_update_failed_total", "Best-effort incremental index updates that failed, by operation.", []string{"operation"}, nil),
		indexRebuilds:       prometheus.NewDesc("shoka_index_rebuilds_total", "Repair-sweep index rebuilds, by reason.", []string{"reason"}, nil),
		lazyRescans:         prometheus.NewDesc("shoka_lazy_rescans_total", "D1 lazy-rescan-on-corrupted-hit invocations (self-extinguishing cost).", nil, nil),
		notifyDrops:         prometheus.NewDesc("shoka_notify_subscriber_drops_total", "Notify events dropped because a subscriber's buffer was full.", nil, nil),

		indexSweepRuns: prometheus.NewDesc("shoka_index_sweep_runs_total", "Index repair-sweep reconcile passes (a pass that rebuilds nothing still counts).", nil, nil),
		indexHealthy:   prometheus.NewDesc("shoka_index_healthy", "1 when a project's derivative index is open and current with HEAD, else 0.", []string{"namespace", "project"}, nil),
		searchFastpath: prometheus.NewDesc("shoka_search_fastpath_total", "Content-search queries by index outcome: fastpath narrowed reads, fallback read every file.", []string{"outcome"}, nil),
	}

	// Resolve optional bridge extras by capability. An extra that satisfies a
	// capability interface is wired; anything else is ignored. nil/absent extras
	// are safe — the corresponding family is simply not emitted (see Collect).
	for _, e := range extras {
		if e == nil {
			continue
		}
		if nd, ok := e.(NotifyDropSource); ok {
			c.notifyDropSrc = nd
		}
	}
	return c
}

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.walPending
	ch <- c.walPendingBytes
	ch <- c.walOldestAge
	ch <- c.walMaxEntries
	ch <- c.walWriteDisabled
	ch <- c.commitsTotal
	ch <- c.activeLeases
	ch <- c.forcedReleases
	ch <- c.projectState
	ch <- c.catalogRebuild
	ch <- c.catalogInvariant
	ch <- c.catalogUpdateFailed
	ch <- c.catalogFiles
	ch <- c.catalogDirs
	ch <- c.walQuarantined
	ch <- c.walQuarantineFailed
	ch <- c.indexUpdateFailed
	ch <- c.indexRebuilds
	ch <- c.lazyRescans
	ch <- c.notifyDrops
	ch <- c.indexSweepRuns
	ch <- c.indexHealthy
	ch <- c.searchFastpath
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	g := func(d *prometheus.Desc, v float64) { ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v) }

	g(c.walPending, float64(c.src.WALPending()))
	g(c.walPendingBytes, float64(c.src.WALPendingBytes()))
	g(c.walOldestAge, c.src.WALOldestEntryAge().Seconds())
	g(c.walMaxEntries, float64(c.src.WALMaxEntries()))
	g(c.walWriteDisabled, boolToFloat(c.src.WALWriteDisabled()))

	success, failure := c.src.CommitStats()
	ch <- prometheus.MustNewConstMetric(c.commitsTotal, prometheus.CounterValue, float64(success), "success")
	ch <- prometheus.MustNewConstMetric(c.commitsTotal, prometheus.CounterValue, float64(failure), "failure")

	leases, forced := c.src.LockStats()
	g(c.activeLeases, float64(leases))
	ch <- prometheus.MustNewConstMetric(c.forcedReleases, prometheus.CounterValue, float64(forced))

	for key, state := range c.src.ProjectStates() {
		ns, project := splitProjectKey(key)
		for _, label := range projectStateLabels {
			v := 0.0
			if label == state {
				v = 1.0
			}
			ch <- prometheus.MustNewConstMetric(c.projectState, prometheus.GaugeValue, v, ns, project, label)
		}
	}

	cnt := func(d *prometheus.Desc, v int64, labels ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, float64(v), labels...)
	}
	uw, ud, iv, rm, rc, rs, ru := c.src.CatalogCounters()
	cnt(c.catalogInvariant, iv)
	cnt(c.catalogUpdateFailed, uw, "write")
	cnt(c.catalogUpdateFailed, ud, "delete")
	cnt(c.catalogRebuild, rm, "missing")
	cnt(c.catalogRebuild, rc, "corrupt")
	cnt(c.catalogRebuild, rs, "schema_mismatch")
	cnt(c.catalogRebuild, ru, "unreadable")

	for key, fd := range c.src.CatalogFileCounts() {
		ns, project := splitProjectKey(key)
		ch <- prometheus.MustNewConstMetric(c.catalogFiles, prometheus.GaugeValue, float64(fd[0]), ns, project)
		ch <- prometheus.MustNewConstMetric(c.catalogDirs, prometheus.GaugeValue, float64(fd[1]), ns, project)
	}

	// Class-A families (the 2026-06-05 M1 directive).
	quarantined, quarantineFailed := c.src.QuarantineStats()
	cnt(c.walQuarantined, quarantined)
	cnt(c.walQuarantineFailed, quarantineFailed)

	idxFailWrite, idxFailDelete, _ := c.src.IndexCounters()
	cnt(c.indexUpdateFailed, idxFailWrite, "write")
	cnt(c.indexUpdateFailed, idxFailDelete, "delete")

	// One reason-labelled rebuild family at the single increment site; the bare
	// aggregate from IndexCounters() is deliberately not exported (no double-count).
	rebuildStale, rebuildRecreated := c.src.IndexRebuildCounters()
	cnt(c.indexRebuilds, rebuildStale, "stale")
	cnt(c.indexRebuilds, rebuildRecreated, "recreated")

	cnt(c.lazyRescans, c.src.LazyRescanCount())

	// Class-B index-line families (the 2026-06-05 M2 directive).
	cnt(c.indexSweepRuns, c.src.IndexSweepRuns())
	for key, healthy := range c.src.IndexHealthStates() {
		ns, project := splitProjectKey(key)
		ch <- prometheus.MustNewConstMetric(c.indexHealthy, prometheus.GaugeValue, boolToFloat(healthy), ns, project)
	}
	searchFast, searchFall := c.src.SearchFastpathStats()
	cnt(c.searchFastpath, searchFast, "fastpath")
	cnt(c.searchFastpath, searchFall, "fallback")

	// Bridge extra: emitted only when a notify-drop source was supplied.
	if c.notifyDropSrc != nil {
		cnt(c.notifyDrops, c.notifyDropSrc.NotifyDrops())
	}
}

// Handler returns an http.Handler serving /metrics for the given storage source,
// backed by a private registry (no default Go/process collectors). The variadic
// extras are optional secondary sources beyond storage (the collector bridge):
// each is type-asserted to the capability interfaces the collector knows (e.g.
// NotifyDropSource), and anything that matches none is ignored. A nil or absent
// extra is safe — the corresponding family is simply not emitted. This is the one
// bridge the storage-only Source needs to see beyond itself; M3 adds OAuth by
// passing the oauth store as another extra, with no change to this signature.
func Handler(src Source, extras ...any) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(newCollector(src, extras...))
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func splitProjectKey(key string) (namespace, project string) {
	if i := strings.IndexByte(key, '/'); i >= 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}
