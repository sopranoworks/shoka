package ui

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sopranoworks/shoka/internal/drafts"
	"github.com/sopranoworks/shoka/internal/notify"
	"github.com/sopranoworks/shoka/internal/storage"
)

// newSharedCenterManager builds a Manager whose storage and /ws/ui share one
// notification center — the wiring main() uses, and the only configuration in
// which a real write publishes to the connected clients.
func newSharedCenterManager(t *testing.T) (*Manager, storage.StorageService, *notify.Center) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "shoka-ui-sender-*")
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
	dm, err := drafts.NewManager(tmpDir)
	if err != nil {
		t.Fatalf("drafts: %v", err)
	}
	return NewManager(s, dm, center), s, center
}

func sendWS(t *testing.T, conn *websocket.Conn, msgType MessageType, payload interface{}) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	data, err := json.Marshal(WSMessage{Type: msgType, Payload: raw})
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("write %s: %v", msgType, err)
	}
}

// expectNoNotify reads frames until the deadline and fails if a NOTIFY of the
// given kind+path arrives. Non-NOTIFY frames (e.g. SAVE_ACK on the originating
// connection) are tolerated. A read timeout is the success path.
func expectNoNotify(t *testing.T, conn *websocket.Conn, kind, path string, within time.Duration) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(within))
	for {
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			return // timeout / closed: no offending NOTIFY arrived
		}
		if msg.Type != MsgNotify {
			continue
		}
		var ev notify.Event
		if err := json.Unmarshal(msg.Payload, &ev); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if ev.Kind == kind && ev.Path == path {
			t.Fatalf("originator unexpectedly received its own NOTIFY kind=%s path=%s", kind, path)
		}
	}
}

// expectNotify reads frames until it sees a NOTIFY of the given kind+path, or
// fails on timeout.
func expectNotify(t *testing.T, conn *websocket.Conn, kind, path string, within time.Duration) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(within))
	for {
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("did not receive NOTIFY kind=%s path=%s: %v", kind, path, err)
		}
		if msg.Type != MsgNotify {
			continue
		}
		var ev notify.Event
		if err := json.Unmarshal(msg.Payload, &ev); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if ev.Kind == kind && ev.Path == path {
			return
		}
	}
}

// TestSenderExclusion_SaveFileNotEchoedToOriginator is the directive's core
// completion criterion: a SAVE_FILE from one /ws/ui connection does not dispatch
// the resulting file.write NOTIFY back to that connection, but does reach others.
func TestSenderExclusion_SaveFileNotEchoedToOriginator(t *testing.T) {
	m, s, _ := newSharedCenterManager(t)
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	srv := httptest.NewServer(m)
	defer srv.Close()

	connA := dialWS(t, srv.URL)
	defer connA.Close()
	connB := dialWS(t, srv.URL)
	defer connB.Close()
	time.Sleep(100 * time.Millisecond) // let both subscriptions register

	sendWS(t, connA, SaveFile, SaveFilePayload{
		Namespace:   "ns",
		ProjectName: "proj",
		Path:        "self.md",
		Content:     "written by A",
	})

	// B (a different connection) must receive the file.write NOTIFY.
	expectNotify(t, connB, "file.write", "self.md", 2*time.Second)
	// A (the originator) must NOT — only its SAVE_ACK, which expectNoNotify skips.
	expectNoNotify(t, connA, "file.write", "self.md", 500*time.Millisecond)
}

// TestSenderExclusion_CreateProjectNotEchoedToOriginator covers the scope the
// operator expanded the directive to include: a CREATE_PROJECT from one
// connection does not echo project.create back to it, but reaches others.
func TestSenderExclusion_CreateProjectNotEchoedToOriginator(t *testing.T) {
	m, _, _ := newSharedCenterManager(t)
	srv := httptest.NewServer(m)
	defer srv.Close()

	connA := dialWS(t, srv.URL)
	defer connA.Close()
	connB := dialWS(t, srv.URL)
	defer connB.Close()
	time.Sleep(100 * time.Millisecond)

	sendWS(t, connA, MsgCreateProject, CreateProjectPayload{
		Namespace:   "ns2",
		ProjectName: "fresh",
	})

	// project.create carries an empty path; assert on kind with empty path.
	expectNotify(t, connB, "project.create", "", 2*time.Second)
	expectNoNotify(t, connA, "project.create", "", 500*time.Millisecond)
}

// TestSenderExclusion_MCPWriteReachesAllConnections confirms a write whose sender
// is an MCP session id (never a ws-<seq>) reaches every /ws/ui connection: no
// /ws/ui connection is the originator of an MCP write.
func TestSenderExclusion_MCPWriteReachesAllConnections(t *testing.T) {
	m, s, _ := newSharedCenterManager(t)
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	srv := httptest.NewServer(m)
	defer srv.Close()

	connA := dialWS(t, srv.URL)
	defer connA.Close()
	connB := dialWS(t, srv.URL)
	defer connB.Close()
	time.Sleep(100 * time.Millisecond)

	// Simulate the MCP write_file path: a sender that cannot match any ws id.
	ctx := notify.WithSender(context.Background(), "mcp:session-xyz")
	if _, err := s.Write(ctx, "", "ns", "proj", "frommcp.md", "hi", nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	expectNotify(t, connA, "file.write", "frommcp.md", 2*time.Second)
	expectNotify(t, connB, "file.write", "frommcp.md", 2*time.Second)
}

// TestSenderExclusion_LegacyNoSenderReachesAll confirms the backward-compatible
// path: a write with no sender on its context (the legacy non-ctx wrappers /
// background reconciliation) dispatches to every connection.
func TestSenderExclusion_LegacyNoSenderReachesAll(t *testing.T) {
	m, s, _ := newSharedCenterManager(t)
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	srv := httptest.NewServer(m)
	defer srv.Close()

	connA := dialWS(t, srv.URL)
	defer connA.Close()
	connB := dialWS(t, srv.URL)
	defer connB.Close()
	time.Sleep(100 * time.Millisecond)

	// context.Background() carries no sender → dispatch to all.
	if _, err := s.Write(context.Background(), "", "ns", "proj", "legacy.md", "x", nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	expectNotify(t, connA, "file.write", "legacy.md", 2*time.Second)
	expectNotify(t, connB, "file.write", "legacy.md", 2*time.Second)
}
