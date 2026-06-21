package ui

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sopranoworks/shoka/internal/drafts"
	"github.com/sopranoworks/shoka/internal/notify"
	"github.com/sopranoworks/shoka/internal/storage"

	"github.com/sopranoworks/shoka/pkg/uiws"
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

// TestNotifyWS_EndToEndStorageWrite exercises the full chain a browser sees: a
// real storage.Write publishes file.write to the shared notification center, the
// /ws/ui subscriber forwards it, and the client receives a NOTIFY whose payload
// names the written file. Storage and the UI share one center (as main() wires
// them).
func TestNotifyWS_EndToEndStorageWrite(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "shoka-ui-e2e-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmpDir) })

	center := notify.NewCenter(100)
	s, err := storage.NewFSGitStorageWithOptions(tmpDir, storage.Options{NotifyCenter: center})
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	dm, err := drafts.NewManager(tmpDir)
	if err != nil {
		t.Fatalf("drafts: %v", err)
	}

	srv := httptest.NewServer(NewManager(s, dm, center))
	defer srv.Close()
	conn := dialWS(t, srv.URL)
	defer conn.Close()
	time.Sleep(50 * time.Millisecond)

	// A real write through the storage layer (as the MCP write_file tool does).
	if _, err := s.Write(context.Background(), "", "ns", "proj", "backlog.md", "hello", nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The project.create from CreateProject may also be buffered; read until we
	// see the file.write for backlog.md (bounded).
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var sawWrite bool
	for i := 0; i < 5 && !sawWrite; i++ {
		var msg uiws.WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			break
		}
		if msg.Type != MsgNotify {
			continue
		}
		var ev notify.Event
		if err := json.Unmarshal(msg.Payload, &ev); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if ev.Kind == "file.write" && ev.Target == "ns/proj" && ev.Path == "backlog.md" {
			sawWrite = true
		}
	}
	if !sawWrite {
		t.Errorf("did not receive a NOTIFY file.write for the written file")
	}
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
	var msg uiws.WSMessage
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
		var msg uiws.WSMessage
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
