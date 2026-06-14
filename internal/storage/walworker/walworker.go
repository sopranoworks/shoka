// Package walworker drains the write-ahead log into git in the background. A
// single dispatcher goroutine reads pending WAL entries and hands them to a
// variable-sized pool of stateless workers. The dispatcher preserves per-project
// commit order — at most one entry per project is in flight at a time — while
// letting different projects commit concurrently. Workers grow the pool up to
// MaxWorkers under load and retire down to MinWorkers when idle.
package walworker

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sopranoworks/shoka/internal/storage/wal"
)

// ErrCommitPermanent marks a commit failure the entry can NEVER recover from, so
// the pool must stop retrying and quarantine it instead of looping forever. A
// CommitFunc returns it (wrapped, so the underlying cause survives in the chain)
// when the failure is deterministic and unrecoverable — today only a repo-absent
// project (see storage.commitEntry). It lives here, beside the CommitFunc contract,
// so the walworker recognises it with errors.Is WITHOUT importing go-git: the
// go-git classification (errors.Is(err, git.ErrRepositoryNotExists)) stays on the
// storage side, which then signals permanence with this marker.
var ErrCommitPermanent = errors.New("walworker: commit can never succeed")

// CommitFunc persists one WAL entry to git. It returns nil on success, a retryable
// error (the pool re-invokes it with exponential backoff until it succeeds or the
// pool shuts down), or an error wrapping ErrCommitPermanent to signal the entry can
// never commit (the pool quarantines it instead of retrying).
type CommitFunc func(ctx context.Context, e wal.Entry) error

// QuarantineFunc preserves an uncommittable WAL entry: it deposits the entry's
// content to lost+found (the storage-side write-bytes deposit, which works even
// with the project's git repo absent) and emits the quarantine NOTIFY. It returns
// an error only if the deposit fails, in which case the pool keeps the entry rather
// than removing it (the content must not be lost). Injected from the storage side
// like CommitFunc so the walworker imports no go-git and no storage internals.
type QuarantineFunc func(e wal.Entry) error

// Config controls the pool. Zero values take the documented defaults.
type Config struct {
	MinWorkers     int           // default 1
	MaxWorkers     int           // default 8
	IdleTimeout    time.Duration // default 30s
	ScanInterval   time.Duration // default 100ms (fallback wake)
	BackoffInitial time.Duration // default 100ms
	BackoffMax     time.Duration // default 30s

	// MaxCommitAttempts is the backstop cap: after this many consecutive failed
	// commits of an UNCLASSIFIED error, the entry is quarantined the same way a
	// classified-permanent failure is, so a stuck-but-not-classified entry stops
	// looping. A classified-permanent failure (ErrCommitPermanent) is quarantined
	// at once and does not wait for this cap. Default 10 — with the default backoff
	// that is ≈51s of retrying, enough to ride out a transient I/O blip. A failure
	// that recovers within the cap commits normally.
	MaxCommitAttempts int // default 10
}

func (c *Config) applyDefaults() {
	if c.MinWorkers <= 0 {
		c.MinWorkers = 1
	}
	if c.MaxWorkers <= 0 {
		c.MaxWorkers = 8
	}
	if c.MaxWorkers < c.MinWorkers {
		c.MaxWorkers = c.MinWorkers
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 30 * time.Second
	}
	if c.ScanInterval <= 0 {
		c.ScanInterval = 100 * time.Millisecond
	}
	if c.BackoffInitial <= 0 {
		c.BackoffInitial = 100 * time.Millisecond
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 30 * time.Second
	}
	if c.MaxCommitAttempts <= 0 {
		c.MaxCommitAttempts = 10
	}
}

// Stats reports current pool state for metrics.
type Stats struct {
	ActiveWorkers    int
	IdleWorkers      int
	InFlightProjects int
	CommitsTotal     int64
	CommitsFailed    int64
	QuarantinedTotal int64 // entries deposited to lost+found and removed from the WAL
	QuarantineFailed int64 // quarantine attempts whose deposit failed (entry kept)
}

// Pool is the background commit worker pool.
type Pool struct {
	log        *wal.Log
	commit     CommitFunc
	quarantine QuarantineFunc
	cfg        Config

	notifyCh chan struct{}      // coalesced wake signal (cap 1)
	doneCh   chan string        // project key, sent by a worker when it finishes an entry
	workCh   chan wal.EntryHead // dispatcher -> idle worker (unbuffered)

	stopCtx    context.Context
	stopCancel context.CancelFunc
	stopOnce   sync.Once
	wg         sync.WaitGroup

	// inFlight is owned exclusively by the dispatcher goroutine (no mutex).
	inFlight map[string]bool

	activeWorkers    atomic.Int64
	idleWorkers      atomic.Int64
	inFlightCount    atomic.Int64
	commitsTotal     atomic.Int64
	commitsFailed    atomic.Int64
	quarantinedTotal atomic.Int64
	quarantineFailed atomic.Int64
}

