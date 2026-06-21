package ui

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/identity"
	"github.com/sopranoworks/shoka/internal/notify"
	"github.com/sopranoworks/shoka/internal/storage"

	"github.com/sopranoworks/shoka/pkg/uiws"
)

// TestWSUI_DeleteFileEndToEnd is the directive's /ws/ui completion criterion for
// the trash-can model's server half: a DELETE_FILE from connection A produces a
// DELETE_ACK to A, removes the file (gone from the working tree, and a delete
// commit recorded once the WAL drains), pushes a file.delete NOTIFY to connection
// B, and does NOT echo that NOTIFY back to A (sender exclusion — A drove its own
// delete behind the client grace, so it self-refreshes). It wires the EXISTING
// storage.Delete; there is no storage or tool change.
func TestWSUI_DeleteFileEndToEnd(t *testing.T) {
	m, s, _ := newSharedCenterManager(t)
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if _, err := s.Write(context.Background(), "", "ns", "proj", "doomed.md", "# Doomed\n", nil); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(m)
	defer srv.Close()

	connA := dialWS(t, srv.URL)
	defer connA.Close()
	connB := dialWS(t, srv.URL)
	defer connB.Close()
	time.Sleep(100 * time.Millisecond)

	sendWS(t, connA, MsgDeleteFile, DeleteFilePayload{
		Namespace:   "ns",
		ProjectName: "proj",
		Path:        "doomed.md",
	})

	// A: DELETE_ACK carrying the deleted path.
	var ack DeleteAckPayload
	readUntil(t, connA, MsgDeleteAck, &ack, 2*time.Second)
	if ack.Path != "doomed.md" {
		t.Errorf("ack path = %q, want doomed.md", ack.Path)
	}

	// File is gone from the working tree immediately (Delete removes under the lock
	// before it notifies).
	if _, _, err := s.ReadFileWithETag("ns", "proj", "doomed.md"); err == nil {
		t.Error("file still readable after DELETE_FILE; expected it to be gone")
	}

	// B: the file.delete NOTIFY (published only on success, i.e. the write path ran
	// and the commit is being recorded).
	connB.SetReadDeadline(time.Now().Add(2 * time.Second))
	var deleted *notify.Event
	for deleted == nil {
		var msg uiws.WSMessage
		if err := connB.ReadJSON(&msg); err != nil {
			t.Fatalf("B never received file.delete: %v", err)
		}
		if msg.Type != MsgNotify {
			continue
		}
		var ev notify.Event
		if err := json.Unmarshal(msg.Payload, &ev); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		if ev.Kind == "file.delete" {
			deleted = &ev
		}
	}
	if deleted.Path != "doomed.md" {
		t.Errorf("file.delete event path = %q, want doomed.md", deleted.Path)
	}
	if deleted.Target != "ns/proj" {
		t.Errorf("file.delete target = %q, want ns/proj", deleted.Target)
	}

	// A (originator) must NOT receive its own file.delete.
	expectNoNotify(t, connA, "file.delete", "doomed.md", 500*time.Millisecond)

	// Commit recorded: once the WAL drains, the path's history carries the create
	// and the delete (>= 2 commits) — the delete is git-tracked, recoverable via
	// History (not a lost+found drop).
	if !s2(t, s).WaitForWAL(10 * time.Second) {
		t.Fatal("WAL did not drain")
	}
	hist, err := s.GetHistory("ns", "proj", "doomed.md", 10)
	if err != nil {
		t.Fatalf("history after delete: %v", err)
	}
	if len(hist) < 2 {
		t.Errorf("history has %d commits, want >= 2 (create + delete)", len(hist))
	}
}

// s2 narrows the StorageService the shared-center manager exposes to the concrete
// *FSGitStorage so the test can call WaitForWAL/GetHistory (the move e2e test
// pins the operator-authored-commit half in the storage submodule; here we assert
// the commit is recorded via the path's own history, no go-git import needed).
func s2(t *testing.T, s storage.StorageService) *storage.FSGitStorage {
	t.Helper()
	fs, ok := s.(*storage.FSGitStorage)
	if !ok {
		t.Fatalf("storage is %T, want *storage.FSGitStorage", s)
	}
	return fs
}

