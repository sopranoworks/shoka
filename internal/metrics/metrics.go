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
}

var projectStateLabels = []string{"healthy", "corrupted", "dangerous"}

type collector struct {
	src Source

	walPending       *prometheus.Desc
	walPendingBytes  *prometheus.Desc
	walOldestAge     *prometheus.Desc
	walMaxEntries    *prometheus.Desc
	walWriteDisabled *prometheus.Desc
	commitsTotal     *prometheus.Desc
	activeLeases     *prometheus.Desc
	forcedReleases   *prometheus.Desc
	projectState     *prometheus.Desc
}

func newCollector(src Source) *collector {
	return &collector{
		src:              src,
		walPending:       prometheus.NewDesc("shoka_wal_pending_entries", "WAL entries awaiting a background git commit.", nil, nil),
		walPendingBytes:  prometheus.NewDesc("shoka_wal_pending_bytes", "Summed content size of pending WAL entries.", nil, nil),
		walOldestAge:     prometheus.NewDesc("shoka_wal_oldest_entry_age_seconds", "Age of the oldest pending WAL entry, in seconds.", nil, nil),
		walMaxEntries:    prometheus.NewDesc("shoka_wal_max_entries", "Configured WAL write-disabled threshold.", nil, nil),
		walWriteDisabled: prometheus.NewDesc("shoka_wal_write_disabled", "1 when writes are disabled because the WAL is full, else 0.", nil, nil),
		commitsTotal:     prometheus.NewDesc("shoka_wal_commits_total", "Background git commits by result.", []string{"result"}, nil),
		activeLeases:     prometheus.NewDesc("shoka_filelock_active_leases", "Currently-held file locks.", nil, nil),
		forcedReleases:   prometheus.NewDesc("shoka_filelock_forced_release_total", "Stale file-lock leases reaped.", nil, nil),
		projectState:     prometheus.NewDesc("shoka_project_state", "Per-project state (1 for the current state, 0 otherwise).", []string{"namespace", "project", "state"}, nil),
	}
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
}

// Handler returns an http.Handler serving /metrics for the given source, backed
// by a private registry (no default Go/process collectors).
func Handler(src Source) http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(newCollector(src))
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
