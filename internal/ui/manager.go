package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/shoka/mcp-server/internal/drafts"
	"github.com/shoka/mcp-server/internal/identity"
	"github.com/shoka/mcp-server/internal/notify"
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/storage/oauthstore"
)

type MessageType string

const (
	GetProjects MessageType = "GET_PROJECTS"
	GetTree     MessageType = "GET_TREE"
	ReadFile    MessageType = "READ_FILE"
	WriteDraft  MessageType = "WRITE_DRAFT"
	SaveFile    MessageType = "SAVE_FILE"
	SaveAck     MessageType = "SAVE_ACK"
	// MsgConflict reports an optimistic-concurrency conflict on SAVE_FILE: the
	// caller's if_match did not match the file's current etag. Distinct from the
	// generic Error frame so a client can branch on "this is a conflict" (it
	// carries the current etag) without parsing a free-form error string.
	MsgConflict      MessageType = "CONFLICT"
	MsgCreateProject MessageType = "CREATE_PROJECT"
	// MsgSearchFiles requests a project-scoped full-text/filename search and
	// MsgSearchResult carries the matches back. Search is read-only and
	// project-scoped: it wires the existing storage.SearchFiles capability (the
	// same one the MCP search_files tool uses) to the /ws/ui request/response
	// dispatch, mirroring READ_FILE. There is deliberately no cross-project
	// variant — the storage layer searches one project at a time.
	MsgSearchFiles  MessageType = "SEARCH_FILES"
	MsgSearchResult MessageType = "SEARCH_RESULT"
	// MsgMoveFile renames/moves a file within a project; MsgMoveAck carries the
	// result back (new etag + an always-0 links_rewritten count). A move is a
	// pure path change: one atomic, history-preserving rename (git log --follow
	// keeps working) that rewrites no inbound links. Inbound-link rewriting was
	// decoupled and disabled in B-33; the goldmark rewriter is retained dormant
	// pending a future reverse-link index (B-33/B-34), so links_rewritten is
	// always 0 today. A stale if_match — or an existing target with no if_match —
	// yields the same CONFLICT frame SAVE_FILE uses, carrying the relevant file's
	// current etag.
	MsgMoveFile MessageType = "MOVE_FILE"
	MsgMoveAck  MessageType = "MOVE_ACK"
	// MsgOAuthList enumerates the live OAuth/MCP connections (token series) the
	// built-in authorization server holds, and MsgOAuthList carries the summaries
	// back; MsgOAuthRevoke revokes one connection by series id and acks. This is
	// the operator-facing management surface over the (b) oauthstore's List/Revoke
	// (the 2026-06-03 MCP OAuth (c) directive). NO SECRETS cross the wire: the
	// response carries oauthstore.SeriesInfo only (client identity, principal,
	// times, series id) — never an access/refresh token, code, or PKCE value.
	//
	// Both requests are ADMINISTRATOR-ONLY, gated server-side (the authoritative
	// gate): a non-admin caller receives MsgOAuthDenied, not data — hiding the UI
	// is not sufficient. They are also refused (MsgOAuthDenied) when OAuth is not
	// enabled (no store). See AdminAuthorizer and the §2.1a seam below.
	MsgOAuthList   MessageType = "OAUTH_LIST"
	MsgOAuthRevoke MessageType = "OAUTH_REVOKE"
	// MsgOAuthIssueSelf mints a fresh access token for the current-mode operator
	// (the "token to self" path, B-46b §2.2) and returns it ONCE in the response.
	// This is the single deliberate exception to "no secret crosses /ws/ui": the
	// operator copies the displayed token into their CLI client config. It is
	// admin-gated (the same adminGate as List/Revoke) and the token is never logged
	// or persisted anywhere on the server beyond the normal token store.
	MsgOAuthIssueSelf MessageType = "OAUTH_ISSUE_SELF"
	// MsgOAuthDenied is the typed refusal frame for the admin-only OAuth requests:
	// reason "forbidden" (the caller is not an administrator) or "oauth_disabled"
	// (OAuth is off, so there is no connection store). It is distinct from the
	// generic ERROR frame so the client can recognise an authorization refusal
	// (and hide the management surface) rather than treat it as a transport error.
	MsgOAuthDenied MessageType = "OAUTH_DENIED"
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
	// IfMatch, when non-empty, enables optimistic concurrency: the write succeeds
	// only if the file's current etag equals it, otherwise a CONFLICT frame is
	// returned. Omitted by callers that have not adopted versioning — those writes
	// take the unchecked path, preserving the pre-versioning behaviour.
	IfMatch string `json:"if_match,omitempty"`
}