// TestWSUI_DeleteStaleIfMatchReturnsConflict pins the optimistic-concurrency
// parity: a DELETE_FILE carrying an if_match that no longer matches (the file was
// edited during the client-side grace) returns the SAME CONFLICT frame
// SAVE_FILE/MOVE_FILE use — carrying the file's current etag and structurally
// distinct from the generic ERROR — so the file is not silently destroyed.
func TestWSUI_DeleteStaleIfMatchReturnsConflict(t *testing.T) {
	conn, s, _, etag := versioningFixture(t)

	// Edit f.md so the original etag goes stale (mid-grace change).
	if r := roundTrip(t, conn, SaveFile,
		`{"namespace":"ns","projectName":"proj","path":"f.md","content":"v2","if_match":"`+etag+`"}`); r.Type != SaveAck {
		t.Fatalf("setup save failed: %s %s", r.Type, r.Payload)
	}

	// Delete with the now-stale etag → CONFLICT, not a delete.
	resp := roundTrip(t, conn, MsgDeleteFile,
		`{"namespace":"ns","projectName":"proj","path":"f.md","if_match":"`+etag+`"}`)
	if resp.Type != MsgConflict {
		t.Fatalf("type = %s, want CONFLICT (payload=%s)", resp.Type, resp.Payload)
	}
	if resp.Type == uiws.Error {
		t.Error("conflict must be structurally distinct from the generic ERROR frame")
	}
	var c ConflictPayload
	if err := json.Unmarshal(resp.Payload, &c); err != nil {
		t.Fatal(err)
	}
	if c.Path != "f.md" {
		t.Errorf("conflict path = %q", c.Path)
	}
	if c.CurrentETag == "" || c.CurrentETag == etag {
		t.Errorf("conflict current_etag should be the new etag, got %q (stale was %q)", c.CurrentETag, etag)
	}

	// The file survived the rejected delete.
	if _, _, err := s.ReadFileWithETag("ns", "proj", "f.md"); err != nil {
		t.Errorf("file should survive a conflicting delete, but read failed: %v", err)
	}
}

// deleteCtxCapturingStore records the context handleDeleteFile hands to
// storage.Delete, so the ui layer's identity/sender wiring can be asserted
// directly (the operator-authored commit is the storage submodule's concern).
type deleteCtxCapturingStore struct {
	storage.StorageService
	mu        sync.Mutex
	deleteCtx context.Context
}

func (c *deleteCtxCapturingStore) Delete(ctx context.Context, sessionID, ns, proj, path string, ifMatch *string) error {
	c.mu.Lock()
	c.deleteCtx = ctx
	c.mu.Unlock()
	return c.StorageService.Delete(ctx, sessionID, ns, proj, path, ifMatch)
}

func (c *deleteCtxCapturingStore) lastDeleteCtx() context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.deleteCtx
}

// TestWSUI_DeleteAttachesOperatorIdentity asserts the ui-layer responsibility: a
// web DELETE_FILE is the operator acting as themselves, so handleDeleteFile marks
// the context user-authored (identity.WithUser, no agent) and stamps the
// connection's sender so the file.delete NOTIFY is not echoed back.
func TestWSUI_DeleteAttachesOperatorIdentity(t *testing.T) {
	m, base, _ := newSharedCenterManager(t)
	if err := base.CreateProject("ns", "proj"); err != nil {
		t.Fatal(err)
	}
	if _, err := base.Write(context.Background(), "", "ns", "proj", "gone.md", "x", nil); err != nil {
		t.Fatal(err)
	}

	spy := &deleteCtxCapturingStore{StorageService: base}
	srv := httptest.NewServer(NewManager(spy, m.drafts, nil))
	defer srv.Close()

	conn := dialWS(t, srv.URL)
	defer conn.Close()
	time.Sleep(50 * time.Millisecond)

	sendWS(t, conn, MsgDeleteFile, DeleteFilePayload{
		Namespace: "ns", ProjectName: "proj", Path: "gone.md",
	})
	readUntil(t, conn, MsgDeleteAck, nil, 2*time.Second)

	ctx := spy.lastDeleteCtx()
	if ctx == nil {
		t.Fatal("handleDeleteFile never called storage.Delete")
	}
	if _, ok := identity.UserFrom(ctx); !ok {
		t.Error("web delete context carries no user; handleDeleteFile must set identity.WithUser")
	}
	if a, ok := identity.AgentFrom(ctx); ok && a.Name != "" {
		t.Errorf("web delete context unexpectedly declares agent %q", a.Name)
	}
	if notify.SenderFrom(ctx) == "" {
		t.Error("web delete context carries no sender; the originator would receive its own file.delete")
	}
}
