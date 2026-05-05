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

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type MessageType string

const (
	GetProjects      MessageType = "GET_PROJECTS"
	GetTree          MessageType = "GET_TREE"
	ReadFile         MessageType = "READ_FILE"
	WriteDraft       MessageType = "WRITE_DRAFT"
	SaveFile         MessageType = "SAVE_FILE"
	MsgCreateProject MessageType = "CREATE_PROJECT"
	Error            MessageType = "ERROR"
)

type WSMessage struct {
	Type    MessageType     `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type GetProjectsPayload struct {
	Namespace string `json:"namespace"`
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
	storage storage.StorageService
	drafts  *drafts.Manager
}

func NewManager(s storage.StorageService, d *drafts.Manager) *Manager {
	return &Manager{
		storage: s,
		drafts:  d,
	}
}

func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
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

func (m *Manager) handleGetProjects(conn *websocket.Conn, payload json.RawMessage) {
	var p GetProjectsPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		m.sendError(conn, "Invalid payload for GET_PROJECTS")
		return
	}

	projects, err := m.storage.ListProjects(p.Namespace)
	if err != nil {
		m.sendError(conn, fmt.Sprintf("Failed to list projects: %v", err))
		return
	}

	m.sendResponse(conn, GetProjects, projects)
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
	files, err := m.storage.ListFiles(namespace, projectName, path)
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

	m.sendResponse(conn, SaveFile, map[string]string{
		"status": "ok",
	})
}
