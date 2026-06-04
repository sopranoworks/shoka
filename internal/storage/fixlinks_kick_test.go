package storage

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// I3 §3.2 — the post-move kick. A successful move enqueues exactly one
// fix_links kick carrying (ns, proj, src, dst); the move itself stays a pure
// rename. The kick is the only trigger (no periodic backstop).
func TestMove_EnqueuesFixLinksKick(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "src.md", "body", nil)
	require.NoError(t, err)

	_, _, err = s.Move(context.Background(), "sess", "ns", "proj", "src.md", "dst.md", nil)
	require.NoError(t, err)

	select {
	case k := <-s.fixLinksKicks:
		assert.Equal(t, fixLinksKick{namespace: "ns", project: "proj", src: "src.md", dst: "dst.md"}, k)
	case <-time.After(time.Second):
		t.Fatal("move did not enqueue a fix_links kick")
	}
}

// The enqueue is non-blocking: a full kick channel must never make a move block
// (move stays a pure rename); the surplus kick is dropped (a missed kick leaves a
// stale-but-recoverable link, absorbed by the tenets — there is no backstop).
func TestMove_EnqueueNeverBlocksWhenKickChannelFull(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "src.md", "body", nil)
	require.NoError(t, err)

	// Saturate the kick channel.
	for i := 0; i < cap(s.fixLinksKicks); i++ {
		s.fixLinksKicks <- fixLinksKick{}
	}

	done := make(chan error, 1)
	go func() {
		_, _, e := s.Move(context.Background(), "sess", "ns", "proj", "src.md", "dst.md", nil)
		done <- e
	}()
	select {
	case e := <-done:
		require.NoError(t, e, "move must succeed even when the kick channel is full")
	case <-time.After(2 * time.Second):
		t.Fatal("move blocked on a full kick channel")
	}
}

// The StartIndexSweep goroutine drains kicks via its select loop and repairs the
// referrers — wiring the kick path end to end (enqueue → drain → fix_links).
func TestIndexSweep_DrainsFixLinksKickAndRepairs(t *testing.T) {
	s, _ := newStore(t, Options{})
	require.NoError(t, s.CreateProject("ns", "proj"))
	_, err := s.Write(context.Background(), "sess", "ns", "proj", "ref.md", "[t](old.md)", nil)
	require.NoError(t, err)
	_, err = s.Write(context.Background(), "sess", "ns", "proj", "old.md", "# Old", nil)
	require.NoError(t, err)
	_, _, err = s.Move(context.Background(), "sess", "ns", "proj", "old.md", "new.md", nil)
	require.NoError(t, err)
	makeIndexHealthy(t, s, "ns", "proj")

	// Drain the kick the move itself enqueued so we control the one under test.
	select {
	case <-s.fixLinksKicks:
	default:
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.StartIndexSweep(ctx, time.Hour) // long interval; the kick (not the tick) drives repair

	s.fixLinksKicks <- fixLinksKick{namespace: "ns", project: "proj", src: "old.md", dst: "new.md"}

	require.Eventually(t, func() bool {
		body, _, rerr := s.ReadFileWithETag("ns", "proj", "ref.md")
		return rerr == nil && body == "[t](new.md)"
	}, 3*time.Second, 20*time.Millisecond, "the drained kick must repair the referrer")
}
