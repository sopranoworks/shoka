// Package walworker drains the write-ahead log into git in the background. A
// single dispatcher goroutine reads pending WAL entries and hands them to a
// variable-sized pool of stateless workers. The dispatcher preserves per-project
// commit order — at most one entry per project is in flight at a time — while
// letting different projects commit concurrently. Workers grow the pool up to
// MaxWorkers under load and retire down to MinWorkers when idle.
package walworker

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shoka/mcp-server/internal/storage/wal"
)

// CommitFunc persists one WAL entry to git. It returns nil on success or a
// retryable error. The pool re-invokes it with exponential backoff until it
// succeeds (or the pool shuts down).
type CommitFunc func(ctx context.Context, e wal.Entry) error

// Config controls the pool. Zero values take the documented defaults.
type Config struct {
	MinWorkers     int           // default 1
	MaxWorkers     int           // default 8
	IdleTimeout    time.Duration // default 30s
	ScanInterval   time.Duration // default 100ms (fallback wake)
	BackoffInitial time.Duration // default 100ms
	BackoffMax     time.Duration // default 30s
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
}

// Stats reports current pool state for metrics.
type Stats struct {
	ActiveWorkers    int
	IdleWorkers      int
	InFlightProjects int
	CommitsTotal     int64
	CommitsFailed    int64
}

// Pool is the background commit worker pool.
type Pool struct {
	log    *wal.Log
	commit CommitFunc
	cfg    Config

	notifyCh chan struct{}      // coalesced wake signal (cap 1)
	doneCh   chan string        // project key, sent by a worker when it finishes an entry
	workCh   chan wal.EntryHead // dispatcher -> idle worker (unbuffered)

	stopCtx    context.Context
	stopCancel context.CancelFunc
	stopOnce   sync.Once
	wg         sync.WaitGroup

	// inFlight is owned exclusively by the dispatcher goroutine (no mutex).
	inFlight map[string]bool

	activeWorkers atomic.Int64
	idleWorkers   atomic.Int64
	inFlightCount atomic.Int64
	commitsTotal  atomic.Int64
	commitsFailed atomic.Int64
}

// NewPool creates the worker pool and starts the dispatcher and MinWorkers
// workers. commit is invoked by workers to persist each entry to git.
func NewPool(log *wal.Log, commit CommitFunc, cfg Config) *Pool {
	cfg.applyDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{
		log:        log,
		commit:     commit,
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

// process commits one entry, retrying with capped exponential backoff until it
// succeeds or the pool shuts down. It always signals completion so the
// dispatcher clears the project's in-flight mark and can dispatch the next entry.
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
	for {
		if p.stopCtx.Err() != nil {
			return
		}
		if cerr := p.commit(p.stopCtx, entry); cerr == nil {
			_ = p.log.Remove(uint64(head.Seq))
			p.commitsTotal.Add(1)
			return
		}
		p.commitsFailed.Add(1)
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