// ConflictPayload is the CONFLICT frame's body: the path that conflicted and the
// file's current etag, so the client can re-base its edit (e.g. show the
// four-button conflict UX) without parsing the error message.
type ConflictPayload struct {
	Path        string `json:"path"`
	CurrentETag string `json:"current_etag"`
	Message     string `json:"message"`
}

// SearchFilesPayload is the SEARCH_FILES request body. SearchIn is optional and
// defaults to "both" (filename + content) in the storage layer; it mirrors the
// MCP search_files tool's input minus the tool-only validation.
type SearchFilesPayload struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
	Query       string `json:"query"`
	SearchIn    string `json:"search_in,omitempty"`
}

// MoveFilePayload is the MOVE_FILE request body. IfMatch is optional and carries
// the same dual semantic as the storage layer: it validates the target's etag
// when the target exists (explicit overwrite), otherwise the source's etag.
type MoveFilePayload struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
	SourcePath  string `json:"source_path"`
	TargetPath  string `json:"target_path"`
	IfMatch     string `json:"if_match,omitempty"`
}

// MoveAckPayload is the MOVE_ACK frame's body: the source and target paths, the
// destination's new etag (usable as if_match for a follow-up edit), and
// LinksRewritten. A move is a pure path change, so LinksRewritten is currently
// always 0; the field is reserved for the future reverse-link-index
// re-enablement (B-33/B-34) and is kept in the shape deliberately, not removed.
type MoveAckPayload struct {
	SourcePath     string `json:"source_path"`
	TargetPath     string `json:"target_path"`
	NewETag        string `json:"new_etag"`
	LinksRewritten int    `json:"links_rewritten"`
}

// OAuthConnectionInfo is one live OAuth/MCP connection in the OAUTH_LIST
// response — the no-secret view of an oauthstore.SeriesInfo. It carries the
// connecting client's identity (its CIMD metadata URL, the Claude side — shown
// to tell connections apart), the bound principal, the issued/expiry times, and
// the series id (the revoke target, plus a short prefix for display). It NEVER
// carries an access/refresh token, authorization code, or PKCE value — those
// live only in the store's SeriesRecord/CodeRecord and never reach List.
type OAuthConnectionInfo struct {
	// SeriesID is the full opaque series identifier — the OAUTH_REVOKE target. It
	// is NOT a bearer credential (it cannot authenticate anything; only access
	// tokens can), so exposing it to an admin client is safe.
	SeriesID      string `json:"series_id"`
	SeriesIDShort string `json:"series_id_short"`
	// ClientID is the connecting client's CIMD metadata URL (its identity). Shown
	// at runtime only; no concrete value is ever written into source/tests.
	ClientID       string    `json:"client_id"`
	PrincipalName  string    `json:"principal_name"`
	PrincipalEmail string    `json:"principal_email"`
	IssuedAt       time.Time `json:"issued_at"`
	AccessExpiry   time.Time `json:"access_expiry"`
}

// OAuthListPayload is the OAUTH_LIST response body: the live connections. The
// slice is always non-nil so the client receives [] rather than null on zero
// connections (the empty-state case).
type OAuthListPayload struct {
	Connections []OAuthConnectionInfo `json:"connections"`
}

// OAuthRevokeRequest is the OAUTH_REVOKE request body: the series id to revoke.
type OAuthRevokeRequest struct {
	SeriesID string `json:"series_id"`
}

// OAuthRevokePayload is the OAUTH_REVOKE ack: the series id that was revoked.
type OAuthRevokePayload struct {
	SeriesID string `json:"series_id"`
	Status   string `json:"status"`
}

// OAuthDeniedPayload is the OAUTH_DENIED frame's body. Reason is "forbidden"
// (caller is not an administrator) or "oauth_disabled" (OAuth is off).
type OAuthDeniedPayload struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// OAuthIssueSelfPayload is the OAUTH_ISSUE_SELF response body: the freshly minted
// access token (display-once — the operator copies it into their CLI config) and
// its expiry, so the UI can warn how long it lasts. The token is the one secret
// that crosses /ws/ui; it is never logged or stored beyond the token store.
type OAuthIssueSelfPayload struct {
	AccessToken  string    `json:"access_token"`
	AccessExpiry time.Time `json:"access_expiry"`
}

