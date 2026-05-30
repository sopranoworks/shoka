package ui

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/shoka/mcp-server/internal/drafts"
	"github.com/shoka/mcp-server/internal/storage"
)

type MessageType string

const (
	GetProjects      MessageType = "GET_PROJECTS"
	GetTree          MessageType = "GET_TREE"
	ReadFile         MessageType = "READ_FILE"
	WriteDraft       MessageType = "WRITE_DRAFT"
	SaveFile         MessageType = "SAVE_FILE"
	SaveAck          MessageType = "SAVE_ACK"
	MsgCreateProject MessageType = "CREATE_PROJECT"
	Error            MessageType = "ERROR"
)

type WSMessage struct {
	Type    MessageType     `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type CreateProjectPayload struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
}

type GetTreePayload struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
}

type ReadFilePayload struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
	Path        string `json:"path"`
}

type WriteDraftPayload struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
	Path        string `json:"path"`
	Content     string `json:"content"`
}

type SaveFilePayload struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
	Path        string `json:"path"`
	Content     string `json:"content"`
}

type FileNode struct {
	Name     string     `json:"name"`
	Path     string     `json:"path"`
	IsDir    bool       `json:"isDir"`
	Children []FileNode `json:"children,omitempty"`
}

type Manager struct {
	storage       storage.StorageService
	drafts        *drafts.Manager
	originChecker func(*http.Request) bool
	upgrader      websocket.Upgrader
}

func NewManager(s storage.StorageService, d *drafts.Manager) *Manager {
	m := &Manager{
		storage: s,
		drafts:  d,
	}
	m.upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			if m.originChecker != nil {
				return m.originChecker(r)
			}
			return true
		},
	}
	return m
}

// SetOriginChecker installs a WebSocket origin policy. When unset (the default),
// all origins are accepted.
func (m *Manager) SetOriginChecker(fn func(*http.Request) bool) {
	m.originChecker = fn
}

func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := m.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade error: %v", err)
		return
	}
	defer conn.Close()

	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var wsMsg WSMessage
		if err := json.Unmarshal(message, &wsMsg); err != nil {
			m.sendError(conn, "Invalid message format")
			continue
		}

		switch wsMsg.Type {
		case GetProjects:
			m.handleGetProjects(conn, wsMsg.Payload)
		case GetTree:
			m.handleGetTree(conn, wsMsg.Payload)
		case ReadFile:
			m.handleReadFile(conn, wsMsg.Payload)
		case WriteDraft:
			m.handleWriteDraft(conn, wsMsg.Payload)
		case SaveFile:
			m.handleSaveFile(conn, wsMsg.Payload)
		case MsgCreateProject:
			m.handleCreateProject(conn, wsMsg.Payload)
		default:
			m.sendError(conn, "Unknown message type")
		}
	}
}

func (m *Manager) sendError(conn *websocket.Conn, errMsg string) {
	msg := WSMessage{
		Type:    Error,
		Payload: json.RawMessage(fmt.Sprintf(`{"message": %q}`, errMsg)),
	}
	data, _ := json.Marshal(msg)
	conn.WriteMessage(websocket.TextMessage, data)
}

func (m *Manager) sendResponse(conn *websocket.Conn, msgType MessageType, payload interface{}) {
	payloadData, err := json.Marshal(payload)
	if err != nil {
		m.sendError(conn, "Failed to marshal response")
		return
	}
	msg := WSMessage{
		Type:    msgType,
		Payload: json.RawMessage(payloadData),
	}
	data, err := json.Marshal(msg)
	if err != nil {
		m.sendError(conn, "Failed to marshal message")
		return
	}
	conn.WriteMessage(websocket.TextMessage, data)
}

// ProjectInfo is one project entry in the GET_PROJECTS response: its namespace,
// name, and health state (healthy | corrupted | dangerous) for the status badge.
// The namespace lets the Web UI group and filter across namespaces (B-13 / B-22).
type ProjectInfo struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	State     string `json:"state"`
}

// projectStateReader is the optional capability the storage layer provides for
// per-project health; type-asserted so the UI need not widen StorageService.
type projectStateReader interface {
	State(namespace, projectName string) storage.ProjectState
}

// handleGetProjects returns one entry per project across every namespace, each
// carrying its namespace, name, and health state. The payload's namespace field
// is ignored: the Web UI receives the full set and filters client-side (B-13 /
// B-22). The state badge and recovery dialog (storage redesign) read the same
// state field, unchanged.
func (m *Manager) handleGetProjects(conn *websocket.Conn, payload json.RawMessage) {
	namespaces, err := m.storage.ListNamespaces()
	if err != nil {
		m.sendError(conn, fmt.Sprintf("Failed to list namespaces: %v", err))
		return
	}

	sr, _ := m.storage.(projectStateReader)
	infos := make([]ProjectInfo, 0)
	for _, ns := range namespaces {
		projects, err := m.storage.ListProjects(ns)
		if err != nil {
			m.sendError(conn, fmt.Sprintf("Failed to list projects: %v", err))
			return
		}
		for _, name := range projects {
			state := string(storage.StateHealthy)
			if sr != nil {
				state = string(sr.State(ns, name))
			}
			infos = append(infos, ProjectInfo{Namespace: ns, Name: name, State: state})
		}
	}
	m.sendResponse(conn, GetProjects, infos)
}

func (m *Manager) handleCreateProject(conn *websocket.Conn, payload json.RawMessage) {
	var p CreateProjectPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		m.sendError(conn, "Invalid payload for CREATE_PROJECT")
		return
	}

	if err := m.storage.CreateProject(p.Namespace, p.ProjectName); err != nil {
		m.sendError(conn, fmt.Sprintf("Failed to create project: %v", err))
		return
	}

	m.sendResponse(conn, MsgCreateProject, map[string]string{
		"status": "ok",
	})
}

func (m *Manager) handleGetTree(conn *websocket.Conn, payload json.RawMessage) {
	var p GetTreePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		m.sendError(conn, "Invalid payload for GET_TREE")
		return
	}

	tree, err := m.getTree(p.Namespace, p.ProjectName, "")
	if err != nil {
		m.sendError(conn, fmt.Sprintf("Failed to get tree: %v", err))
		return
	}

	m.sendResponse(conn, GetTree, tree)
}

func (m *Manager) getTree(namespace, projectName, path string) ([]FileNode, error) {
	files, _, err := m.storage.ListFiles(namespace, projectName, path)
	if err != nil {
		return nil, err
	}

	var nodes []FileNode
	for _, f := range files {
		isDir := strings.HasSuffix(f, "/")
		name := strings.TrimSuffix(f, "/")
		nodePath := filepath.Join(path, name)

		node := FileNode{
			Name:  name,
			Path:  nodePath,
			IsDir: isDir,
		}

		if isDir {
			children, err := m.getTree(namespace, projectName, nodePath)
			if err != nil {
				return nil, err
			}
			node.Children = children
		}

		nodes = append(nodes, node)
	}

	return nodes, nil
}

func (m *Manager) handleReadFile(conn *websocket.Conn, payload json.RawMessage) {
	var p ReadFilePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		m.sendError(conn, "Invalid payload for READ_FILE")
		return
	}

	content, err := m.storage.ReadFile(p.Namespace, p.ProjectName, p.Path)
	if err != nil {
		m.sendError(conn, fmt.Sprintf("Failed to read file: %v", err))
		return
	}

	m.sendResponse(conn, ReadFile, map[string]string{
		"path":    p.Path,
		"content": content,
	})
}

func (m *Manager) handleWriteDraft(conn *websocket.Conn, payload json.RawMessage) {
	var p WriteDraftPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		m.sendError(conn, "Invalid payload for WRITE_DRAFT")
		return
	}

	draftPath, err := m.drafts.GetDraftPath(p.Namespace, p.ProjectName, p.Path)
	if err != nil {
		m.sendError(conn, fmt.Sprintf("Failed to get draft path: %v", err))
		return
	}

	if err := m.drafts.SaveDraft(draftPath, []byte(p.Content)); err != nil {
		m.sendError(conn, fmt.Sprintf("Failed to save draft: %v", err))
		return
	}

	m.sendResponse(conn, WriteDraft, map[string]string{
		"status": "ok",
	})
}

func (m *Manager) handleSaveFile(conn *websocket.Conn, payload json.RawMessage) {
	var p SaveFilePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		m.sendError(conn, "Invalid payload for SAVE_FILE")
		return
	}

	if err := m.storage.WriteFile(p.Namespace, p.ProjectName, p.Path, p.Content); err != nil {
		m.sendError(conn, fmt.Sprintf("Failed to save file: %v", err))
		return
	}

	m.sendResponse(conn, SaveAck, map[string]string{
		"path":   p.Path,
		"status": "ok",
	})
}