// NewPool creates the worker pool and starts the dispatcher and MinWorkers
// workers. commit is invoked by workers to persist each entry to git; quarantine
// is invoked to preserve an entry that can never commit (it may be nil, in which
// case a permanent/backstop failure still returns — freeing the slot — but leaves
// the entry in the WAL). Both are injected from the storage side.
func NewPool(log *wal.Log, commit CommitFunc, quarantine QuarantineFunc, cfg Config) *Pool {
	cfg.applyDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		log:        log,
		commit:     commit,
		quarantine: quarantine,
		cfg:        cfg,
		notifyCh:   make(chan struct{}, 1),
		doneCh:     make(chan string),
		workCh:     make(chan wal.EntryHead),
		stopCtx:    ctx,
		stopCancel: cancel,
		inFlight:   make(map[string]bool),
	}
	p.wg.Add(1)
	go p.dispatcher()
	for i := 0; i < cfg.MinWorkers; i++ {
		p.spawnWorker()
	}
	// Pick up anything already in the WAL from a previous run.
	p.Notify()
	return p
}

// Notify wakes the dispatcher to scan the WAL immediately (used by the write
// path after Append). Non-blocking and coalesced.
func (p *Pool) Notify() {
	select {
	case p.notifyCh <- struct{}{}:
	default:
	}
}

// Shutdown stops accepting new work and waits for in-flight commits to finish,
// up to timeout. After Shutdown, Notify is a no-op.
func (p *Pool) Shutdown(timeout time.Duration) error {
	p.stopOnce.Do(func() { p.stopCancel() })
	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("walworker: shutdown timed out after %s", timeout)
	}
}

// Stats reports current pool state.
func (p *Pool) Stats() Stats {
	return Stats{
		ActiveWorkers:    int(p.activeWorkers.Load()),
		IdleWorkers:      int(p.idleWorkers.Load()),
		InFlightProjects: int(p.inFlightCount.Load()),
		CommitsTotal:     p.commitsTotal.Load(),
		CommitsFailed:    p.commitsFailed.Load(),
		QuarantinedTotal: p.quarantinedTotal.Load(),
		QuarantineFailed: p.quarantineFailed.Load(),
	}
}

// --- dispatcher ---

func (p *Pool) dispatcher() {
	defer p.wg.Done()
	ticker := time.NewTicker(p.cfg.ScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stopCtx.Done():
			return
		case <-p.notifyCh:
			p.dispatch()
		case <-ticker.C:
			p.dispatch()
		case key := <-p.doneCh:
			p.unmarkInFlight(key)
			p.dispatch()
		}
	}
}

// dispatch assigns pending entries to workers in seq order, one per project.
func (p *Pool) dispatch() {
	heads, err := p.log.ListPending()
	if err != nil {
		return
	}
	for _, h := range heads {
		key := h.Namespace + "/" + h.Project
		if p.inFlight[key] {
			continue
		}
		p.markInFlight(key)
		if !p.tryAssign(h) {
			// No spare capacity right now; release the mark and stop — the next
			// completion or scan tick will retry.
			p.unmarkInFlight(key)
			return
		}
	}
}

// tryAssign hands h to an idle worker, spawning one if none is idle and the pool
// is below MaxWorkers. Returns false if the pool is saturated. Never holds a
// lock while sending on workCh.
func (p *Pool) tryAssign(h wal.EntryHead) bool {
	select {
	case p.workCh <- h:
		return true
	default:
	}
	if p.activeWorkers.Load() < int64(p.cfg.MaxWorkers) {
		p.spawnWorker()
		select {
		case p.workCh <- h:
			return true
		case <-p.stopCtx.Done():
			return false
		}
	}
	return false
}

func (p *Pool) markInFlight(key string) {
	p.inFlight[key] = true
	p.inFlightCount.Add(1)
}

func (p *Pool) unmarkInFlight(key string) {
	if p.inFlight[key] {
		delete(p.inFlight, key)
		p.inFlightCount.Add(-1)
	}
}

// --- workers ---

func (p *Pool) spawnWorker() {
	p.activeWorkers.Add(1)
	p.wg.Add(1)
	go p.worker()
}