// SearchResultPayload is the SEARCH_RESULT frame's body: the matches, each a
// {path, snippet}. The slice is always non-nil so the client receives [] rather
// than null on a no-match query.
type SearchResultPayload struct {
	Matches []storage.SearchMatch `json:"matches"`
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
//
// id is this connection's sender identity (the 2026-06-01 sender-exclusion
// directive): the connection subscribes to the notify center under it, and its
// own writes carry it (via notify.WithSender on the write context) so the center
// does not echo the write back to this connection. It is unique per connection
// for the life of the process ("ws-<seq>").
type wsClient struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	id      string
	// req is the upgraded connection's HTTP request, retained so the admin-gated
	// OAUTH_ISSUE_SELF handler can derive the RFC 8707 resource (forwarded-header
	// aware) the same way /authorize does. It is read-only after the upgrade.
	req *http.Request
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

// OAuthConnectionStore is the narrow capability the OAuth management requests
// (OAUTH_LIST/OAUTH_REVOKE) depend on — exactly the (b) oauthstore's no-secret
// List and per-series Revoke. The Manager depends on this interface, not the
// concrete *oauthstore.Store, so the handle stays nil when OAuth is disabled and
// tests can inject a fake. NO store change is implied: *oauthstore.Store already
// satisfies it.
type OAuthConnectionStore interface {
	List() ([]oauthstore.SeriesInfo, error)
	Revoke(seriesID string) error
}

// OAuthSelfIssuer mints a fresh access token bound to the current-mode operator
// (the "token to self" path, B-46b §2.2). It is a SEPARATE capability from
// OAuthConnectionStore so the manager stays free of oauth/serverurl/identity
// wiring: the concrete issuer is built in cmd/shoka (it holds the store, the
// operator principal, the TTLs, and the resource deriver) and injected via
// SetOAuthSelfIssuer. The request is passed so the issuer can derive the RFC 8707
// resource exactly as /authorize does (forwarded-header aware). It returns the
// access token and its expiry; the manager never sees how it is minted. nil when
// OAuth is disabled.
type OAuthSelfIssuer interface {
	IssueSelf(r *http.Request) (accessToken string, accessExpiry time.Time, err error)
}

// OAuthSelfIssuerFunc adapts a function to OAuthSelfIssuer.
type OAuthSelfIssuerFunc func(r *http.Request) (string, time.Time, error)

// IssueSelf calls f.
func (f OAuthSelfIssuerFunc) IssueSelf(r *http.Request) (string, time.Time, error) { return f(r) }

// AdminAuthorizer is the administrator-only authorization seam for the OAuth
// management requests (the 2026-06-03 MCP OAuth (c) directive §2.1a). It gates
// OAUTH_LIST/OAUTH_REVOKE on the SERVER side — the authoritative gate — because
// those requests can cut off other users' MCP connections, a privileged
// operation (unlike file move/list, which any project user may do). Hiding the
// UI is not enough: the request itself must refuse a non-admin caller.
//
// B-28 ATTACH POINT: Shoka has no admin/role concept today, and /ws/ui carries
// NO authenticated identity (a connection has only a "ws-<seq>" sender id, not a
// user). So the single-user implementation (singleUserAdmin) returns true — the
// sole operator IS the administrator, and the screen works normally today. When
// the Web-auth / multi-user leg (B-28) lands, the connection will carry a real
// identity; a real admin-role check drops into this seam additively, and the
// method's signature widens to take that identity. This is the seam, NOT an RBAC
// system — do not build roles here.
type AdminAuthorizer interface {
	IsAdmin() bool
}

// singleUserAdmin is the default AdminAuthorizer for single-user mode: the sole
// user is the administrator, so it is trivially true. Replaced via
// SetAdminAuthorizer when a real role check exists (B-28).
type singleUserAdmin struct{}

func (singleUserAdmin) IsAdmin() bool { return true }

type Manager struct {
	storage       storage.StorageService
	drafts        *drafts.Manager
	notify        *notify.Center
	originChecker func(*http.Request) bool
	upgrader      websocket.Upgrader
	notifyDrops   atomic.Int64
	// oauth is the OAuth connection store for the admin management requests. It is
	// nil when OAuth is disabled (set via SetOAuthStore inside the oauth-enabled
	// wiring), in which case OAUTH_LIST/OAUTH_REVOKE return MsgOAuthDenied with
	// reason "oauth_disabled" rather than nil-panicking.
	oauth OAuthConnectionStore
	// selfIssuer mints the operator's "token to self" (B-46b §2.2). nil when OAuth
	// is disabled (wired via SetOAuthSelfIssuer in the oauth-enabled startup path),
	// in which case OAUTH_ISSUE_SELF returns MsgOAuthDenied ("oauth_disabled").
	selfIssuer OAuthSelfIssuer
	// admin gates the administrator-only OAuth requests (§2.1a). Never nil:
	// NewManager defaults it to singleUserAdmin (trivially true).
	admin AdminAuthorizer
	// connSeq assigns each connection a unique sender id ("ws-<seq>"). Atomic so
	// concurrent upgrades never collide on an id.
	connSeq atomic.Uint64
}

// NewManager builds the /ws/ui manager. notifyCenter may be nil (e.g. in tests);
// when nil, no NOTIFY events are pushed but every other message works unchanged.
func NewManager(s storage.StorageService, d *drafts.Manager, notifyCenter *notify.Center) *Manager {
	m := &Manager{
		storage: s,
		drafts:  d,
		notify:  notifyCenter,
		admin:   singleUserAdmin{},
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

// SetOAuthStore wires the OAuth connection store for the admin management
// requests. Called only in the oauth-enabled startup path; when unset the store
// is nil and OAUTH_LIST/OAUTH_REVOKE return MsgOAuthDenied ("oauth_disabled").
func (m *Manager) SetOAuthStore(s OAuthConnectionStore) {
	m.oauth = s
}

// SetOAuthSelfIssuer wires the token-to-self minter for OAUTH_ISSUE_SELF. Called
// only in the oauth-enabled startup path; when unset the issuer is nil and
// OAUTH_ISSUE_SELF returns MsgOAuthDenied ("oauth_disabled").
func (m *Manager) SetOAuthSelfIssuer(i OAuthSelfIssuer) {
	m.selfIssuer = i
}

// SetAdminAuthorizer overrides the administrator-only authorization seam for the
// OAuth management requests (§2.1a). The default is singleUserAdmin (trivially
// true). This is the B-28 attach point: a real admin-role check replaces the
// default once /ws/ui carries an authenticated identity.
func (m *Manager) SetAdminAuthorizer(a AdminAuthorizer) {
	if a != nil {
		m.admin = a
	}
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
	client := &wsClient{conn: conn, id: fmt.Sprintf("ws-%d", m.connSeq.Add(1)), req: r}

	// Subscribe to the notification center and forward events to this browser as
	// NOTIFY messages. The callback is non-blocking: it pushes onto a bounded
	// buffer and drops on full so a slow client cannot stall the publisher
	// (directive §4.2 / §5.3). A nil center yields a no-op subscription.
	//
	// SubscribeAs ties the subscription to this connection's sender id so the
	// center excludes this connection from events it originated (2026-06-01
	// sender-exclusion directive): a write made on this connection is not echoed
	// back to it as if a second actor had made it.
	events := make(chan notify.Event, 64)
	unsubscribe := m.notify.SubscribeAs(client.id, func(e notify.Event) {
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
		case MsgSearchFiles:
			m.handleSearchFiles(client, wsMsg.Payload)
		case MsgMoveFile:
			m.handleMoveFile(client, wsMsg.Payload)
		case MsgOAuthList:
			m.handleOAuthList(client)
		case MsgOAuthRevoke:
			m.handleOAuthRevoke(client, wsMsg.Payload)
		case MsgOAuthIssueSelf:
			m.handleOAuthIssueSelf(client)
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

	// Sender identity: this connection originated the create, so the resulting
	// project.create NOTIFY must not be echoed back to it (2026-06-01 directive).
	ctx := notify.WithSender(context.Background(), client.id)
	if err := m.storage.CreateProjectCtx(ctx, p.Namespace, p.ProjectName); err != nil {
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

	content, etag, err := m.storage.ReadFileWithETag(p.Namespace, p.ProjectName, p.Path)
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to read file: %v", err))
		return
	}

	// etag travels with the content so the client can send it back as if_match on
	// a subsequent SAVE_FILE. Clients that ignore the field are unaffected.
	client.sendResponse(ReadFile, map[string]string{
		"path":    p.Path,
		"content": content,
		"etag":    etag,
	})
}

// handleSearchFiles runs a project-scoped search via the shared
// storage.SearchFiles capability and returns the matches. It is read-only — no
// commit, no NOTIFY — so, like handleReadFile, it carries no identity or sender
// context. A nil result is normalised to an empty slice so the wire shape is
// always {"matches": [...]}.
func (m *Manager) handleSearchFiles(client *wsClient, payload json.RawMessage) {
	var p SearchFilesPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for SEARCH_FILES")
		return
	}

	matches, err := m.storage.SearchFiles(p.Namespace, p.ProjectName, p.Query, p.SearchIn)
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to search files: %v", err))
		return
	}
	if matches == nil {
		matches = []storage.SearchMatch{}
	}

	client.sendResponse(MsgSearchResult, SearchResultPayload{Matches: matches})
}

// handleMoveFile renames/moves a file via storage.Move. Like SAVE_FILE it is the
// operator acting as themselves (identity.WithUser → operator is the commit
// Author) and carries the connection's sender id so the resulting file.move
// NOTIFY is not echoed back to this connection. A conflict (stale if_match, or an
// existing target with no if_match) returns the same CONFLICT frame SAVE_FILE
// uses; success returns MOVE_ACK with the new etag and an always-0
// links_rewritten count (a move is a pure path change; inbound-link rewriting is
// disabled and the rewriter retained dormant per B-33).
func (m *Manager) handleMoveFile(client *wsClient, payload json.RawMessage) {
	var p MoveFilePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for MOVE_FILE")
		return
	}

	ctx := identity.WithUser(context.Background(), identity.User{})
	ctx = notify.WithSender(ctx, client.id)

	var ifMatch *string
	if p.IfMatch != "" {
		ifMatch = &p.IfMatch
	}

	newEtag, links, err := m.storage.Move(ctx, "", p.Namespace, p.ProjectName, p.SourcePath, p.TargetPath, ifMatch)
	if err != nil {
		var conflict *storage.VersionConflictError
		if errors.As(err, &conflict) {
			client.sendResponse(MsgConflict, ConflictPayload{
				Path:        p.TargetPath,
				CurrentETag: conflict.Current,
				Message:     "Move rejected: the file was modified or the target already exists",
			})
			return
		}
		client.sendError(fmt.Sprintf("Failed to move file: %v", err))
		return
	}

	client.sendResponse(MsgMoveAck, MoveAckPayload{
		SourcePath:     p.SourcePath,
		TargetPath:     p.TargetPath,
		NewETag:        newEtag,
		LinksRewritten: links,
	})
}

// adminGate enforces the §2.1a administrator-only authorization on the OAuth
// management requests and that OAuth is enabled at all. It is the AUTHORITATIVE
// server-side gate: it returns false and sends an OAUTH_DENIED frame when the
// caller is not an administrator (reason "forbidden") or when OAuth is disabled
// so there is no store (reason "oauth_disabled"). Authorization is checked
// before capability, so a non-admin never learns whether OAuth is enabled. A
// true return guarantees m.oauth is non-nil, so handlers never nil-panic.
func (m *Manager) adminGate(client *wsClient) bool {
	if !m.admin.IsAdmin() {
		client.sendResponse(MsgOAuthDenied, OAuthDeniedPayload{
			Reason:  "forbidden",
			Message: "OAuth connection management is administrator-only",
		})
		return false
	}
	if m.oauth == nil {
		client.sendResponse(MsgOAuthDenied, OAuthDeniedPayload{
			Reason:  "oauth_disabled",
			Message: "OAuth is not enabled on this server",
		})
		return false
	}
	return true
}

// handleOAuthList returns the live OAuth/MCP connections as no-secret summaries
// (oauthstore.SeriesInfo). Administrator-only (adminGate). Read-only — no commit,
// no NOTIFY — so, like handleSearchFiles, it carries no identity or sender
// context. The Connections slice is always non-nil so the wire shape is always
// {"connections": [...]} (the empty-state client renders [] as "no connections").
func (m *Manager) handleOAuthList(client *wsClient) {
	if !m.adminGate(client) {
		return
	}
	infos, err := m.oauth.List()
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to list OAuth connections: %v", err))
		return
	}
	conns := make([]OAuthConnectionInfo, 0, len(infos))
	for _, s := range infos {
		conns = append(conns, toOAuthConnectionInfo(s))
	}
	client.sendResponse(MsgOAuthList, OAuthListPayload{Connections: conns})
}

// handleOAuthRevoke revokes one connection by series id (oauthstore.Revoke).
// Administrator-only (adminGate). Revoking one series leaves every other intact
// (the store guarantees it). An absent series_id is a typed error rather than a
// silent no-op; a well-formed but already-revoked id succeeds idempotently (the
// store's Revoke is idempotent — the right behaviour when two admins race or the
// row is already gone).
func (m *Manager) handleOAuthRevoke(client *wsClient, payload json.RawMessage) {
	if !m.adminGate(client) {
		return
	}
	var p OAuthRevokeRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for OAUTH_REVOKE")
		return
	}
	if p.SeriesID == "" {
		client.sendError("OAUTH_REVOKE requires a series_id")
		return
	}
	if err := m.oauth.Revoke(p.SeriesID); err != nil {
		client.sendError(fmt.Sprintf("Failed to revoke OAuth connection: %v", err))
		return
	}
	client.sendResponse(MsgOAuthRevoke, OAuthRevokePayload{SeriesID: p.SeriesID, Status: "ok"})
}

