// Package filelock provides the only way Shoka acquires a write lock on a file
// path. Callers never touch a mutex directly: they hand a function to
// Manager.WithLock and the function runs while the path is held. This keeps the
// concurrency primitive sealed inside the package (no *sync.Mutex, no Lock/
// Unlock is reachable from outside), which is a structural guarantee the storage
// redesign depends on.
//
// Exclusion is provided by a fixed array of 256 stripe mutexes; a path maps to a
// stripe by an FNV-1a hash. Different paths therefore almost never contend, and
// the same path always serialises. Read operations take no lock at all.
package filelock

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"runtime/debug"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// numStripes is the number of stripe mutexes. A path hashes to one stripe; two
// distinct paths only contend on the rare hash collision (1/numStripes).
const numStripes = 256

// Config controls Manager behaviour. Zero values take the documented defaults.
type Config struct {
	MaxLeaseDuration time.Duration // default: 5 * time.Minute
	ReaperInterval   time.Duration // default: 1 * time.Second
}

// LeaseInfo is a snapshot of one held lease, for observability and metrics.
type LeaseInfo struct {
	Path       string
	SessionID  string
	AcquiredAt time.Time
}

// lease records a currently-held path lock. cancelled is a record-keeping flag
// set by the reaper; it does not preempt the running function (Go cannot kill a
// goroutine), it only marks the lease as administratively released.
type lease struct {
	sessionID  string
	acquiredAt time.Time
	cancelled  bool
}

// Manager is the only way to obtain a file lock in Shoka. Callers do not see the
// locks themselves; they pass a function to WithLock and it is invoked under the
// lock for the given path.
type Manager struct {
	maxLeaseDuration time.Duration
	reaperInterval   time.Duration

	stripes [numStripes]sync.Mutex

	// dirStripes is a SEPARATE stripe array for directory-scoped locks (B-48
	// empty-directory reclamation). It is deliberately disjoint from `stripes`:
	// a file path and a directory path never share a mutex, even on a hash
	// collision. That disjointness is what keeps the combined file+dir locking
	// discipline deadlock-free — see WithDirLock.
	dirStripes [numStripes]sync.Mutex

	leasesMu sync.Mutex
	leases   map[string]*lease

	forcedReleases atomic.Int64

	stopCh   chan struct{}
	stopOnce sync.Once

	logger *slog.Logger
}

// NewManager creates a Manager. Pass zero values in Config to take defaults. The
// reaper goroutine starts immediately and runs until Stop is called.
func NewManager(cfg Config) *Manager {
	if cfg.MaxLeaseDuration <= 0 {
		cfg.MaxLeaseDuration = 5 * time.Minute
	}
	if cfg.ReaperInterval <= 0 {
		cfg.ReaperInterval = 1 * time.Second
	}
	m := &Manager{
		maxLeaseDuration: cfg.MaxLeaseDuration,
		reaperInterval:   cfg.ReaperInterval,
		leases:           make(map[string]*lease),
		stopCh:           make(chan struct{}),
		logger:           slog.Default(),
	}
	go m.reaper()
	return m
}

// Stop terminates the reaper. After Stop, WithLock may continue to be called
// safely (locks still serialise), but lease expiry is no longer enforced. In
// practice Stop is only called at server shutdown.
func (m *Manager) Stop() {
	m.stopOnce.Do(func() { close(m.stopCh) })
}

