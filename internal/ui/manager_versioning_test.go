package ui

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/sopranoworks/shoka/internal/drafts"
	"github.com/sopranoworks/shoka/internal/identity"
	"github.com/sopranoworks/shoka/internal/storage"
)

// versioningFixture spins up a /ws/ui server backed by storage with a known
// operator identity, a project, and one committed file "f.md" containing "v1".
// It returns the live ws connection, the concrete storage (for WaitForWAL and git
// inspection), and the etag of "v1".
func versioningFixture(t *testing.T) (conn *websocket.Conn, s *storage.FSGitStorage, dir, etag string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "shoka-ui-ver-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	s, err = storage.NewFSGitStorageWithOptions(dir, storage.Options{
		Identity: identity.Defaults{
			UserName:  "Osamu Takahashi",
			UserEmail: "forte.nit@gmail.com",
			AgentName: "shoka-agent",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatal(err)
	}
	etag, err = s.Write(context.Background(), "", "ns", "proj", "f.md", "v1", nil)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(NewManager(s, mustDrafts(t, dir), nil))
	t.Cleanup(server.Close)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err = websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, s, dir, etag
}

func mustDrafts(t *testing.T, dir string) *drafts.Manager {
	t.Helper()
	dm, err := drafts.NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	return dm
}

// roundTrip sends one request and reads one response frame.
func roundTrip(t *testing.T, conn *websocket.Conn, typ MessageType, payload string) WSMessage {
	t.Helper()
	if err := conn.WriteJSON(WSMessage{Type: typ, Payload: json.RawMessage(payload)}); err != nil {
		t.Fatalf("write %s: %v", typ, err)
	}
	var resp WSMessage
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("read response to %s: %v", typ, err)
	}
	return resp
}

func TestWSUI_ReadFileReturnsETag(t *testing.T) {
	conn, _, _, etag := versioningFixture(t)

	resp := roundTrip(t, conn, ReadFile, `{"namespace":"ns","projectName":"proj","path":"f.md"}`)
	if resp.Type != ReadFile {
		t.Fatalf("type = %s, want READ_FILE", resp.Type)
	}
	var body map[string]string
	if err := json.Unmarshal(resp.Payload, &body); err != nil {
		t.Fatal(err)
	}
	if body["content"] != "v1" {
		t.Errorf("content = %q, want v1", body["content"])
	}
	if body["etag"] == "" {
		t.Fatal("READ_FILE response carries no etag")
	}
	if body["etag"] != etag {
		t.Errorf("etag = %q, want %q (the write etag)", body["etag"], etag)
	}
}