// handleOAuthIssueSelf mints a fresh access token bound to the current-mode
// operator (the "token to self" path, B-46b §2.2) and returns it ONCE. It is the
// only place a secret token crosses /ws/ui — a deliberate, admin-gated exception
// so the operator can paste the token into their CLI client config. The token is
// NOT logged (no log statement carries it) and is persisted only in the normal
// token store. Administrator-only via the same adminGate as List/Revoke; the
// issuer being nil (OAuth disabled) is reported as oauth_disabled.
func (m *Manager) handleOAuthIssueSelf(client *wsClient) {
	if !m.adminGate(client) {
		return
	}
	if m.selfIssuer == nil {
		client.sendResponse(MsgOAuthDenied, OAuthDeniedPayload{
			Reason:  "oauth_disabled",
			Message: "token issuance is not available on this server",
		})
		return
	}
	token, expiry, err := m.selfIssuer.IssueSelf(client.req)
	if err != nil {
		// The error is generic on the wire; the token never appears in it.
		client.sendError("Failed to issue a token")
		log.Printf("OAUTH_ISSUE_SELF mint failed: %v", err)
		return
	}
	client.sendResponse(MsgOAuthIssueSelf, OAuthIssueSelfPayload{
		AccessToken:  token,
		AccessExpiry: expiry,
	})
}

