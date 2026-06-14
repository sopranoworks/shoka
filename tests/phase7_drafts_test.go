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
	"github.com/stretchr/testify/require"
)

// When a draft cannot be persisted, the client must be told rather than having
// its keystrokes silently dropped.
func TestDraftSaveFailureNotifiesClient(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "shoka-draft-err-*")
	require.NoError(t, err)
	defer os.RemoveAll(tempDir)

	// Place a regular file where the draft's parent directory would need to be,
	// so MkdirAll (and therefore SaveDraft) fails.
	draftsDir := filepath.Join(tempDir, "ns1", "proj1", ".drafts")
	require.NoError(t, os.MkdirAll(draftsDir, 0755))
	require.NoError(t, os.WriteFile(filepath.Join(draftsDir, "sub"), []byte("x"), 0644))

	m, err := drafts.NewManager(tempDir)
	require.NoError(t, err)
	server := httptest.NewServer(m)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/drafts/ns1/proj1?filepath=sub/inner.md"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	defer conn.Close()

	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte("some draft content")))

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(3*time.Second)))
	_, msg, err := conn.ReadMessage()
	require.NoError(t, err, "expected an error notification from the server, not silence")
	assert.True(t, strings.HasPrefix(string(msg), "ERROR"), "expected an ERROR notification, got %q", string(msg))
}
