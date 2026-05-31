package ui

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
	"github.com/shoka/mcp-server/internal/drafts"
	"github.com/shoka/mcp-server/internal/notify"
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
	// MsgNotify carries one notify.Event pushed from the server to the browser
	// (the 2026-05-31 auto-refresh directive). It is additive: it rides the same
	// {type,payload} envelope as every other message, so existing consumers that
	// switch on type are unaffected.
	MsgNotify MessageType = "NOTIFY"
	Error     MessageType = "ERROR"
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

// wsClient wraps one WebSocket connection with a write mutex. gorilla/websocket
// permits only one concurrent writer per connection; the read-loop's responses
// and the notify-drain goroutine both write, so every write goes through here.
type wsClient struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
}

func (c *wsClient) writeMessage(msgType MessageType, payload interface{}) error {
	payloadData, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	msg := WSMessage{Type: msgType, Payload: json.RawMessage(payloadData)}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c *wsClient) sendError(errMsg string) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	msg := WSMessage{
		Type:    Error,
		Payload: json.RawMessage(fmt.Sprintf(`{"message": %q}`, errMsg)),
	}
	data, _ := json.Marshal(msg)
	_ = c.conn.WriteMessage(websocket.TextMessage, data)
}

func (c *wsClient) sendResponse(msgType MessageType, payload interface{}) {
	if err := c.writeMessage(msgType, payload); err != nil {
		c.sendError("Failed to marshal response")
	}
}

type Manager struct {
	storage       storage.StorageService
	drafts        *drafts.Manager
	notify        *notify.Center
	originChecker func(*http.Request) bool
	upgrader      websocket.Upgrader
	notifyDrops   atomic.Int64
}

// NewManager builds the /ws/ui manager. notifyCenter may be nil (e.g. in tests);
// when nil, no NOTIFY events are pushed but every other message works unchanged.
func NewManager(s storage.StorageService, d *drafts.Manager, notifyCenter *notify.Center) *Manager {
	m := &Manager{
		storage: s,
		drafts:  d,
		notify:  notifyCenter,
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

// NotifyDrops reports how many notify events were dropped because a client's
// send buffer was full (observability; used by tests).
func (m *Manager) NotifyDrops() int64 { return m.notifyDrops.Load() }

func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := m.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade error: %v", err)
		return
	}
	defer conn.Close()
	client := &wsClient{conn: conn}

	// Subscribe to the notification center and forward events to this browser as
	// NOTIFY messages. The callback is non-blocking: it pushes onto a bounded
	// buffer and drops on full so a slow client cannot stall the publisher
	// (directive §4.2 / §5.3). A nil center yields a no-op subscription.
	events := make(chan notify.Event, 64)
	unsubscribe := m.notify.Subscribe(func(e notify.Event) {
		select {
		case events <- e:
		default:
			m.notifyDrops.Add(1)
			log.Printf("notify subscriber buffer full, dropping event kind=%s target=%s path=%s",
				e.Kind, e.Target, e.Path)
		}
	})

	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for e := range events {
			if err := client.writeMessage(MsgNotify, e); err != nil {
				return // write failure: the connection is going away
			}
		}
	}()

	// Read loop: request/response, unchanged in behaviour.
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var wsMsg WSMessage
		if err := json.Unmarshal(message, &wsMsg); err != nil {
			client.sendError("Invalid message format")
			continue
		}

		switch wsMsg.Type {
		case GetProjects:
			m.handleGetProjects(client, wsMsg.Payload)
		case GetTree:
			m.handleGetTree(client, wsMsg.Payload)
		case ReadFile:
			m.handleReadFile(client, wsMsg.Payload)
		case WriteDraft:
			m.handleWriteDraft(client, wsMsg.Payload)
		case SaveFile:
			m.handleSaveFile(client, wsMsg.Payload)
		case MsgCreateProject:
			m.handleCreateProject(client, wsMsg.Payload)
		default:
			client.sendError("Unknown message type")
		}
	}

	// Connection is closing. Unsubscribe first so no further callback runs (the
	// unsubscribe returns only after any in-flight fan-out completes), then close
	// the channel so the drain goroutine exits. Ordering avoids send-on-closed.
	unsubscribe()
	close(events)
	<-drainDone
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
func (m *Manager) handleGetProjects(client *wsClient, payload json.RawMessage) {
	namespaces, err := m.storage.ListNamespaces()
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to list namespaces: %v", err))
		return
	}

	sr, _ := m.storage.(projectStateReader)
	infos := make([]ProjectInfo, 0)
	for _, ns := range namespaces {
		projects, err := m.storage.ListProjects(ns)
		if err != nil {
			client.sendError(fmt.Sprintf("Failed to list projects: %v", err))
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
	client.sendResponse(GetProjects, infos)
}

func (m *Manager) handleCreateProject(client *wsClient, payload json.RawMessage) {
	var p CreateProjectPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for CREATE_PROJECT")
		return
	}

	if err := m.storage.CreateProject(p.Namespace, p.ProjectName); err != nil {
		client.sendError(fmt.Sprintf("Failed to create project: %v", err))
		return
	}

	client.sendResponse(MsgCreateProject, map[string]string{
		"status": "ok",
	})
}

func (m *Manager) handleGetTree(client *wsClient, payload json.RawMessage) {
	var p GetTreePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for GET_TREE")
		return
	}

	tree, err := m.getTree(p.Namespace, p.ProjectName, "")
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to get tree: %v", err))
		return
	}

	client.sendResponse(GetTree, tree)
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

func (m *Manager) handleReadFile(client *wsClient, payload json.RawMessage) {
	var p ReadFilePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for READ_FILE")
		return
	}

	content, err := m.storage.ReadFile(p.Namespace, p.ProjectName, p.Path)
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to read file: %v", err))
		return
	}

	client.sendResponse(ReadFile, map[string]string{
		"path":    p.Path,
		"content": content,
	})
}

func (m *Manager) handleWriteDraft(client *wsClient, payload json.RawMessage) {
	var p WriteDraftPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for WRITE_DRAFT")
		return
	}

	draftPath, err := m.drafts.GetDraftPath(p.Namespace, p.ProjectName, p.Path)
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to get draft path: %v", err))
		return
	}

	if err := m.drafts.SaveDraft(draftPath, []byte(p.Content)); err != nil {
		client.sendError(fmt.Sprintf("Failed to save draft: %v", err))
		return
	}

	client.sendResponse(WriteDraft, map[string]string{
		"status": "ok",
	})
}

func (m *Manager) handleSaveFile(client *wsClient, payload json.RawMessage) {
	var p SaveFilePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for SAVE_FILE")
		return
	}

	if err := m.storage.WriteFile(p.Namespace, p.ProjectName, p.Path, p.Content); err != nil {
		client.sendError(fmt.Sprintf("Failed to save file: %v", err))
		return
	}

	client.sendResponse(SaveAck, map[string]string{
		"path":   p.Path,
		"status": "ok",
	})
}