func TestWSUI_SaveWithMatchingIfMatchSucceeds(t *testing.T) {
	conn, _, _, etag := versioningFixture(t)

	resp := roundTrip(t, conn, SaveFile,
		`{"namespace":"ns","projectName":"proj","path":"f.md","content":"v2","if_match":"`+etag+`"}`)
	if resp.Type != SaveAck {
		t.Fatalf("type = %s, want SAVE_ACK (payload=%s)", resp.Type, resp.Payload)
	}
	var body map[string]string
	if err := json.Unmarshal(resp.Payload, &body); err != nil {
		t.Fatal(err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q", body["status"])
	}
	if body["etag"] == "" || body["etag"] == etag {
		t.Errorf("SAVE_ACK should return a fresh etag, got %q (old was %q)", body["etag"], etag)
	}
}

func TestWSUI_SaveWithStaleIfMatchReturnsConflict(t *testing.T) {
	conn, _, _, etag := versioningFixture(t)

	// First save moves the file off the original etag.
	if r := roundTrip(t, conn, SaveFile,
		`{"namespace":"ns","projectName":"proj","path":"f.md","content":"v2","if_match":"`+etag+`"}`); r.Type != SaveAck {
		t.Fatalf("setup save failed: %s %s", r.Type, r.Payload)
	}

	// Second save reuses the now-stale original etag → CONFLICT.
	resp := roundTrip(t, conn, SaveFile,
		`{"namespace":"ns","projectName":"proj","path":"f.md","content":"v3","if_match":"`+etag+`"}`)
	if resp.Type != MsgConflict {
		t.Fatalf("type = %s, want CONFLICT (payload=%s)", resp.Type, resp.Payload)
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
	if resp.Type == Error {
		t.Error("conflict must be structurally distinct from the generic ERROR frame")
	}
}

func TestWSUI_SaveWithoutIfMatchBackwardCompat(t *testing.T) {
	conn, _, _, _ := versioningFixture(t)

	// A client that has not adopted versioning omits if_match: the write still
	// succeeds via the unchecked path (today's behaviour).
	resp := roundTrip(t, conn, SaveFile,
		`{"namespace":"ns","projectName":"proj","path":"f.md","content":"vX"}`)
	if resp.Type != SaveAck {
		t.Fatalf("type = %s, want SAVE_ACK (payload=%s)", resp.Type, resp.Payload)
	}
	var body map[string]string
	if err := json.Unmarshal(resp.Payload, &body); err != nil {
		t.Fatal(err)
	}
	if body["etag"] == "" {
		t.Error("SAVE_ACK should still return the new etag even without if_match")
	}
}

// ctxCapturingStore wraps a StorageService and records the context handed to
// Write. It lets this ui-layer test assert WHAT identity handleSaveFile attaches
// to the write context — the ui layer's actual responsibility — without reaching
// into storage's git state from another package (a layering violation, and an
// Anchor-1 go-git import outside the storage submodule). The other half of the
// guarantee — "a WithUser context produces an operator-authored commit" — is
// storage's own responsibility, pinned inside the submodule by
// storage.TestCommitIdentity_WithUserMakesUserAuthor.
type ctxCapturingStore struct {
	storage.StorageService
	mu       sync.Mutex
	writeCtx context.Context
}

func (c *ctxCapturingStore) Write(ctx context.Context, sessionID, ns, proj, path, content string, ifMatch *string) (string, error) {
	c.mu.Lock()
	c.writeCtx = ctx
	c.mu.Unlock()
	return c.StorageService.Write(ctx, sessionID, ns, proj, path, content, ifMatch)
}

func (c *ctxCapturingStore) lastWriteCtx() context.Context {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeCtx
}

func TestWSUI_SaveAttachesOperatorIdentityToContext(t *testing.T) {
	dir, err := os.MkdirTemp("", "shoka-ui-ctx-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	base, err := storage.NewFSGitStorageWithOptions(dir, storage.Options{
		Identity: identity.Defaults{
			UserName:  "Osamu Takahashi",
			UserEmail: "forte.nit@gmail.com",
			AgentName: "shoka-agent",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close() })
	if err := base.CreateProject("ns", "proj"); err != nil {
		t.Fatal(err)
	}
	etag, err := base.Write(context.Background(), "", "ns", "proj", "f.md", "v1", nil)
	if err != nil {
		t.Fatal(err)
	}

	spy := &ctxCapturingStore{StorageService: base}
	server := httptest.NewServer(NewManager(spy, mustDrafts(t, dir), nil))
	t.Cleanup(server.Close)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })

	if r := roundTrip(t, conn, SaveFile,
		`{"namespace":"ns","projectName":"proj","path":"f.md","content":"v2","if_match":"`+etag+`"}`); r.Type != SaveAck {
		t.Fatalf("save failed: %s %s", r.Type, r.Payload)
	}

	// The ui layer's responsibility: a web SAVE_FILE is the operator acting as
	// themselves, so handleSaveFile must mark the write context as user-authored
	// (identity.WithUser) rather than leaving it agent-authored. Presence of a user
	// on the ctx is the marker — single-user mode carries an empty User; a future
	// authenticated request substitutes the real one at the same call site.
	ctx := spy.lastWriteCtx()
	if ctx == nil {
		t.Fatal("handleSaveFile never called storage.Write")
	}
	if _, ok := identity.UserFrom(ctx); !ok {
		t.Error("web save context carries no user; handleSaveFile must set identity.WithUser so the commit is operator-authored")
	}
	// And it must NOT declare an agent — a web save is the operator, not an agent.
	if a, ok := identity.AgentFrom(ctx); ok && a.Name != "" {
		t.Errorf("web save context unexpectedly declares agent %q", a.Name)
	}
}
