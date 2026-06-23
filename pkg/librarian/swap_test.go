package librarian

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

// recordingClient is a fake llm.Client that records how many times it was called
// and (optionally) blocks inside CreateMessage so a swap can be staged while a
// call is in flight. With nil channels it returns immediately.
type recordingClient struct {
	name    string
	calls   atomic.Int32
	started chan struct{} // signalled once, on entry to the first call
	release chan struct{} // the call blocks until this is closed
}

func (c *recordingClient) CreateMessage(_ context.Context, _ llm.CreateMessageParams) (llm.Message, error) {
	n := c.calls.Add(1)
	if c.started != nil && n == 1 {
		c.started <- struct{}{}
	}
	if c.release != nil {
		<-c.release
	}
	// A final text answer (no tool_use) ends the loop after one round-trip.
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.Block{{Type: llm.BlockText, Text: "ok from " + c.name}}}, nil
}

// TestLibrarian_SwapDuringInflightAsk: a client swap while an Ask is in flight
// must NOT affect that call — it completes on the client it started with — and the
// NEXT Ask must use the new client. This is the core guarantee of the live reload.
func TestLibrarian_SwapDuringInflightAsk(t *testing.T) {
	root := t.TempDir()
	a := &recordingClient{name: "A", started: make(chan struct{}, 1), release: make(chan struct{})}
	b := &recordingClient{name: "B"}

	lib := New(a, 4)

	done := make(chan Result, 1)
	go func() {
		r, _ := lib.Ask(context.Background(), Request{Question: "q", Root: root})
		done <- r
	}()

	<-a.started      // A has captured the call and is mid-round-trip
	lib.SetClient(b) // swap to B while A is in flight
	close(a.release) // let A finish

	r := <-done
	if a.calls.Load() != 1 || b.calls.Load() != 0 {
		t.Fatalf("in-flight call used the wrong client: A=%d B=%d, want A=1 B=0", a.calls.Load(), b.calls.Load())
	}
	if !strings.Contains(r.Answer, "from A") {
		t.Errorf("in-flight answer = %q, want it served by A", r.Answer)
	}

	// The next Ask must use the swapped-in client B.
	r2, _ := lib.Ask(context.Background(), Request{Question: "q2", Root: root})
	if b.calls.Load() != 1 {
		t.Errorf("after swap, B calls = %d, want 1", b.calls.Load())
	}
	if !strings.Contains(r2.Answer, "from B") {
		t.Errorf("post-swap answer = %q, want it served by B", r2.Answer)
	}
}

// TestLibrarian_ConcurrentSwapAndAsk stresses the guarded client reference: many
// Asks and SetClients run concurrently. Under -race this fails if the client field
// is accessed without the lock. It must complete without a panic or data race.
func TestLibrarian_ConcurrentSwapAndAsk(t *testing.T) {
	root := t.TempDir()
	clients := []*recordingClient{{name: "0"}, {name: "1"}, {name: "2"}}
	lib := New(clients[0], 2)

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				lib.SetClient(clients[j%len(clients)])
			}
		}()
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if _, err := lib.Ask(context.Background(), Request{Question: "q", Root: root}); err != nil {
					t.Errorf("Ask: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
