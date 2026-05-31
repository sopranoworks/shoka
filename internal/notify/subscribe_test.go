package notify

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// drainCollector returns a callback that appends events to a slice (guarded),
// plus an accessor for the collected events.
func drainCollector() (cb func(Event), get func() []Event) {
	var mu sync.Mutex
	var got []Event
	cb = func(e Event) {
		mu.Lock()
		got = append(got, e)
		mu.Unlock()
	}
	get = func() []Event {
		mu.Lock()
		defer mu.Unlock()
		out := make([]Event, len(got))
		copy(out, got)
		return out
	}
	return cb, get
}

func TestSubscribe_ReceivesEveryEventInOrder(t *testing.T) {
	c := NewCenter(10)
	cb, get := drainCollector()
	unsub := c.Subscribe(cb)
	defer unsub()

	c.Notify("file.write", "ns/proj", "a.md")
	c.Notify("file.delete", "ns/proj", "b.md")
	c.Notify("project.create", "ns/proj", "")

	got := get()
	if len(got) != 3 {
		t.Fatalf("expected 3 events, got %d", len(got))
	}
	wantKind := []string{"file.write", "file.delete", "project.create"}
	for i, w := range wantKind {
		if got[i].Kind != w {
			t.Errorf("event %d kind = %q, want %q", i, got[i].Kind, w)
		}
		if got[i].Seq != uint64(i+1) {
			t.Errorf("event %d seq = %d, want %d", i, got[i].Seq, i+1)
		}
	}
}

func TestSubscribe_MultipleSubscribersEachReceiveAll(t *testing.T) {
	c := NewCenter(10)
	cb1, get1 := drainCollector()
	cb2, get2 := drainCollector()
	defer c.Subscribe(cb1)()
	defer c.Subscribe(cb2)()

	c.Notify("file.write", "ns/proj", "a.md")
	c.Notify("file.write", "ns/proj", "b.md")

	if len(get1()) != 2 || len(get2()) != 2 {
		t.Fatalf("each subscriber should get 2 events; got %d and %d", len(get1()), len(get2()))
	}
}

func TestSubscribe_UnsubscribeStopsDelivery(t *testing.T) {
	c := NewCenter(10)
	cb1, get1 := drainCollector()
	cb2, get2 := drainCollector()
	unsub1 := c.Subscribe(cb1)
	defer c.Subscribe(cb2)()

	c.Notify("file.write", "ns/proj", "a.md")
	unsub1()
	c.Notify("file.write", "ns/proj", "b.md")

	if len(get1()) != 1 {
		t.Errorf("unsubscribed subscriber should have 1 event, got %d", len(get1()))
	}
	if len(get2()) != 2 {
		t.Errorf("still-subscribed subscriber should have 2 events, got %d", len(get2()))
	}
}

// TestSubscribe_SlowSubscriberDoesNotStallPublisher models the WebSocket
// subscriber pattern: a buffered channel drained slowly, dropping on full. The
// publisher loop must complete well within a tight bound regardless.
func TestSubscribe_SlowSubscriberDoesNotStallPublisher(t *testing.T) {
	c := NewCenter(10)
	var dropped atomic.Int64
	events := make(chan Event, 4) // tiny buffer
	unsub := c.Subscribe(func(e Event) {
		select {
		case events <- e:
		default:
			dropped.Add(1) // full — drop, never block
		}
	})
	defer unsub()
	// Never drain `events`, so it fills and the rest are dropped.

	const n = 1000
	done := make(chan struct{})
	go func() {
		for i := 0; i < n; i++ {
			c.Notify("file.write", "ns/proj", "x")
		}
		close(done)
	}()

	select {
	case <-done:
		// Good: the publisher finished despite the un-drained subscriber.
	case <-time.After(5 * time.Second):
		t.Fatal("publisher stalled on a slow/full subscriber")
	}
	if got := dropped.Load(); got == 0 {
		t.Errorf("expected some drops with an un-drained buffer, got 0")
	}
}

// TestSubscribe_ConcurrentWithNotifyAndUnsubscribe is a race-detector exercise:
// subscribers come and go while events are published concurrently.
func TestSubscribe_ConcurrentWithNotifyAndUnsubscribe(t *testing.T) {
	c := NewCenter(100)
	var wg sync.WaitGroup

	// Publishers.
	for p := 0; p < 4; p++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				c.Notify("file.write", "ns/proj", "x")
			}
		}()
	}
	// Churning subscribers.
	for s := 0; s < 8; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				unsub := c.Subscribe(func(Event) {})
				unsub()
			}
		}()
	}
	wg.Wait()
}

func TestSubscribe_NilReceiverReturnsNoopUnsubscribe(t *testing.T) {
	var c *Center
	unsub := c.Subscribe(func(Event) {})
	if unsub == nil {
		t.Fatal("Subscribe on nil receiver must return a non-nil no-op unsubscribe")
	}
	unsub() // must not panic
	c.Notify("file.write", "ns/proj", "a.md") // nil receiver, still a no-op
}

func TestSubscribe_NilCallbackIsNoop(t *testing.T) {
	c := NewCenter(10)
	unsub := c.Subscribe(nil)
	unsub()                                   // must not panic
	c.Notify("file.write", "ns/proj", "a.md") // must not panic with a nil callback registered
}
