package drafts

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/shoka/mcp-server/internal/utils"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // In production, this should be more restrictive
	},
}

// Manager handles draft persistence via WebSockets.
type Manager struct {
	baseDir string
	mu      sync.Mutex
}

// NewManager creates a new Manager with the specified base directory.
func NewManager(baseDir string) (*Manager, error) {
	absBaseDir, err := filepath.Abs(baseDir)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path for base directory: %w", err)
	}
	return &Manager{baseDir: absBaseDir}, nil
}

func (m *Manager) GetDraftPath(namespace, projectName, path string) (string, error) {
	if namespace == "" {
		namespace = "default"
	}
	if !utils.IsValidName(namespace) {
		return "", fmt.Errorf("invalid namespace: %s", namespace)
	}
	if !utils.IsValidName(projectName) {
		return "", fmt.Errorf("invalid project name: %s", projectName)
	}

	projectPath := filepath.Join(m.baseDir, namespace, projectName)
	draftsDir := filepath.Join(projectPath, ".drafts")
	fullPath := filepath.Join(draftsDir, path)

	// Robust path traversal protection for the draft file
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("invalid file path (absolute paths not allowed): %s", path)
	}
	rel, err := filepath.Rel(draftsDir, fullPath)
	if err != nil || strings.HasPrefix(rel, "..") || strings.HasPrefix(rel, "/") {
		return "", fmt.Errorf("invalid file path: %s", path)
	}

	return fullPath, nil
}

// HandleWebSocket upgrades the HTTP connection to a WebSocket and manages the draft session.
func (m *Manager) HandleWebSocket(w http.ResponseWriter, r *http.Request, namespace, projectName, path string) {
	draftPath, err := m.GetDraftPath(namespace, projectName, path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Send initial state
	m.mu.Lock()
	content, err := os.ReadFile(draftPath)
	m.mu.Unlock()
	if err == nil {
		if err := conn.WriteMessage(websocket.TextMessage, content); err != nil {
			return
		}
	}

	// Read loop
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			break
		}

		// Save message as draft
		if err := m.SaveDraft(draftPath, message); err != nil {
			// In a real app, we might want to log this or notify the client
			continue
		}
	}
}

func (m *Manager) SaveDraft(draftPath string, content []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(draftPath), 0755); err != nil {
		return fmt.Errorf("failed to create draft directory: %w", err)
	}

	if err := os.WriteFile(draftPath, content, 0644); err != nil {
		return fmt.Errorf("failed to write draft file: %w", err)
	}

	return nil
}

// ServeHTTP implements the http.Handler interface.
// It expects the path to be in the format /drafts/{namespace}/{projectName}
// and the filepath in the 'filepath' query parameter.
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Simple path parsing: /drafts/{namespace}/{projectName}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "drafts" {
		http.Error(w, "invalid path format, expected /drafts/{namespace}/{projectName}", http.StatusBadRequest)
		return
	}

	namespace := parts[1]
	projectName := parts[2]
	path := r.URL.Query().Get("filepath")
	if path == "" {
		http.Error(w, "missing 'filepath' query parameter", http.StatusBadRequest)
		return
	}

	m.HandleWebSocket(w, r, namespace, projectName, path)
}
