// Package notify is Shoka's in-process notification center: a small, passive
// collection point that records "something happened at <target>/<path>" events
// as they occur during storage operations.
//
// The center is deliberately minimal (the 2026-05-30 notification-center MVP
// directive): a bounded in-memory ring buffer plus the Snapshot/Since readers.
// The 2026-05-31 Web UI auto-refresh directive added a push API, Subscribe, so
// the /ws/ui WebSocket can forward events to browsers live. There is still no
// transport binding inside the center, no persistence, and no coupling to the
// WAL — it is a lossy, side-channel observation point. Publishing never affects
// storage correctness and never blocks on a subscriber.
//
// The 2026-06-01 notify-dispatch directive added sender-exclusion: a write
// carries a sender identifier (NotifyFrom), a subscriber declares its identity
// (SubscribeAs), and the dispatch loop does not deliver an event back to the
// subscriber whose identity matches the sender. This stops a /ws/ui connection
// from receiving its own write as if a second actor had made it. The exclusion
// lives here, in the dispatch decision, so every subscriber type benefits — not
// in any one transport's frame-emission path. An empty sender (the legacy
// Notify/Subscribe pair) is "unidentified" and dispatches to everyone, so
// callers that have not adopted sender identity are unchanged.
//
// Event kinds published by storage:
//   - "file.write"                  — a write_file succeeded (target=ns/project, path=rel)
//   - "file.delete"                 — a delete_file succeeded
//   - "project.create"              — a new project was created (empty path)
//   - "catalog.invariant_violation" — read_file found a path in the catalog but
//     not in the working tree (the 2026-05-30 catalog directive §5.6)
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
	// SourcePath is set only on a "file.move" event: it carries the file's OLD
	// path while Path carries the NEW path, so a consumer can tell which file was
	// renamed to what (the move-file directive §1.5). Empty (and omitted) for every
	// other kind, so the wire shape those consumers observe is unchanged.
	SourcePath string `json:"source_path,omitempty"`
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

	// Subscribers receive every event published from now on (the 2026-05-31
	// push API). Guarded by subMu, independent of mu so the ring-buffer write
	// and the fan-out do not contend. Callbacks must be non-blocking. Each
	// subscriber carries an identity (the 2026-06-01 sender-exclusion directive)
	// used to suppress echoing an event back to its originator; an empty identity
	// is never excluded.
	subMu     sync.RWMutex
	subs      map[uint64]subscriber
	nextSubID uint64
}

// subscriber is one registered fan-out target: its callback and the identity
// used for sender-exclusion. id is "" for a subscriber that declared no identity
// (the legacy Subscribe path); such a subscriber is never excluded.
type subscriber struct {
	id string
	cb func(Event)
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
//
// Notify is the unidentified-sender form: the event is dispatched to every
// subscriber. Callers that know the originating sender (e.g. a /ws/ui
// connection's own write) use NotifyFrom so the originator is excluded.
func (c *Center) Notify(kind, target, path string) {
	c.NotifyFrom("", kind, target, path)
}

// NotifyFrom is Notify with a sender identifier (the 2026-06-01 sender-exclusion
// directive). The event is recorded and dispatched exactly as Notify, except
// that the subscriber whose declared identity (see SubscribeAs) equals a
// non-empty sender does NOT receive it — a write does not echo back to its own
// originator. An empty sender behaves identically to Notify (dispatch to all).
// The sender is internal to dispatch: it is never written into the Event, so the
// wire shape subscribers observe is unchanged.
func (c *Center) NotifyFrom(sender, kind, target, path string) {
	c.publishFrom(sender, Event{Kind: kind, Target: target, Path: path})
}

// NotifyMoveFrom publishes a "file.move" event carrying both the source (old) and
// target (new) paths (the move-file directive §1.5). It is the move analogue of
// NotifyFrom: the sender is the originating connection/session, excluded from its
// own event exactly as for file.write/file.delete. target is the joined
// "<namespace>/<project>"; sourcePath is the old path, path is the new path.
func (c *Center) NotifyMoveFrom(sender, target, sourcePath, path string) {
	c.publishFrom(sender, Event{Kind: "file.move", Target: target, Path: path, SourcePath: sourcePath})
}

// publishFrom records ev (assigning Seq + Timestamp) and fans it out, excluding
// the originating sender. It is the shared core of NotifyFrom / NotifyMoveFrom.
func (c *Center) publishFrom(sender string, ev Event) {
	if c == nil {
		return
	}
	c.mu.Lock()
	ev.Seq = c.next
	ev.Timestamp = time.Now()
	c.buf[c.writeIx] = ev
	c.next++
	c.writeIx = (c.writeIx + 1) % len(c.buf)
	if c.count < len(c.buf) {
		c.count++
	}
	c.mu.Unlock()

	// Fan out to subscribers (push API). Held under the read lock only across the
	// iteration; callbacks are contractually non-blocking (they push to their own
	// buffered channel and drop on full), so a slow subscriber cannot stall this
	// publisher. mu is already released, so the two locks never nest. The
	// originator (sender) is skipped: a non-empty sender matching a subscriber's
	// declared identity means "this is the connection that made the write", which
	// must not be told its own change came from elsewhere.
	c.subMu.RLock()
	for _, sub := range c.subs {
		if sender != "" && sub.id == sender {
			continue
		}
		sub.cb(ev)
	}
	c.subMu.RUnlock()
}

// Subscribe registers a callback to receive every event published from now on.
// The returned function unsubscribes; it must be called when the subscriber goes
// away (e.g. the WebSocket disconnects).
//
// The callback runs on the publisher's goroutine, under the center's subscriber
// read lock. It MUST be fast and non-blocking: the expected pattern is to push
// the event onto a buffered channel that the subscriber drains separately, and
// to drop the event if that channel is full. A callback that blocks will stall
// the publisher; a callback that drops loses only its own subscriber's event.
// The callback must not call its own unsubscribe synchronously (that would
// deadlock on the subscriber lock); unsubscribe from a different goroutine.
//
// Within one subscriber, events arrive in publish order. There is no ordering
// guarantee across subscribers. Subscribe is safe to call from any goroutine and
// concurrently with Notify. On a nil receiver it returns a no-op unsubscribe.
//
// Subscribe registers with no identity: the subscriber receives every event,
// including those it originated (it cannot be excluded, having declared no
// sender identity to match). Subscribers that want self-exclusion use
// SubscribeAs with the same identity they pass to NotifyFrom.
func (c *Center) Subscribe(callback func(Event)) (unsubscribe func()) {
	return c.SubscribeAs("", callback)
}

// SubscribeAs is Subscribe with a sender identity (the 2026-06-01
// sender-exclusion directive). The subscriber will not receive an event whose
// NotifyFrom sender equals this id (when id is non-empty) — its own writes are
// not echoed back to it. An empty id is equivalent to Subscribe (never
// excluded). All other Subscribe semantics (non-blocking callback contract,
// publish-order delivery, nil-receiver/nil-callback no-op) are unchanged.
func (c *Center) SubscribeAs(id string, callback func(Event)) (unsubscribe func()) {
	if c == nil || callback == nil {
		return func() {}
	}
	c.subMu.Lock()
	if c.subs == nil {
		c.subs = make(map[uint64]subscriber)
	}
	c.nextSubID++
	sid := c.nextSubID
	c.subs[sid] = subscriber{id: id, cb: callback}
	c.subMu.Unlock()

	return func() {
		c.subMu.Lock()
		delete(c.subs, sid)
		c.subMu.Unlock()
	}
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
