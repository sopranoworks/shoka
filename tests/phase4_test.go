package tests

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sopranoworks/shoka/internal/drafts"
	"github.com/stretchr/testify/assert"
)

func TestDraftingIntegration(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "shoka-phase4-test-*")
	assert.NoError(t, err)
	defer os.RemoveAll(tempDir)

	manager, err := drafts.NewManager(tempDir)
	assert.NoError(t, err)

	server := httptest.NewServer(manager)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/drafts/ns1/proj1?filepath=test.md"

	// 1. First connection: send some draft content
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	assert.NoError(t, err)

	draftContent := "Hello, this is a draft."
	err = conn.WriteMessage(websocket.TextMessage, []byte(draftContent))
	assert.NoError(t, err)

	// Wait a bit for persistence
	time.Sleep(100 * time.Millisecond)
	conn.Close()

	// Verify file exists on disk
	expectedPath := filepath.Join(tempDir, "ns1", "proj1", ".drafts", "test.md")
	content, err := os.ReadFile(expectedPath)
	assert.NoError(t, err)
	assert.Equal(t, draftContent, string(content))

	// 2. Reconnection: should receive the initial content
	conn2, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	assert.NoError(t, err)
	defer conn2.Close()

	_, received, err := conn2.ReadMessage()
	assert.NoError(t, err)
	assert.Equal(t, draftContent, string(received))

	// Update draft
	updatedContent := "Updated draft content."
	err = conn2.WriteMessage(websocket.TextMessage, []byte(updatedContent))
	assert.NoError(t, err)

	time.Sleep(100 * time.Millisecond)

	// Verify update on disk
	content, err = os.ReadFile(expectedPath)
	assert.NoError(t, err)
	assert.Equal(t, updatedContent, string(content))
}