// WithLock runs fn while holding the lock for path on behalf of sessionID.
//
// If ctx is cancelled while waiting for the lock, WithLock returns ctx.Err()
// without calling fn. fn is responsible for re-checking ctx during its own work.
//
// fn's panic is recovered and returned as an error wrapping the panic value
// (with a stack trace logged at ERROR). The lock is released either way.
//
// sessionID is recorded with the lease so ReleaseAllForSession can find it. An
// empty sessionID is allowed (e.g. background work not tied to an MCP session);
// such a lease is only released by expiry, never by ReleaseAllForSession.
//
// Lease expiry is administrative only: when the reaper expires a lease it
// removes the record but does NOT preempt a still-running fn (Go cannot kill a
// goroutine), and it does NOT release the underlying stripe mutex. A subsequent
// WithLock for a path on the same stripe therefore still waits for fn to return.
// The lease duration is intentionally generous so legitimate work is not flagged.
func (m *Manager) WithLock(ctx context.Context, sessionID, path string, fn func() error) (err error) {
	mu := m.lockFor(path)
	if lerr := lockCtx(ctx, mu); lerr != nil {
		return lerr
	}
	// Release order (defers run LIFO): recover panic -> drop lease record -> unlock.
	defer mu.Unlock()

	m.leasesMu.Lock()
	m.leases[path] = &lease{sessionID: sessionID, acquiredAt: time.Now()}
	m.leasesMu.Unlock()
	defer func() {
		m.leasesMu.Lock()
		delete(m.leases, path)
		m.leasesMu.Unlock()
	}()

	defer func() {
		if r := recover(); r != nil {
			m.logger.Error("filelock: panic in locked function",
				"path", path, "session", sessionID, "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("filelock: panic in locked function for %q: %v", path, r)
		}
	}()

	return fn()
}

// WithLocks runs fn while holding the locks for every path in paths at once —
// the multi-file primitive an atomic move needs (rename source + destination +
// each rewritten referrer). It exists because the per-path stripe mutexes are
// non-reentrant and shared by hashing: two distinct paths can map to the same
// stripe, so a caller cannot safely nest WithLock for a set of paths. WithLocks
// maps the paths to their DISTINCT stripe mutexes and locks each one exactly
// once, in ascending stripe-index order. That global ordering makes the
// acquisition deadlock-free against any concurrent WithLock/WithLocks (all
// acquire stripes in the same order), and the de-duplication avoids a stripe
// self-deadlock when two paths collide on one stripe.
//
// ctx cancellation is honoured while waiting for each stripe; on cancellation any
// already-acquired stripes are released and ctx.Err() is returned. A lease is
// recorded per path (for ReleaseAllForSession and observability), and fn's panic
// is recovered exactly as in WithLock. An empty paths slice runs fn with no lock.
func (m *Manager) WithLocks(ctx context.Context, sessionID string, paths []string, fn func() error) (err error) {
	// Distinct stripe indices, ascending — the canonical lock order.
	idxSet := make(map[int]struct{}, len(paths))
	for _, p := range paths {
		idxSet[int(fnv32a(p)%numStripes)] = struct{}{}
	}
	idxs := make([]int, 0, len(idxSet))
	for i := range idxSet {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)

	acquired := make([]int, 0, len(idxs))
	unlockAll := func() {
		for j := len(acquired) - 1; j >= 0; j-- {
			m.stripes[acquired[j]].Unlock()
		}
	}
	for _, i := range idxs {
		if lerr := lockCtx(ctx, &m.stripes[i]); lerr != nil {
			unlockAll()
			return lerr
		}
		acquired = append(acquired, i)
	}
	defer unlockAll()

	m.leasesMu.Lock()
	for _, p := range paths {
		m.leases[p] = &lease{sessionID: sessionID, acquiredAt: time.Now()}
	}
	m.leasesMu.Unlock()
	defer func() {
		m.leasesMu.Lock()
		for _, p := range paths {
			delete(m.leases, p)
		}
		m.leasesMu.Unlock()
	}()

	defer func() {
		if r := recover(); r != nil {
			m.logger.Error("filelock: panic in locked function (multi)",
				"paths", paths, "session", sessionID, "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("filelock: panic in locked function for %v: %v", paths, r)
		}
	}()

	return fn()
}

// WithDirLock runs fn while holding the DIRECTORY-scoped lock for dirPath.
//
// This is a SEPARATE lock namespace from the per-file stripe locks (a distinct
// stripe array, m.dirStripes): a file path and a directory path never share a
// mutex, even on a hash collision. That disjointness is what makes the combined
// discipline deadlock-free — see the lock-ordering note below.
//
// B-48 (empty-directory reclamation) needs it: a file write creates its parent
// directory (MkdirAll) and an empty-dir reaper removes it, and the two must
// serialise on the PARENT DIRECTORY, which the per-file lock — keyed on the FILE
// path — does not cover (B-48 investigation §2.4 / framing correction 2). Every
// file-writing path takes this lock around its MkdirAll+create; every reaper
// takes it around its empty-check-and-remove. With rm semantics (os.Remove
// succeeds iff the directory is empty) both interleavings are correct:
// remove→write recreates via MkdirAll; write→remove fails ENOTEMPTY and the
// directory correctly stays — and the writer never sees a half-created directory
// vanish (the MkdirAll→CreateTemp window is closed because the writer holds this
// lock across it).
//
// Lock ordering (deadlock-free): the per-file stripe lock (WithLock/WithLocks) is
// the OUTER lock and this directory lock is the INNER lock. Callers acquire
// WithDirLock only while already holding the file lock, and hold AT MOST ONE
// directory lock at a time (never two nested). Because the file-stripe and
// dir-stripe arrays are disjoint, a goroutine holding a directory lock is never
// waiting on a file lock, so no wait cycle can form.
//
// ctx cancellation and panic recovery behave exactly as WithLock. No lease is
// recorded: a directory lock is held only across a single MkdirAll/create or
// check-and-remove, far below the lease-expiry horizon.
func (m *Manager) WithDirLock(ctx context.Context, sessionID, dirPath string, fn func() error) (err error) {
	mu := &m.dirStripes[fnv32a(dirPath)%numStripes]
	if lerr := lockCtx(ctx, mu); lerr != nil {
		return lerr
	}
	defer mu.Unlock()

	defer func() {
		if r := recover(); r != nil {
			m.logger.Error("filelock: panic in dir-locked function",
				"dir", dirPath, "session", sessionID, "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("filelock: panic in dir-locked function for %q: %v", dirPath, r)
		}
	}()

	return fn()
}

// ReleaseAllForSession marks all leases held by sessionID as released (removes
// their records). Like reaper-triggered release, it does not stop in-flight fn
// goroutines. Leases with an empty sessionID are not affected.
func (m *Manager) ReleaseAllForSession(sessionID string) {
	if sessionID == "" {
		return
	}
	m.leasesMu.Lock()
	defer m.leasesMu.Unlock()
	for path, l := range m.leases {
		if l.sessionID == sessionID {
			delete(m.leases, path)
		}
	}
}

// ActiveLeases returns a snapshot of currently-held leases.
func (m *Manager) ActiveLeases() []LeaseInfo {
	m.leasesMu.Lock()
	defer m.leasesMu.Unlock()
	out := make([]LeaseInfo, 0, len(m.leases))
	for path, l := range m.leases {
		out = append(out, LeaseInfo{Path: path, SessionID: l.sessionID, AcquiredAt: l.acquiredAt})
	}
	return out
}

// ForcedReleaseCount returns the cumulative number of leases the reaper has
// expired. It backs the shoka_filelock_forced_release_total metric. (This
// accessor is not in the directive's §4.1 surface list but is required by the
// §11 metric; it exposes no locking primitive.)
func (m *Manager) ForcedReleaseCount() int64 {
	return m.forcedReleases.Load()
}

// lockFor returns the stripe mutex a path maps to.
func (m *Manager) lockFor(path string) *sync.Mutex {
	return &m.stripes[fnv32a(path)%numStripes]
}

func fnv32a(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

func (m *Manager) reaper() {
	ticker := time.NewTicker(m.reaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopCh:
			return
		case <-ticker.C:
			m.reapOnce(time.Now())
		}
	}
}

// reapOnce removes lease records older than maxLeaseDuration. It only touches
// the record map; it does not release the stripe mutex (see WithLock docs).
func (m *Manager) reapOnce(now time.Time) {
	m.leasesMu.Lock()
	defer m.leasesMu.Unlock()
	for path, l := range m.leases {
		if now.Sub(l.acquiredAt) > m.maxLeaseDuration {
			l.cancelled = true
			m.logger.Warn("filelock: reaping stale lease",
				"path", path, "session", l.sessionID, "age", now.Sub(l.acquiredAt))
			delete(m.leases, path)
			m.forcedReleases.Add(1)
		}
	}
}

// lockCtx acquires mu, honouring ctx cancellation while waiting. If ctx is
// already done it returns without acquiring; if it is cancelled while blocked it
// abandons the wait and arranges to release the lock once the background
// acquisition completes, so no lock is leaked.
func lockCtx(ctx context.Context, mu *sync.Mutex) error {
	if mu.TryLock() {
		select {
		case <-ctx.Done():
			mu.Unlock()
			return ctx.Err()
		default:
			return nil
		}
	}
	acquired := make(chan struct{})
	go func() {
		mu.Lock()
		close(acquired)
	}()
	select {
	case <-acquired:
		return nil
	case <-ctx.Done():
		go func() {
			<-acquired
			mu.Unlock()
		}()
		return ctx.Err()
	}
}
