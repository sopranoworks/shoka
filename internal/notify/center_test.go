package notify

import (
	"sync"
	"testing"
)

func TestSingleNotifySnapshot(t *testing.T) {
	c := NewCenter(10)
	c.Notify("file.write", "ns/proj", "a.md")

	got := c.Snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	e := got[0]
	if e.Kind != "file.write" || e.Target != "ns/proj" || e.Path != "a.md" {
		t.Errorf("unexpected event: %+v", e)
	}
	if e.Seq != 1 {
		t.Errorf("expected first Seq == 1, got %d", e.Seq)
	}
	if e.Timestamp.IsZero() {
		t.Errorf("expected Timestamp to be set")
	}
}

func TestNotifyUnderCapacityOrderAndSeq(t *testing.T) {
	c := NewCenter(10)
	const n = 7
	for i := 0; i < n; i++ {
		c.Notify("k", "ns/proj", "p")
	}
	got := c.Snapshot()
	if len(got) != n {
		t.Fatalf("expected %d events, got %d", n, len(got))
	}
	for i, e := range got {
		want := uint64(i + 1)
		if e.Seq != want {
			t.Errorf("event %d: expected Seq %d, got %d", i, want, e.Seq)
		}
	}
}

func TestNotifyOverCapacityEvictsOldest(t *testing.T) {
	c := NewCenter(5)
	for i := 0; i < 12; i++ {
		c.Notify("k", "ns/proj", "p")
	}
	got := c.Snapshot()
	if len(got) != 5 {
		t.Fatalf("expected 5 events (max), got %d", len(got))
	}
	// 12 notifies → Seqs 1..12; only the latest 5 (8..12) survive, in order.
	for i, e := range got {
		want := uint64(8 + i)
		if e.Seq != want {
			t.Errorf("event %d: expected Seq %d, got %d", i, want, e.Seq)
		}
	}
}

func TestConcurrentNotify(t *testing.T) {
	c := NewCenter(20000) // large enough to keep them all
	const goroutines = 100
	const each = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				c.Notify("k", "ns/proj", "p")
			}
		}()
	}
	wg.Wait()

	got := c.Snapshot()
	if len(got) != goroutines*each {
		t.Fatalf("expected %d events, got %d", goroutines*each, len(got))
	}
	// Seq values must cover the contiguous range 1..10000 exactly once.
	seen := make(map[uint64]bool, len(got))
	for _, e := range got {
		if e.Seq < 1 || e.Seq > uint64(goroutines*each) {
			t.Fatalf("Seq out of range: %d", e.Seq)
		}
		if seen[e.Seq] {
			t.Fatalf("duplicate Seq: %d", e.Seq)
		}
		seen[e.Seq] = true
	}
	if len(seen) != goroutines*each {
		t.Fatalf("expected %d distinct Seqs, got %d", goroutines*each, len(seen))
	}
	// Snapshot must still be in ascending seq order.
	for i := 1; i < len(got); i++ {
		if got[i].Seq <= got[i-1].Seq {
			t.Fatalf("snapshot not in ascending seq order at %d: %d then %d", i, got[i-1].Seq, got[i].Seq)
		}
	}
}

func TestSinceReturnsOnlyNewer(t *testing.T) {
	c := NewCenter(100)
	for i := 0; i < 10; i++ {
		c.Notify("k", "ns/proj", "p")
	}
	events, missed := c.Since(4)
	if missed {
		t.Errorf("expected missed=false (buffer holds all 10)")
	}
	if len(events) != 6 {
		t.Fatalf("expected 6 events (Seq 5..10), got %d", len(events))
	}
	if events[0].Seq != 5 || events[len(events)-1].Seq != 10 {
		t.Errorf("unexpected range: %d..%d", events[0].Seq, events[len(events)-1].Seq)
	}
}

func TestSinceMissedWhenOlderThanBuffer(t *testing.T) {
	c := NewCenter(5)
	for i := 0; i < 12; i++ {
		c.Notify("k", "ns/proj", "p")
	}
	// Buffer now holds Seq 8..12. Asking since 3 means events 4..7 were evicted.
	events, missed := c.Since(3)
	if !missed {
		t.Errorf("expected missed=true")
	}
	if len(events) != 5 {
		t.Fatalf("expected 5 events, got %d", len(events))
	}
	if events[0].Seq != 8 {
		t.Errorf("expected oldest returned Seq 8, got %d", events[0].Seq)
	}
}

func TestSinceCurrentMaxIsEmptyNotMissed(t *testing.T) {
	c := NewCenter(10)
	for i := 0; i < 6; i++ {
		c.Notify("k", "ns/proj", "p")
	}
	events, missed := c.Since(6) // 6 is the current max seq
	if missed {
		t.Errorf("expected missed=false at current max")
	}
	if len(events) != 0 {
		t.Errorf("expected no events, got %d", len(events))
	}
}

func TestSinceNoEvents(t *testing.T) {
	c := NewCenter(10)
	events, missed := c.Since(0)
	if missed || events != nil {
		t.Errorf("expected (nil,false) on empty center, got (%v,%v)", events, missed)
	}
}

func TestNilReceiverIsSafe(t *testing.T) {
	var c *Center
	c.Notify("k", "t", "p") // must not panic
	if got := c.Snapshot(); len(got) != 0 {
		t.Errorf("nil Snapshot should be empty, got %d", len(got))
	}
	events, missed := c.Since(0)
	if events != nil || missed {
		t.Errorf("nil Since should be (nil,false), got (%v,%v)", events, missed)
	}
}

func TestNewCenterNonPositiveUsesDefault(t *testing.T) {
	for _, size := range []int{0, -1} {
		c := NewCenter(size)
		if len(c.buf) != defaultMaxEntries {
			t.Errorf("NewCenter(%d): expected default buffer %d, got %d", size, defaultMaxEntries, len(c.buf))
		}
		// And it works: more than a handful of notifies, latest survive.
		for i := 0; i < defaultMaxEntries+5; i++ {
			c.Notify("k", "ns/proj", "p")
		}
		got := c.Snapshot()
		if len(got) != defaultMaxEntries {
			t.Errorf("NewCenter(%d): expected %d retained, got %d", size, defaultMaxEntries, len(got))
		}
	}
}

func TestEmptyFieldsRecordedVerbatim(t *testing.T) {
	c := NewCenter(10)
	c.Notify("", "", "")
	got := c.Snapshot()
	if len(got) != 1 {
		t.Fatalf("expected 1 event, got %d", len(got))
	}
	e := got[0]
	if e.Kind != "" || e.Target != "" || e.Path != "" {
		t.Errorf("expected empty fields recorded verbatim, got %+v", e)
	}
	if e.Seq != 1 {
		t.Errorf("expected Seq 1, got %d", e.Seq)
	}
}

func TestSnapshotIsACopy(t *testing.T) {
	c := NewCenter(10)
	c.Notify("k", "ns/proj", "p")
	got := c.Snapshot()
	got[0].Kind = "mutated"
	again := c.Snapshot()
	if again[0].Kind != "k" {
		t.Errorf("Snapshot must return a fresh copy; center was mutated to %q", again[0].Kind)
	}
}
