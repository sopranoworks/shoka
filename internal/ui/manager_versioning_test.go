package ui

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/gorilla/websocket"
	"github.com/shoka/mcp-server/internal/drafts"
	"github.com/shoka/mcp-server/internal/identity"
	"github.com/shoka/mcp-server/internal/storage"
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

func TestWSUI_SaveAttributesAuthorToOperator(t *testing.T) {
	conn, s, dir, etag := versioningFixture(t)

	if r := roundTrip(t, conn, SaveFile,
		`{"namespace":"ns","projectName":"proj","path":"f.md","content":"v2","if_match":"`+etag+`"}`); r.Type != SaveAck {
		t.Fatalf("save failed: %s %s", r.Type, r.Payload)
	}

	// The commit is async (WAL worker); wait for it, then inspect HEAD.
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatalf("WAL did not drain (pending=%d)", s.WALPending())
	}

	r, err := git.PlainOpen(filepath.Join(dir, "ns", "proj"))
	if err != nil {
		t.Fatal(err)
	}
	ref, err := r.Head()
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}

	// A web SAVE_FILE is the operator acting as themselves: Author = operator user,
	// not the default agent. (Contrast: an MCP write is agent-authored — pinned by
	// internal/storage TestCommitIdentity_DefaultAgent.)
	if c.Author.Name != "Osamu Takahashi" || c.Author.Email != "forte.nit@gmail.com" {
		t.Errorf("author = %s <%s>, want the operator user", c.Author.Name, c.Author.Email)
	}
	if c.Committer.Name != "Osamu Takahashi" {
		t.Errorf("committer = %q, want the operator user", c.Committer.Name)
	}
	if !strings.Contains(c.Message, "Shoka-User: Osamu Takahashi <forte.nit@gmail.com>") {
		t.Errorf("missing user trailer:\n%s", c.Message)
	}
}