// toOAuthConnectionInfo maps a store SeriesInfo to the no-secret wire DTO,
// computing a short series-id prefix for display. The full series id rides along
// as the revoke target (it is not a bearer credential).
func toOAuthConnectionInfo(s oauthstore.SeriesInfo) OAuthConnectionInfo {
	short := s.SeriesID
	if len(short) > 8 {
		short = short[:8]
	}
	return OAuthConnectionInfo{
		SeriesID:       s.SeriesID,
		SeriesIDShort:  short,
		ClientID:       s.ClientID,
		PrincipalName:  s.Principal.Name,
		PrincipalEmail: s.Principal.Email,
		IssuedAt:       s.IssuedAt,
		AccessExpiry:   s.AccessExpiry,
	}
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

	// A web save is the operator acting as themselves, not an agent: attribute the
	// commit's Author to the configured operator user (identity.WithUser). The
	// empty User keeps single-user mode; a future authenticated request substitutes
	// the actual user at this call site. The ctx-aware Write carries the identity
	// (the old WriteFile path used context.Background() and resolved to the default
	// agent).
	ctx := identity.WithUser(context.Background(), identity.User{})
	// Sender identity: this connection originated the write, so the resulting
	// file.write NOTIFY must not be echoed back to it (2026-06-01 directive).
	ctx = notify.WithSender(ctx, client.id)

	// if_match present → optimistic concurrency; absent → unchecked write (nil),
	// preserving the pre-versioning behaviour for callers that have not adopted it.
	var ifMatch *string
	if p.IfMatch != "" {
		ifMatch = &p.IfMatch
	}

	etag, err := m.storage.Write(ctx, "", p.Namespace, p.ProjectName, p.Path, p.Content, ifMatch)
	if err != nil {
		var conflict *storage.VersionConflictError
		if errors.As(err, &conflict) {
			client.sendResponse(MsgConflict, ConflictPayload{
				Path:        p.Path,
				CurrentETag: conflict.Current,
				Message:     "File was modified by someone else",
			})
			return
		}
		client.sendError(fmt.Sprintf("Failed to save file: %v", err))
		return
	}

	// Return the new etag so the client can use it as if_match for its next save
	// (the editor's read-modify-write loop).
	client.sendResponse(SaveAck, map[string]string{
		"path":   p.Path,
		"status": "ok",
		"etag":   etag,
	})
}
