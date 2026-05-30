// Package notify is Shoka's in-process notification center: a small, passive
// collection point that records "something happened at <target>/<path>" events
// as they occur during storage operations.
//
// The center is deliberately minimal (the 2026-05-30 notification-center MVP
// directive): a bounded in-memory ring buffer plus three methods. There is no
// subscriber API, no transport binding, no persistence, and no coupling to the
// WAL — it is a lossy, side-channel observation point that future consumers
// (Web UI auto-refresh, drift-rescan triggers) read from via Snapshot/Since.
// Publishing never affects storage correctness.
package notify

import (
	"sync"
	"time"
)

// defaultMaxEntries is the ring buffer size used when NewCenter is given a
// non-positive size.
const defaultMaxEntries = 1000

// Event records that something happened. The center is the source of these.
// Consumers should treat events as invalidation signals: "go re-check the
// indicated target/path because something there changed." The Kind field is
// recorded for observability and for future filtering by consumers, but the
// center itself does not interpret it.
type Event struct {
	Seq       uint64    `json:"seq"`
	Timestamp time.Time `json:"timestamp"`
	Kind      string    `json:"kind"`
	Target    string    `json:"target"`         // "<namespace>/<project>"
	Path      string    `json:"path,omitempty"` // file path within the project; "" for project-level events
}

// Center is the in-process collection point for events. A nil *Center is a
// valid no-op recorder: every method tolerates a nil receiver so storage code
// can call it unconditionally.
type Center struct {
	mu      sync.Mutex
	buf     []Event // ring buffer of fixed length == cap(buf)
	next    uint64  // Seq to assign to the next event (monotonic, starts at 1)
	writeIx int     // index in buf for the next write
	count   int     // number of slots filled (<= len(buf)); caps at len(buf)
}

// NewCenter creates a Center with a ring buffer of the given maximum size.
// maxEntries must be >= 1; if <= 0, a sensible default (1000) is used.
func NewCenter(maxEntries int) *Center {
	if maxEntries <= 0 {
		maxEntries = defaultMaxEntries
	}
	return &Center{
		buf:  make([]Event, maxEntries),
		next: 1,
	}
}

// Notify records that a kind-typed thing happened at target/path. path may be
// "" for project-level events. It is safe to call on a nil receiver (becomes a
// no-op), and safe for concurrent callers. It never blocks on I/O and never
// panics on its input — the center is a dumb recorder; validation is the
// publisher's responsibility.
func (c *Center) Notify(kind, target, path string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.buf[c.writeIx] = Event{
		Seq:       c.next,
		Timestamp: time.Now(),
		Kind:      kind,
		Target:    target,
		Path:      path,
	}
	c.next++
	c.writeIx = (c.writeIx + 1) % len(c.buf)
	if c.count < len(c.buf) {
		c.count++
	}
	c.mu.Unlock()
}

// Snapshot returns up to the most recent maxEntries events in seq order
// (oldest first). The returned slice is a fresh copy; callers may modify it
// freely. Returns an empty slice if the center is nil or has no events.
func (c *Center) Snapshot() []Event {
	if c == nil {
		return []Event{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.orderedLocked()
}

// Since returns events with Seq > sinceSeq, in seq order. If sinceSeq is older
// than the oldest event still in the ring buffer, the caller has missed events
// and the boolean second return value is true. Otherwise it is false. Returns
// (nil, false) on a nil receiver.
func (c *Center) Since(sinceSeq uint64) (events []Event, missed bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	all := c.orderedLocked()
	if len(all) == 0 {
		return nil, false
	}
	// We have overwritten events between sinceSeq and the oldest still present
	// when the oldest event's Seq is more than one past sinceSeq.
	oldest := all[0].Seq
	if oldest > sinceSeq+1 {
		missed = true
	}
	for _, e := range all {
		if e.Seq > sinceSeq {
			events = append(events, e)
		}
	}
	return events, missed
}

// orderedLocked returns the buffered events oldest-first. Caller holds c.mu.
func (c *Center) orderedLocked() []Event {
	out := make([]Event, 0, c.count)
	if c.count == 0 {
		return out
	}
	// The oldest filled slot is writeIx when the buffer is full; otherwise the
	// buffer filled from index 0 and the oldest is index 0.
	start := 0
	if c.count == len(c.buf) {
		start = c.writeIx
	}
	for i := 0; i < c.count; i++ {
		out = append(out, c.buf[(start+i)%len(c.buf)])
	}
	return out
}