func (p *Pool) worker() {
	defer p.wg.Done()
	for {
		p.idleWorkers.Add(1)
		timer := time.NewTimer(p.cfg.IdleTimeout)
		select {
		case <-p.stopCtx.Done():
			p.idleWorkers.Add(-1)
			timer.Stop()
			p.activeWorkers.Add(-1)
			return
		case head := <-p.workCh:
			p.idleWorkers.Add(-1)
			timer.Stop()
			p.process(head)
		case <-timer.C:
			p.idleWorkers.Add(-1)
			// Retire if above MinWorkers; otherwise stay as a core worker.
			for {
				cur := p.activeWorkers.Load()
				if cur <= int64(p.cfg.MinWorkers) {
					break
				}
				if p.activeWorkers.CompareAndSwap(cur, cur-1) {
					return
				}
			}
		}
	}
}

// process commits one entry, retrying with capped exponential backoff. It returns
// (firing the deferred doneCh, so the dispatcher clears the project's in-flight
// mark and can dispatch the next entry) on ANY of: commit success, pool shutdown,
// or quarantine — a classified-permanent failure (ErrCommitPermanent) or an
// unclassified failure that persists past MaxCommitAttempts. The load-bearing
// guarantee is that process() never loops forever: an entry that can never commit
// is quarantined and the worker slot freed, so a handful of such entries can no
// longer saturate the pool and stall all commit progress (the B-38.2 hole).
func (p *Pool) process(head wal.EntryHead) {
	key := head.Namespace + "/" + head.Project
	defer func() {
		select {
		case p.doneCh <- key:
		case <-p.stopCtx.Done():
		}
	}()

	entry, err := p.log.ReadByID(uint64(head.Seq))
	if err != nil {
		// Entry is unreadable (e.g. quarantined as corrupt). It is no longer
		// pending, so there is nothing to retry; count one failure and move on.
		p.commitsFailed.Add(1)
		return
	}

	backoff := p.cfg.BackoffInitial
	attempts := 0
	for {
		if p.stopCtx.Err() != nil {
			return
		}
		cerr := p.commit(p.stopCtx, entry)
		if cerr == nil {
			_ = p.log.Remove(uint64(head.Seq))
			p.commitsTotal.Add(1)
			return
		}
		p.commitsFailed.Add(1)
		attempts++

		// Quarantine when the entry can never commit:
		//   - a classified-permanent failure — the project's git repo is absent,
		//     signalled by commitEntry wrapping ErrCommitPermanent — at once; or
		//   - an UNCLASSIFIED failure that keeps failing past the backstop cap, so a
		//     stuck-but-not-classified entry also stops looping.
		// A genuinely transient failure that recovers within the cap commits above.
		if errors.Is(cerr, ErrCommitPermanent) || attempts >= p.cfg.MaxCommitAttempts {
			if p.runQuarantine(head, entry) {
				// Deposited and removed: RETURN so the deferred doneCh fires,
				// freeing the worker slot and the project's in-flight mark. This is
				// the load-bearing exit that closes the pool-saturation hole.
				return
			}
			// Quarantine did NOT remove the entry (no seam configured, or — the rare
			// case — the deposit failed and the content must not be lost). Throttle
			// with the backoff before returning, so the dispatcher's immediate
			// re-pick is not a hot loop; the entry stays pending and is retried. The
			// slot still frees on return, so the pool is not saturated.
			select {
			case <-p.stopCtx.Done():
			case <-time.After(backoff):
			}
			return
		}

		select {
		case <-p.stopCtx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < p.cfg.BackoffMax {
			backoff *= 2
			if backoff > p.cfg.BackoffMax {
				backoff = p.cfg.BackoffMax
			}
		}
	}
}

// runQuarantine preserves an uncommittable entry via the injected QuarantineFunc
// and removes it from the WAL so it never replays. It reports whether the entry was
// removed (deposited successfully). Ordering is load-bearing: the deposit happens
// BEFORE Remove, and on a deposit failure the entry is NOT removed (its content must
// not be lost before it is safely preserved) — the caller then keeps it pending.
func (p *Pool) runQuarantine(head wal.EntryHead, entry wal.Entry) bool {
	if p.quarantine == nil {
		// No quarantine seam configured (only in tests with always-succeeding
		// commits, where this path is otherwise unreachable). Nothing removed.
		return false
	}
	if qerr := p.quarantine(entry); qerr != nil {
		// Deposit failed — keep the entry pending (its content is not yet safe in
		// lost+found). Rare (a disk/permission fault on the lost+found area). The
		// storage-side QuarantineFunc logs the failure; this counter surfaces it.
		p.quarantineFailed.Add(1)
		return false
	}
	_ = p.log.Remove(uint64(head.Seq))
	p.quarantinedTotal.Add(1)
	return true
}
