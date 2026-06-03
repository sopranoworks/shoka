package ui

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shoka/mcp-server/internal/identity"
	"github.com/shoka/mcp-server/internal/notify"
	"github.com/shoka/mcp-server/internal/storage"
)

// readUntil reads frames until one of msgType arrives (decoding its payload into
// dst), or fails on timeout. Other frame types are skipped.
func readUntil(t *testing.T, conn *websocket.Conn, msgType MessageType, dst interface{}, within time.Duration) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(within))
	for {
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("did not receive %s: %v", msgType, err)
		}
		if msg.Type != msgType {
			continue
		}
		if dst != nil {
			if err := json.Unmarshal(msg.Payload, dst); err != nil {
				t.Fatalf("decode %s: %v", msgType, err)
			}
		}
		return
	}
}

// TestWSUI_MoveFileEndToEnd is the directive's /ws/ui completion criterion: a
// MOVE_FILE from connection A produces a MOVE_ACK to A (new etag + links count),
// a file.move NOTIFY to connection B (carrying source and target), and no echo of
// that NOTIFY back to A (sender exclusion). The operator-authored-commit half of
// the guarantee is pinned inside the storage submodule
// (storage.TestMove_WebIsUserAuthored) to avoid a go-git import in this package
// (archlint Anchor 1); this test asserts the ui-layer wiring through the identity
// ctx in TestWSUI_MoveAttachesOperatorIdentity below.
func TestWSUI_MoveFileEndToEnd(t *testing.T) {
	m, s, _ := newSharedCenterManager(t)
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := s.Write(context.Background(), "", "ns", "proj", "old.md", "# Old\n", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Write(context.Background(), "", "ns", "proj", "ref.md", "see [x](old.md)\n", nil); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(m)
	defer srv.Close()

	connA := dialWS(t, srv.URL)
	defer connA.Close()
	connB := dialWS(t, srv.URL)
	defer connB.Close()
	time.Sleep(100 * time.Millisecond)

	sendWS(t, connA, MsgMoveFile, MoveFilePayload{
		Namespace:   "ns",
		ProjectName: "proj",
		SourcePath:  "old.md",
		TargetPath:  "new.md",
	})

	// A: MOVE_ACK with the new etag and the rewritten-links count.
	var ack MoveAckPayload
	readUntil(t, connA, MsgMoveAck, &ack, 2*time.Second)
	if ack.SourcePath != "old.md" || ack.TargetPath != "new.md" {
		t.Errorf("ack paths = %s -> %s", ack.SourcePath, ack.TargetPath)
	}
	if ack.NewETag == "" {
		t.Error("ack missing new_etag")
	}
	// Link auto-update on move is disabled (B-33): the ref.md referrer is left
	// untouched, so MOVE_ACK always reports 0 links rewritten.
	if ack.LinksRewritten != 0 {
		t.Errorf("ack links_rewritten = %d, want 0 (link rewrite on move is disabled)", ack.LinksRewritten)
	}

	// B: the file.move NOTIFY carrying both paths.
	connB.SetReadDeadline(time.Now().Add(2 * time.Second))
	var moved *notify.Event
	for moved == nil {
		var msg WSMessage
		if err := connB.ReadJSON(&msg); err != nil {
			t.Fatalf("B never received file.move: %v", err)
		}
		if msg.Type != MsgNotify {
			continue
		}
		var ev notify.Event
		if err := json.Unmarshal(msg.Payload, &ev); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if ev.Kind == "file.move" {
			moved = &ev
		}
	}
	if moved.SourcePath != "old.md" || moved.Path != "new.md" {
		t.Errorf("file.move event source=%q path=%q", moved.SourcePath, moved.Path)
	}
	if moved.Target != "ns/proj" {
		t.Errorf("file.move target = %q", moved.Target)
	}

	// A (originator) must NOT receive its own file.move.
	expectNoNotify(t, connA, "file.move", "new.md", 500*time.Millisecond)
}

// moveCtxCapturingStore records the context handleMoveFile hands to storage.Move,
// so the ui layer's identity/sender wiring can be asserted without reaching into
// git from another package (Anchor 1). It delegates to the embedded storage.
type moveCtxCapturingStore struct {
	storage.StorageService
	mu      sync.Mutex
	moveCtx context.Context
}

func (c *moveCtxCapturingStore) Move(ctx context.Context, sessionID, ns, proj, src, dst string, ifMatch *string) (string, int, error) {
	c.mu.Lock()
	c.moveCtx = ctx
	c.mu.Unlock()
	return c.StorageService.Move(ctx, sessionID, ns, proj, src, dst, ifMatch)
}

func (c *moveCtxCapturingStore) lastMoveCtx() context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.moveCtx
}

// TestWSUI_MoveAttachesOperatorIdentity asserts the ui-layer responsibility: a web
// MOVE_FILE is the operator acting as themselves, so handleMoveFile marks the
// context user-authored (identity.WithUser, no agent) and stamps the connection's
// sender so the file.move NOTIFY is not echoed back. (The resulting operator
// commit author is pinned by storage.TestMove_WebIsUserAuthored.)
func TestWSUI_MoveAttachesOperatorIdentity(t *testing.T) {
	m, base, _ := newSharedCenterManager(t)
	if err := base.CreateProject("ns", "proj"); err != nil {
		t.Fatal(err)
	}
	if _, err := base.Write(context.Background(), "", "ns", "proj", "old.md", "x", nil); err != nil {
		t.Fatal(err)
	}

	spy := &moveCtxCapturingStore{StorageService: base}
	srv := httptest.NewServer(NewManager(spy, m.drafts, nil))
	defer srv.Close()

	conn := dialWS(t, srv.URL)
	defer conn.Close()
	time.Sleep(50 * time.Millisecond)

	sendWS(t, conn, MsgMoveFile, MoveFilePayload{
		Namespace: "ns", ProjectName: "proj", SourcePath: "old.md", TargetPath: "new.md",
	})
	readUntil(t, conn, MsgMoveAck, nil, 2*time.Second)

	ctx := spy.lastMoveCtx()
	if ctx == nil {
		t.Fatal("handleMoveFile never called storage.Move")
	}
	if _, ok := identity.UserFrom(ctx); !ok {
		t.Error("web move context carries no user; handleMoveFile must set identity.WithUser")
	}
	if a, ok := identity.AgentFrom(ctx); ok && a.Name != "" {
		t.Errorf("web move context unexpectedly declares agent %q", a.Name)
	}
	if notify.SenderFrom(ctx) == "" {
		t.Error("web move context carries no sender; the originator would receive its own file.move")
	}
}
