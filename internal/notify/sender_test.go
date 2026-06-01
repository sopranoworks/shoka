package notify

import (
	"context"
	"sync"
	"testing"
)

// TestNotifyFrom_ExcludesOriginator pins the core of the 2026-06-01 directive: a
// subscriber that declared an identity does not receive an event whose sender is
// that same identity, while every other subscriber does.
func TestNotifyFrom_ExcludesOriginator(t *testing.T) {
	c := NewCenter(10)
	cbA, getA := drainCollector()
	cbB, getB := drainCollector()
	defer c.SubscribeAs("conn-A", cbA)()
	defer c.SubscribeAs("conn-B", cbB)()

	// conn-A originates the write.
	c.NotifyFrom("conn-A", "file.write", "ns/proj", "a.md")

	if got := getA(); len(got) != 0 {
		t.Errorf("originator conn-A should not receive its own event, got %d", len(got))
	}
	got := getB()
	if len(got) != 1 {
		t.Fatalf("conn-B should receive the event, got %d", len(got))
	}
	if got[0].Kind != "file.write" || got[0].Path != "a.md" {
		t.Errorf("conn-B got unexpected event: %+v", got[0])
	}
}

// TestNotifyFrom_EmptySenderDispatchesToAll confirms backward compatibility: an
// unidentified sender (the legacy Notify path) reaches everyone, including
// identified subscribers.
func TestNotifyFrom_EmptySenderDispatchesToAll(t *testing.T) {
	c := NewCenter(10)
	cbA, getA := drainCollector()
	cbB, getB := drainCollector()
	defer c.SubscribeAs("conn-A", cbA)()
	defer c.Subscribe(cbB)() // no identity

	c.NotifyFrom("", "file.write", "ns/proj", "a.md")
	// And the legacy Notify must behave identically (delegates to NotifyFrom "").
	c.Notify("file.delete", "ns/proj", "b.md")

	if got := getA(); len(got) != 2 {
		t.Errorf("identified subscriber should receive both unidentified events, got %d", len(got))
	}
	if got := getB(); len(got) != 2 {
		t.Errorf("unidentified subscriber should receive both events, got %d", len(got))
	}
}

// TestNotifyFrom_UnidentifiedSubscriberNeverExcluded confirms a subscriber that
// declared no identity (Subscribe) receives even an identified sender's event:
// "" never matches a non-empty sender.
func TestNotifyFrom_UnidentifiedSubscriberNeverExcluded(t *testing.T) {
	c := NewCenter(10)
	cb, get := drainCollector()
	defer c.Subscribe(cb)()

	c.NotifyFrom("conn-A", "file.write", "ns/proj", "a.md")

	if got := get(); len(got) != 1 {
		t.Errorf("unidentified subscriber should still receive an identified-sender event, got %d", len(got))
	}
}

// TestNotifyFrom_DistinctSendersDoNotCrossSuppress confirms that exclusion is
// per-identity: conn-A's write reaches conn-B and vice versa (the multi-tab
// case — each tab is a separate writer from the other's perspective).
func TestNotifyFrom_DistinctSendersDoNotCrossSuppress(t *testing.T) {
	c := NewCenter(10)
	cbA, getA := drainCollector()
	cbB, getB := drainCollector()
	defer c.SubscribeAs("conn-A", cbA)()
	defer c.SubscribeAs("conn-B", cbB)()

	c.NotifyFrom("conn-A", "file.write", "ns/proj", "a.md") // A writes
	c.NotifyFrom("conn-B", "file.write", "ns/proj", "b.md") // B writes

	gotA := getA()
	if len(gotA) != 1 || gotA[0].Path != "b.md" {
		t.Errorf("conn-A should receive only conn-B's write, got %+v", gotA)
	}
	gotB := getB()
	if len(gotB) != 1 || gotB[0].Path != "a.md" {
		t.Errorf("conn-B should receive only conn-A's write, got %+v", gotB)
	}
}

// TestNotifyFrom_EventCarriesNoSender confirms the sender is internal to
// dispatch and never leaks into the Event the subscriber observes (§2.3: the
// wire shape is unchanged).
func TestNotifyFrom_EventCarriesNoSender(t *testing.T) {
	c := NewCenter(10)
	cb, get := drainCollector()
	defer c.SubscribeAs("conn-B", cb)()

	c.NotifyFrom("conn-A", "file.write", "ns/proj", "a.md")

	got := get()
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	// Event has exactly Seq/Timestamp/Kind/Target/Path — no sender field exists to
	// inspect; assert the recorded fields are the published ones and nothing more.
	if got[0].Kind != "file.write" || got[0].Target != "ns/proj" || got[0].Path != "a.md" {
		t.Errorf("event fields altered by sender handling: %+v", got[0])
	}
	// The Snapshot (ring buffer) also stores the same shape, unaffected by sender.
	snap := c.Snapshot()
	if len(snap) != 1 || snap[0].Path != "a.md" {
		t.Errorf("snapshot did not record the event verbatim: %+v", snap)
	}
}

// TestNotifyFrom_NilCenterSafe confirms the nil-receiver no-op contract holds for
// the new identified form too.
func TestNotifyFrom_NilCenterSafe(t *testing.T) {
	var c *Center
	c.NotifyFrom("conn-A", "file.write", "ns/proj", "a.md") // must not panic
	unsub := c.SubscribeAs("conn-A", func(Event) {})
	unsub() // must not panic
}

// TestWithSender_RoundTrip confirms the ctx helpers carry the sender and default
// to "" when absent.
func TestWithSender_RoundTrip(t *testing.T) {
	if got := SenderFrom(context.Background()); got != "" {
		t.Errorf("absent sender should be empty, got %q", got)
	}
	ctx := WithSender(context.Background(), "conn-A")
	if got := SenderFrom(ctx); got != "conn-A" {
		t.Errorf("sender round-trip = %q, want conn-A", got)
	}
}

// TestNotifyFrom_ConcurrentSafe runs identified publishes and subscribes under
// the race detector to confirm the added identity field did not break locking.
func TestNotifyFrom_ConcurrentSafe(t *testing.T) {
	c := NewCenter(1000)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			unsub := c.SubscribeAs("conn", func(Event) {})
			for j := 0; j < 50; j++ {
				c.NotifyFrom("conn", "file.write", "ns/proj", "x")
				c.NotifyFrom("other", "file.write", "ns/proj", "y")
			}
			unsub()
		}(i)
	}
	wg.Wait()
}
