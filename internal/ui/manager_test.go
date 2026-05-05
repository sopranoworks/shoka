package ui

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/shoka/mcp-server/internal/drafts"
	"github.com/shoka/mcp-server/internal/storage"
)

func TestManager_ServeHTTP(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "shoka-ui-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	s, err := storage.NewFSGitStorage(tmpDir)
	if err != nil {
		t.Fatalf("failed to create storage: %v", err)
	}

	dm, err := drafts.NewManager(tmpDir)
	if err != nil {
		t.Fatalf("failed to create draft manager: %v", err)
	}

	m := NewManager(s, dm)
	server := httptest.NewServer(m)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to connect to websocket: %v", err)
	}
	defer conn.Close()

	// Test GET_PROJECTS
	req := WSMessage{
		Type:    GetProjects,
		Payload: json.RawMessage(`{"namespace": "default"}`),
	}
	if err := conn.WriteJSON(req); err != nil {
		t.Fatalf("failed to send GET_PROJECTS: %v", err)
	}

	var resp WSMessage
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("failed to read GET_PROJECTS response: %v", err)
	}
	if resp.Type != GetProjects {
		t.Errorf("expected response type %s, got %s", GetProjects, resp.Type)
	}

	// Test GET_TREE (empty project)
	if err := s.CreateProject("default", "test-project"); err != nil {
		t.Fatalf("failed to create project: %v", err)
	}
	req = WSMessage{
		Type:    GetTree,
		Payload: json.RawMessage(`{"namespace": "default", "projectName": "test-project"}`),
	}
	if err := conn.WriteJSON(req); err != nil {
		t.Fatalf("failed to send GET_TREE: %v", err)
	}
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("failed to read GET_TREE response: %v", err)
	}
	if resp.Type != GetTree {
		t.Errorf("expected response type %s, got %s", GetTree, resp.Type)
	}

	// Test READ_FILE
	if err := s.WriteFile("default", "test-project", "hello.txt", "world"); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	req = WSMessage{
		Type:    ReadFile,
		Payload: json.RawMessage(`{"namespace": "default", "projectName": "test-project", "path": "hello.txt"}`),
	}
	if err := conn.WriteJSON(req); err != nil {
		t.Fatalf("failed to send READ_FILE: %v", err)
	}
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("failed to read READ_FILE response: %v", err)
	}
	if resp.Type != ReadFile {
		t.Errorf("expected response type %s, got %s", ReadFile, resp.Type)
	}

	// Test WRITE_DRAFT
	req = WSMessage{
		Type:    WriteDraft,
		Payload: json.RawMessage(`{"namespace": "default", "projectName": "test-project", "path": "hello.txt", "content": "draft world"}`),
	}
	if err := conn.WriteJSON(req); err != nil {
		t.Fatalf("failed to send WRITE_DRAFT: %v", err)
	}
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("failed to read WRITE_DRAFT response: %v", err)
	}
	if resp.Type != WriteDraft {
		t.Errorf("expected response type %s, got %s", WriteDraft, resp.Type)
	}

	// Verify draft file
	draftPath, _ := dm.GetDraftPath("default", "test-project", "hello.txt")
	content, err := os.ReadFile(draftPath)
	if err != nil {
		t.Fatalf("failed to read draft file: %v", err)
	}
	if string(content) != "draft world" {
		t.Errorf("expected draft content 'draft world', got %q", string(content))
	}
}
