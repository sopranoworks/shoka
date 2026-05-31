package ui

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shoka/mcp-server/internal/drafts"
	"github.com/shoka/mcp-server/internal/notify"
	"github.com/shoka/mcp-server/internal/storage"
)

func newNotifyManager(t *testing.T) (*Manager, *notify.Center) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "shoka-ui-notify-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })
	s, err := storage.NewFSGitStorage(tmpDir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	dm, err := drafts.NewManager(tmpDir)
	if err != nil {
		t.Fatalf("drafts: %v", err)
	}
	center := notify.NewCenter(100)
	return NewManager(s, dm, center), center
}

func dialWS(t *testing.T, serverURL string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(serverURL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

func TestNotifyWS_DeliversEventToClient(t *testing.T) {
	m, center := newNotifyManager(t)
	srv := httptest.NewServer(m)
	defer srv.Close()

	conn := dialWS(t, srv.URL)
	defer conn.Close()

	// Give the server a moment to register the subscription before publishing.
	time.Sleep(50 * time.Millisecond)
	center.Notify("file.write", "ns/proj", "backlog.md")

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var msg WSMessage
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read NOTIFY: %v", err)
	}
	if msg.Type != MsgNotify {
		t.Fatalf("expected %s, got %s", MsgNotify, msg.Type)
	}
	var ev notify.Event
	if err := json.Unmarshal(msg.Payload, &ev); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if ev.Kind != "file.write" || ev.Target != "ns/proj" || ev.Path != "backlog.md" {
		t.Errorf("unexpected event payload: %+v", ev)
	}
}

func TestNotifyWS_MultipleClientsEachReceive(t *testing.T) {
	m, center := newNotifyManager(t)
	srv := httptest.NewServer(m)
	defer srv.Close()

	c1 := dialWS(t, srv.URL)
	defer c1.Close()
	c2 := dialWS(t, srv.URL)
	defer c2.Close()
	time.Sleep(50 * time.Millisecond)

	center.Notify("file.delete", "ns/proj", "x.md")

	for i, c := range []*websocket.Conn{c1, c2} {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		var msg WSMessage
		if err := c.ReadJSON(&msg); err != nil {
			t.Fatalf("client %d read: %v", i, err)
		}
		if msg.Type != MsgNotify {
			t.Errorf("client %d expected NOTIFY, got %s", i, msg.Type)
		}
	}
}

func TestNotifyWS_CloseUnsubscribesNoGoroutineLeak(t *testing.T) {
	m, center := newNotifyManager(t)
	srv := httptest.NewServer(m)
	defer srv.Close()

	settle := func() { time.Sleep(100 * time.Millisecond); runtime.GC() }
	settle()
	base := runtime.NumGoroutine()

	for i := 0; i < 10; i++ {
		conn := dialWS(t, srv.URL)
		time.Sleep(10 * time.Millisecond)
		center.Notify("file.write", "ns/proj", "a.md")
		_ = conn.Close()
	}
	settle()

	// Goroutine count should return near baseline (each connection's read loop
	// and drain goroutine must have exited). Allow slack for httptest/runtime.
	var after int
	for i := 0; i < 20; i++ {
		after = runtime.NumGoroutine()
		if after <= base+3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if after > base+3 {
		t.Errorf("goroutine leak: baseline %d, after %d", base, after)
	}
}

func TestNotifyWS_SlowClientDoesNotStallPublisher(t *testing.T) {
	m, center := newNotifyManager(t)
	srv := httptest.NewServer(m)
	defer srv.Close()

	conn := dialWS(t, srv.URL)
	defer conn.Close()
	time.Sleep(50 * time.Millisecond)
	// Never read from conn: the server's drain goroutine will block writing once
	// the OS/socket buffer fills, the 64-event channel fills, and further events
	// are dropped — but the publisher (Notify) must stay fast.

	const n = 1000
	start := time.Now()
	for i := 0; i < n; i++ {
		center.Notify("file.write", "ns/proj", "x")
	}
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("publisher stalled on a slow client: %v for %d notifies", elapsed, n)
	}
	// Some events must have been dropped (the client never read).
	if m.NotifyDrops() == 0 {
		t.Errorf("expected dropped events for a non-reading client, got 0")
	}
}
