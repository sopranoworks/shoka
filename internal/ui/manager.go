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
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sopranoworks/shoka/internal/drafts"
	"github.com/sopranoworks/shoka/internal/identity"
	"github.com/sopranoworks/shoka/internal/ingest"
	"github.com/sopranoworks/shoka/internal/libstatus"
	"github.com/sopranoworks/shoka/internal/notify"
	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/pkg/authz"
	"github.com/sopranoworks/shoka/pkg/uiws"
)

// MessageType is the /ws/ui frame discriminator. The transport + auth/user/OAuth core
// message types live in pkg/uiws (uiws.MessageType); this alias lets the document
// message constants below — and the dispatch switch's document cases — stay unqualified
// while remaining the same type as the core constants (uiws.MsgAccountGet, …).
type MessageType = uiws.MessageType

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
	// B-28 ns/proj management part 1: the destructive + identity ops. Project
	// create/delete = admin on the target namespace; namespace create/delete =
	// super-user only (gated via authz.IsSuperUser in the gate, see wsSuperUserOps).
	MsgDeleteProject   MessageType = "DELETE_PROJECT"
	MsgCreateNamespace MessageType = "CREATE_NAMESPACE"
	MsgDeleteNamespace MessageType = "DELETE_NAMESPACE"
	// B-28 stage B: managed-namespace health (read, admin-filtered) + the recovery
	// actions (drop_missing / clean_orphaned / adopt).
	MsgNamespaceHealth  MessageType = "NAMESPACE_HEALTH"
	MsgNamespaceRecover MessageType = "NAMESPACE_RECOVER"
	// MsgMoveProject moves a project between namespaces (B-28 project move) — super-user only.
	MsgMoveProject MessageType = "MOVE_PROJECT"
	// B-28 ns/proj rename: RENAME_PROJECT renames a project within its namespace (admin on the
	// namespace — wsLevels), RENAME_NAMESPACE relabels a whole namespace (super-user —
	// wsSuperUserOps).
	MsgRenameProject   MessageType = "RENAME_PROJECT"
	MsgRenameNamespace MessageType = "RENAME_NAMESPACE"
	// MsgSearchFiles requests a project-scoped full-text/filename search and
	// MsgSearchResult carries the matches back. Search is read-only and
	// project-scoped, mirroring READ_FILE.
	MsgSearchFiles  MessageType = "SEARCH_FILES"
	MsgSearchResult MessageType = "SEARCH_RESULT"
	// MsgMoveFile renames/moves a file within a project; MsgMoveAck carries the
	// result back (new etag + an always-0 links_rewritten count).
	MsgMoveFile MessageType = "MOVE_FILE"
	MsgMoveAck  MessageType = "MOVE_ACK"
	// MsgDeleteFile removes a file; MsgDeleteAck carries the deleted path back. It
	// wires the EXISTING storage.Delete (git-tracked hard-remove, recoverable via
	// History). It carries an optional if_match: a stale etag yields the SAME
	// CONFLICT frame SAVE_FILE/MOVE_FILE use.
	MsgDeleteFile MessageType = "DELETE_FILE"
	MsgDeleteAck  MessageType = "DELETE_ACK"
	// MsgListDeleted lists a project's currently-deleted files (a cheap O(cap) read of
	// the per-project deleted-file log); MsgReviveFile re-creates one deleted file
	// forward-only; MsgReviveAck carries the revived path back. Both are ADMIN-ONLY.
	MsgListDeleted MessageType = "LIST_DELETED"
	MsgReviveFile  MessageType = "REVIVE_FILE"
	MsgReviveAck   MessageType = "REVIVE_ACK"
	// MsgRecoverProject is the in-product recovery for a project stuck in `corrupted`:
	// it re-syncs the write-path baseline to the ACTUAL on-disk git HEAD and clears a
	// FALSE corrupted flag. MsgRecoverAck carries the resulting state back. It wires
	// storage.ResyncToHead — NON-DESTRUCTIVE.
	MsgRecoverProject MessageType = "RECOVER_PROJECT"
	MsgRecoverAck     MessageType = "RECOVER_ACK"
	// MsgNotify carries one notify.Event pushed from the server to the browser (the
	// 2026-05-31 auto-refresh directive). It rides the same {type,payload} envelope.
	MsgNotify MessageType = "NOTIFY"
	// MsgLibrarianStatus returns the cached ask_the_librarian health snapshot
	// (B-73 config-and-validation); MsgRefreshLibrarianStatus re-runs the one-call
	// health-check and returns the fresh snapshot. Both carry a LibrarianStatus
	// payload and are admin-only (they expose config validity, never the API key).
	MsgLibrarianStatus        MessageType = "LIBRARIAN_STATUS"
	MsgRefreshLibrarianStatus MessageType = "REFRESH_LIBRARIAN_STATUS"
)

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
	// ContentEncoding selects how Content is interpreted, identical to the MCP
	// write_file tool: empty/"utf8" is literal text (the existing editor/create
	// behaviour, unchanged); "base64" decodes Content from base64 to raw bytes via
	// the shared ingest helper, enforcing the closed markdown/json/yaml allowlist.
	ContentEncoding string `json:"content_encoding,omitempty"`
}

// ConflictPayload is the CONFLICT frame's body: the path that conflicted and the
// file's current etag, so the client can re-base its edit without parsing the error.
type ConflictPayload struct {
	Path        string `json:"path"`
	CurrentETag string `json:"current_etag"`
	Message     string `json:"message"`
}

// SearchFilesPayload is the SEARCH_FILES request body. SearchIn is optional and
// defaults to "both" (filename + content) in the storage layer.
type SearchFilesPayload struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
	Query       string `json:"query"`
	SearchIn    string `json:"search_in,omitempty"`
}

// MoveFilePayload is the MOVE_FILE request body. IfMatch validates the target's etag
// when the target exists (explicit overwrite), otherwise the source's etag.
type MoveFilePayload struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
	SourcePath  string `json:"source_path"`
	TargetPath  string `json:"target_path"`
	IfMatch     string `json:"if_match,omitempty"`
}

// MoveAckPayload is the MOVE_ACK frame's body. LinksRewritten is currently always 0
// (a move is a pure path change); the field is reserved for the future reverse-link
// index re-enablement (B-33/B-34).
type MoveAckPayload struct {
	SourcePath     string `json:"source_path"`
	TargetPath     string `json:"target_path"`
	NewETag        string `json:"new_etag"`
	LinksRewritten int    `json:"links_rewritten"`
}

// DeleteFilePayload is the DELETE_FILE request body. IfMatch carries the same
// optimistic-concurrency semantic as SAVE_FILE.
type DeleteFilePayload struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
	Path        string `json:"path"`
	IfMatch     string `json:"if_match,omitempty"`
}

// DeleteAckPayload is the DELETE_ACK frame's body: the path that was deleted.
type DeleteAckPayload struct {
	Path string `json:"path"`
}

// RecoverProjectPayload is the RECOVER_PROJECT request body.
type RecoverProjectPayload struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
}

// RecoverAckPayload is the RECOVER_ACK frame's body: the project's resulting health
// after the re-sync.
type RecoverAckPayload struct {
	Namespace string `json:"namespace"`
	Project   string `json:"project"`
	State     string `json:"state"`
	Recovered bool   `json:"recovered"`
	Message   string `json:"message"`
}

// SearchResultPayload is the SEARCH_RESULT frame's body: the matches, each a
// {path, snippet}. The slice is always non-nil so the client receives [] on no match.
type SearchResultPayload struct {
	Matches []storage.SearchMatch `json:"matches"`
}

type FileNode struct {
	Name     string     `json:"name"`
	Path     string     `json:"path"`
	IsDir    bool       `json:"isDir"`
	Children []FileNode `json:"children,omitempty"`
}

// Administrator authorization for the OAUTH_*/ADMIN_* management requests is enforced by
// the single dispatch gate (Client.Gate over the merged level table; those ops are
// admin-level in uiws.CoreLevels) — there is no separate admin seam.

type Manager struct {
	// CoreHandlers is the embedded auth/user/OAuth slice (the 2026-06-21 extraction,
	// now living in pkg/uiws): it owns the user + OAuth stores and the ACCOUNT_*/
	// ADMIN_*/OAUTH_* handlers. Embedding a pointer (so the setters mutate the shared
	// holder) promotes every core method/field onto *Manager, so the dispatch switch,
	// the SetUserStore/SetOAuthStore wiring in cmd/shoka, and the tests reach them
	// exactly as before — Shoka's behaviour is unchanged. The holder needs NO
	// StorageService, so the slice is independently constructible by a second program.
	*uiws.CoreHandlers
	storage storage.StorageService
	drafts  *drafts.Manager
	notify  *notify.Center
	// librarianStatus holds the cached ask_the_librarian health (B-73); nil when
	// the librarian is not wired (e.g. the LLM is not configured). The handlers
	// treat nil as "unconfigured".
	librarianStatus *libstatus.Checker
	originChecker   func(*http.Request) bool
	upgrader        websocket.Upgrader
	notifyDrops     atomic.Int64
	// connSeq assigns each connection a unique sender id ("ws-<seq>"). Atomic so
	// concurrent upgrades never collide on an id.
	connSeq atomic.Uint64
	// levels / superOps are the merged /ws/ui authorization tables passed to
	// client.Gate on every message: uiws.CoreLevels (the auth/user/OAuth core rows)
	// unioned with the document rows (wsLevels), plus the super-user-only ops
	// (wsSuperUserOps). Built once in NewManager. The union is asserted byte-equal to
	// the pre-extraction single wsLevels table by the gate-equivalence test, so the
	// gating decision is unchanged.
	levels   map[MessageType]uiws.Op
	superOps map[MessageType]bool
}

// NewManager builds the /ws/ui manager. notifyCenter may be nil (e.g. in tests);
// when nil, no NOTIFY events are pushed but every other message works unchanged.
func NewManager(s storage.StorageService, d *drafts.Manager, notifyCenter *notify.Center) *Manager {
	m := &Manager{
		CoreHandlers: &uiws.CoreHandlers{},
		storage:      s,
		drafts:       d,
		notify:       notifyCenter,
	}
	// Merge the core rows (pkg/uiws) with the document rows into the single gate table,
	// exactly reproducing the pre-extraction wsLevels (see the equivalence test).
	m.levels = make(map[MessageType]uiws.Op, len(uiws.CoreLevels)+len(wsLevels))
	for k, v := range uiws.CoreLevels {
		m.levels[k] = v
	}
	for k, v := range wsLevels {
		m.levels[k] = v
	}
	m.superOps = wsSuperUserOps
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

// The OAuth/user store setters (SetOAuthStore/SetOAuthSelfIssuer/SetUserStore) live on
// the embedded *uiws.CoreHandlers and are promoted onto *Manager, so cmd/shoka and the
// tests call m.SetOAuthStore/… unchanged.

// NotifyDrops reports how many notify events were dropped because a client's
// send buffer was full (observability; used by tests).
func (m *Manager) NotifyDrops() int64 { return m.notifyDrops.Load() }

// userIdentity returns the owning-user identity for a web write. When the connection
// carries an authenticated session principal (B-28 stage 1), the logged-in user's
// email is the git Author (email = account = git author). Otherwise it returns the
// empty User, which identity.Resolve fills with the configured single operator — the
// pre-login behaviour, preserved so an empty user store is never locked out. It reads
// the connection's exported principal (the uiws.Client carries it; identity is the
// document/git concern and stays in internal/ui, never in pkg/uiws).
func userIdentity(c *uiws.Client) identity.User {
	if c.HasPrincipal() {
		p := c.Principal()
		return identity.User{Name: p.Name, Email: p.Email}
	}
	return identity.User{}
}

// wsLevels is the DOCUMENT half of the /ws/ui authorization table (the core half is
// uiws.CoreLevels; NewManager unions them). Reads need read; content mutations need
// write; project recovery and the ns/proj admin ops need admin. A message absent from
// the merged table fails CLOSED at admin (global) in the gate.
var wsLevels = map[MessageType]uiws.Op{
	GetProjects:    {Level: authz.LevelRead, Global: true}, // global: lists every namespace
	GetTree:        {Level: authz.LevelRead, Global: false},
	ReadFile:       {Level: authz.LevelRead, Global: false},
	MsgSearchFiles: {Level: authz.LevelRead, Global: false},
	MsgGetHistory:  {Level: authz.LevelRead, Global: false},
	MsgGetFileAt:   {Level: authz.LevelRead, Global: false},
	MsgGetDiff:     {Level: authz.LevelRead, Global: false},

	WriteDraft:    {Level: authz.LevelWrite, Global: false},
	SaveFile:      {Level: authz.LevelWrite, Global: false},
	MsgMoveFile:   {Level: authz.LevelWrite, Global: false},
	MsgDeleteFile: {Level: authz.LevelWrite, Global: false},

	// Project create/delete = admin on the target namespace (B-28; create RAISED from
	// write). The namespace ops (CREATE/DELETE_NAMESPACE) are super-user only —
	// wsSuperUserOps, not this namespace-targeted table.
	MsgCreateProject: {Level: authz.LevelAdmin, Global: false},
	MsgDeleteProject: {Level: authz.LevelAdmin, Global: false},

	// RENAME_PROJECT = admin on the namespace (looser than MOVE_PROJECT, super-user).
	MsgRenameProject: {Level: authz.LevelAdmin, Global: false},

	MsgRecoverProject: {Level: authz.LevelAdmin, Global: false},

	// Deleted-file log ops: admin on the target namespace.
	MsgListDeleted: {Level: authz.LevelAdmin, Global: false},
	MsgReviveFile:  {Level: authz.LevelAdmin, Global: false},

	// Health read = admin-somewhere (global admin target; the handler filters to the
	// principal's admin namespaces). Recovery = admin on the target namespace (the
	// handler tightens whole-namespace actions to super-user).
	MsgNamespaceHealth:  {Level: authz.LevelAdmin, Global: true},
	MsgNamespaceRecover: {Level: authz.LevelAdmin, Global: false},

	// Librarian health is server-global config validity (admin-somewhere reads it;
	// the handler exposes no secret). Refresh makes one real API call, so it is
	// gated the same as the read.
	MsgLibrarianStatus:        {Level: authz.LevelAdmin, Global: true},
	MsgRefreshLibrarianStatus: {Level: authz.LevelAdmin, Global: true},
}

// wsSuperUserOps are the /ws/ui messages that require a SUPER-USER (wildcard admin), not
// merely admin-on-a-namespace — namespace create/delete/move/rename (B-28). They are
// gated via authz.IsSuperUser, checked FIRST, and are deliberately absent from the level
// tables. They are all document ops (the auth/user/OAuth core contributes none).
var wsSuperUserOps = map[MessageType]bool{
	MsgCreateNamespace: true,
	MsgDeleteNamespace: true,
	MsgMoveProject:     true,
	MsgRenameNamespace: true, // a namespace rename relabels the whole namespace + all its grants
}

func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := m.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade error: %v", err)
		return
	}
	defer conn.Close()
	// NewClient captures the WebUI session principal (B-28 stage 1) from the upgrade
	// request context (attached by authapi.Middleware) so web writes are authored as
	// the logged-in user and the gate can read the connection's scope.
	client := uiws.NewClient(conn, fmt.Sprintf("ws-%d", m.connSeq.Add(1)), r)

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
	unsubscribe := m.notify.SubscribeAs(client.ID, func(e notify.Event) {
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
			if err := client.WriteMessage(MsgNotify, e); err != nil {
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

		var wsMsg uiws.WSMessage
		if err := json.Unmarshal(message, &wsMsg); err != nil {
			client.SendError("Invalid message format")
			continue
		}

		// The single /ws/ui authorization gate (B-28 stage-2 flip): every message is
		// checked here, before its handler, through the shared authz.Authorize — not
		// scattered into the handlers. A refusal sends PERMISSION_DENIED and skips the
		// handler.
		if !client.Gate(wsMsg.Type, wsMsg.Payload, m.levels, m.superOps) {
			continue
		}

		// Core ops (ACCOUNT_*/ADMIN_*/OAUTH_*/DOMAIN_*/CLIENT_*) are owned by the
		// embedded CoreHandlers; Dispatch returns true when it handled the message.
		// Everything else is a document op handled by the switch below.
		if m.Dispatch(client, wsMsg.Type, wsMsg.Payload) {
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
		case MsgDeleteProject:
			m.handleDeleteProject(client, wsMsg.Payload)
		case MsgCreateNamespace:
			m.handleCreateNamespace(client, wsMsg.Payload)
		case MsgDeleteNamespace:
			m.handleDeleteNamespace(client, wsMsg.Payload)
		case MsgNamespaceHealth:
			m.handleNamespaceHealth(client)
		case MsgNamespaceRecover:
			m.handleNamespaceRecover(client, wsMsg.Payload)
		case MsgMoveProject:
			m.handleMoveProject(client, wsMsg.Payload)
		case MsgRenameProject:
			m.handleRenameProject(client, wsMsg.Payload)
		case MsgRenameNamespace:
			m.handleRenameNamespace(client, wsMsg.Payload)
		case MsgSearchFiles:
			m.handleSearchFiles(client, wsMsg.Payload)
		case MsgGetHistory:
			m.handleGetHistory(client, wsMsg.Payload)
		case MsgGetFileAt:
			m.handleGetFileAt(client, wsMsg.Payload)
		case MsgGetDiff:
			m.handleGetDiff(client, wsMsg.Payload)
		case MsgMoveFile:
			m.handleMoveFile(client, wsMsg.Payload)
		case MsgDeleteFile:
			m.handleDeleteFile(client, wsMsg.Payload)
		case MsgRecoverProject:
			m.handleRecoverProject(client, wsMsg.Payload)
		case MsgListDeleted:
			m.handleListDeleted(client, wsMsg.Payload)
		case MsgReviveFile:
			m.handleReviveFile(client, wsMsg.Payload)
		case MsgLibrarianStatus:
			m.handleLibrarianStatus(client)
		case MsgRefreshLibrarianStatus:
			m.handleRefreshLibrarianStatus(client)
		default:
			client.SendError("Unknown message type")
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

// projectRecoverer is the optional storage capability behind RECOVER_PROJECT:
// re-sync the write-path baseline to the on-disk git HEAD and return the resulting
// state. Type-asserted (like projectStateReader) so StorageService stays unwidened.
type projectRecoverer interface {
	ResyncToHead(namespace, projectName string) (storage.ProjectState, error)
}

// projectDeleter / namespaceManager are the optional storage capabilities behind the
// B-28 ns/proj-management destructive ops, type-asserted (like projectRecoverer) so
// StorageService stays unwidened.
type projectDeleter interface {
	DeleteProject(ctx context.Context, namespace, projectName string) error
}

type namespaceManager interface {
	CreateNamespace(namespace string) error
	DeleteNamespace(ctx context.Context, namespace string) error
}

// projectMover is the optional storage capability behind MOVE_PROJECT (B-28 project move).
type projectMover interface {
	MoveProject(ctx context.Context, oldNamespace, projectName, newNamespace string) error
}

// projectRenamer / namespaceRenamer are the optional storage capabilities behind
// RENAME_PROJECT / RENAME_NAMESPACE (B-28 ns/proj rename).
type projectRenamer interface {
	RenameProject(ctx context.Context, namespace, oldName, newName string) error
}

type namespaceRenamer interface {
	RenameNamespace(ctx context.Context, oldName, newName string) error
}

// namespaceHealthReader / namespaceRecoverer are the optional storage capabilities behind
// the B-28 stage-B health + recovery ops, type-asserted like the others.
type namespaceHealthReader interface {
	CheckAllHealth() storage.HealthReport
}

type namespaceRecoverer interface {
	DropMissingNamespace(namespace string) error
	DropMissingProject(namespace, projectName string) error
	CleanOrphanedSibling(namespace, name string) error
	AdoptForeign(namespace, projectName string) error
}

// handleGetProjects returns one entry per project across every namespace, each
// carrying its namespace, name, and health state. The payload's namespace field
// is ignored: the Web UI receives the full set and filters client-side (B-13 /
// B-22). The state badge and recovery dialog (storage redesign) read the same
// state field, unchanged.
func (m *Manager) handleGetProjects(client *uiws.Client, payload json.RawMessage) {
	namespaces, err := m.storage.ListNamespaces()
	if err != nil {
		client.SendError(fmt.Sprintf("Failed to list namespaces: %v", err))
		return
	}

	sr, _ := m.storage.(projectStateReader)
	infos := make([]ProjectInfo, 0)
	for _, ns := range namespaces {
		projects, err := m.storage.ListProjects(ns)
		if err != nil {
			client.SendError(fmt.Sprintf("Failed to list projects: %v", err))
			return
		}
		// Global-read filter (B-28 stage 3, the deferred stage-2 item): a logged-in
		// scoped user sees only the namespaces it has at least read on; a super-user
		// (or the no-principal no-lockout connection) sees all. Result-shaping after
		// the gate already authorized the global read.
		if !client.CanRead(ns) {
			continue
		}
		for _, name := range projects {
			state := string(storage.StateHealthy)
			if sr != nil {
				state = string(sr.State(ns, name))
			}
			infos = append(infos, ProjectInfo{Namespace: ns, Name: name, State: state})
		}
	}
	client.SendResponse(GetProjects, infos)
}

func (m *Manager) handleCreateProject(client *uiws.Client, payload json.RawMessage) {
	var p CreateProjectPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for CREATE_PROJECT")
		return
	}

	// Sender identity: this connection originated the create, so the resulting
	// project.create NOTIFY must not be echoed back to it (2026-06-01 directive).
	ctx := notify.WithSender(context.Background(), client.ID)
	if err := m.storage.CreateProjectCtx(ctx, p.Namespace, p.ProjectName); err != nil {
		client.SendError(fmt.Sprintf("Failed to create project: %v", err))
		return
	}

	client.SendResponse(MsgCreateProject, map[string]string{
		"status": "ok",
	})
}

// NamespacePayload carries just a namespace name (CREATE_NAMESPACE / DELETE_NAMESPACE).
type NamespacePayload struct {
	Namespace string `json:"namespace"`
}

func (m *Manager) handleDeleteProject(client *uiws.Client, payload json.RawMessage) {
	var p CreateProjectPayload // {namespace, projectName} — same shape as create
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for DELETE_PROJECT")
		return
	}
	pd, ok := m.storage.(projectDeleter)
	if !ok {
		client.SendError("project deletion is not available on this server")
		return
	}
	ctx := notify.WithSender(context.Background(), client.ID)
	if err := pd.DeleteProject(ctx, p.Namespace, p.ProjectName); err != nil {
		client.SendError(fmt.Sprintf("Failed to delete project: %v", err))
		return
	}
	client.SendResponse(MsgDeleteProject, map[string]string{"status": "ok"})
}

func (m *Manager) handleCreateNamespace(client *uiws.Client, payload json.RawMessage) {
	var p NamespacePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for CREATE_NAMESPACE")
		return
	}
	nm, ok := m.storage.(namespaceManager)
	if !ok {
		client.SendError("namespace management is not available on this server")
		return
	}
	if err := nm.CreateNamespace(p.Namespace); err != nil {
		client.SendError(fmt.Sprintf("Failed to create namespace: %v", err))
		return
	}
	client.SendResponse(MsgCreateNamespace, map[string]string{"status": "ok"})
}

func (m *Manager) handleDeleteNamespace(client *uiws.Client, payload json.RawMessage) {
	var p NamespacePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for DELETE_NAMESPACE")
		return
	}
	nm, ok := m.storage.(namespaceManager)
	if !ok {
		client.SendError("namespace management is not available on this server")
		return
	}
	ctx := notify.WithSender(context.Background(), client.ID)
	if err := nm.DeleteNamespace(ctx, p.Namespace); err != nil {
		client.SendError(fmt.Sprintf("Failed to delete namespace: %v", err))
		return
	}
	client.SendResponse(MsgDeleteNamespace, map[string]string{"status": "ok"})
}

// MoveProjectPayload is the /ws/ui project-move request (B-28). namespace is the SOURCE;
// newNamespace the TARGET (must pre-exist). super-user-gated (wsSuperUserOps).
type MoveProjectPayload struct {
	Namespace    string `json:"namespace"`
	ProjectName  string `json:"projectName"`
	NewNamespace string `json:"newNamespace"`
}

func (m *Manager) handleMoveProject(client *uiws.Client, payload json.RawMessage) {
	var p MoveProjectPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for MOVE_PROJECT")
		return
	}
	mv, ok := m.storage.(projectMover)
	if !ok {
		client.SendError("project move is not available on this server")
		return
	}
	ctx := notify.WithSender(context.Background(), client.ID)
	if err := mv.MoveProject(ctx, p.Namespace, p.ProjectName, p.NewNamespace); err != nil {
		client.SendError(fmt.Sprintf("Failed to move project: %v", err))
		return
	}
	client.SendResponse(MsgMoveProject, map[string]string{"status": "ok"})
}

// RenameProjectPayload is the /ws/ui project-rename request (B-28). namespace + projectName
// identify the project; newProjectName is the new name (must be free in the namespace).
// admin-on-namespace-gated (wsLevels). Keys match wsTarget (namespace/projectName).
type RenameProjectPayload struct {
	Namespace      string `json:"namespace"`
	ProjectName    string `json:"projectName"`
	NewProjectName string `json:"newProjectName"`
}

func (m *Manager) handleRenameProject(client *uiws.Client, payload json.RawMessage) {
	var p RenameProjectPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for RENAME_PROJECT")
		return
	}
	rn, ok := m.storage.(projectRenamer)
	if !ok {
		client.SendError("project rename is not available on this server")
		return
	}
	ctx := notify.WithSender(context.Background(), client.ID)
	if err := rn.RenameProject(ctx, p.Namespace, p.ProjectName, p.NewProjectName); err != nil {
		client.SendError(fmt.Sprintf("Failed to rename project: %v", err))
		return
	}
	client.SendResponse(MsgRenameProject, map[string]string{"status": "ok"})
}

// RenameNamespacePayload is the /ws/ui namespace-rename request (B-28). namespace is the
// current name; newNamespace the new name. super-user-gated (wsSuperUserOps).
type RenameNamespacePayload struct {
	Namespace    string `json:"namespace"`
	NewNamespace string `json:"newNamespace"`
}

func (m *Manager) handleRenameNamespace(client *uiws.Client, payload json.RawMessage) {
	var p RenameNamespacePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for RENAME_NAMESPACE")
		return
	}
	rn, ok := m.storage.(namespaceRenamer)
	if !ok {
		client.SendError("namespace rename is not available on this server")
		return
	}
	ctx := notify.WithSender(context.Background(), client.ID)
	if err := rn.RenameNamespace(ctx, p.Namespace, p.NewNamespace); err != nil {
		client.SendError(fmt.Sprintf("Failed to rename namespace: %v", err))
		return
	}
	client.SendResponse(MsgRenameNamespace, map[string]string{"status": "ok"})
}

// NamespaceRecoverPayload is the /ws/ui recovery request. ProjectName empty ⇒ a
// whole-namespace action (super-user only). Keys match wsTarget (namespace/projectName).
type NamespaceRecoverPayload struct {
	Action      string `json:"action"`
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
}

func (m *Manager) handleNamespaceHealth(client *uiws.Client) {
	hr, ok := m.storage.(namespaceHealthReader)
	if !ok {
		client.SendError("namespace health is not available on this server")
		return
	}
	// The gate authorized admin-somewhere; filter the picture to the principal's admin
	// namespaces (a super-user sees all, incl. base-level foreign namespaces).
	client.SendResponse(MsgNamespaceHealth, filterHealthByAdminScope(hr.CheckAllHealth(), client.Scope()))
}

// SetLibrarianStatus wires the cached ask_the_librarian health checker (B-73).
// Called once at startup from cmd/shoka; nil leaves the status "unconfigured".
func (m *Manager) SetLibrarianStatus(c *libstatus.Checker) { m.librarianStatus = c }

// handleLibrarianStatus returns the cached librarian health snapshot — a cheap
// read, NOT a fresh API call (the WebUI shows the last cached result on load).
func (m *Manager) handleLibrarianStatus(client *uiws.Client) {
	if m.librarianStatus == nil {
		client.SendResponse(MsgLibrarianStatus, libstatus.Snapshot{Kind: "unconfigured"})
		return
	}
	client.SendResponse(MsgLibrarianStatus, m.librarianStatus.Get())
}

// handleRefreshLibrarianStatus re-runs the one-call health-check on demand (the
// operator's manual refresh) and returns the fresh snapshot. One real (tiny) API
// call per click — never the API key.
func (m *Manager) handleRefreshLibrarianStatus(client *uiws.Client) {
	if m.librarianStatus == nil {
		client.SendResponse(MsgLibrarianStatus, libstatus.Snapshot{Kind: "unconfigured"})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client.SendResponse(MsgLibrarianStatus, m.librarianStatus.Refresh(ctx))
}

// filterHealthByAdminScope narrows a health report to what the principal may see: a
// super-user keeps everything; a namespace-admin keeps only the namespaces it administers
// and the base-level foreign-namespace listing is dropped (super-user-only view).
func filterHealthByAdminScope(report storage.HealthReport, scope string) storage.HealthReport {
	adminNs, superUser := authz.AdminNamespaces(scope)
	if superUser {
		return report
	}
	allow := make(map[string]bool, len(adminNs))
	for _, ns := range adminNs {
		allow[ns] = true
	}
	out := storage.HealthReport{}
	for _, nh := range report.Namespaces {
		if allow[nh.Name] {
			out.Namespaces = append(out.Namespaces, nh)
		}
	}
	return out
}

func (m *Manager) handleNamespaceRecover(client *uiws.Client, payload json.RawMessage) {
	var p NamespaceRecoverPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for NAMESPACE_RECOVER")
		return
	}
	nr, ok := m.storage.(namespaceRecoverer)
	if !ok {
		client.SendError("namespace recovery is not available on this server")
		return
	}
	if p.Namespace == "" {
		p.Namespace = "default"
	}
	nsLevel := p.ProjectName == "" // a whole-namespace action

	// Whole-namespace actions are super-user only: the dispatch gate authorized only
	// admin-on-the-namespace (which a namespace-admin satisfies for its own namespace), so
	// tighten here — mirroring the MCP handler / the create/delete-namespace ops.
	denyNamespaceLevel := func() bool {
		if nsLevel && !authz.IsSuperUser(client.Scope()) {
			client.SendResponse(uiws.MsgPermissionDenied, uiws.PermissionDeniedPayload{
				Op:       string(MsgNamespaceRecover),
				Required: "super-user",
				Message:  "permission denied: a whole-namespace recovery action requires a super-user",
			})
			return true
		}
		return false
	}

	var err error
	switch p.Action {
	case "drop_missing":
		if denyNamespaceLevel() {
			return
		}
		if nsLevel {
			err = nr.DropMissingNamespace(p.Namespace)
		} else {
			err = nr.DropMissingProject(p.Namespace, p.ProjectName)
		}
	case "clean_orphaned":
		if nsLevel {
			client.SendError("clean_orphaned requires projectName (the stray's base name)")
			return
		}
		err = nr.CleanOrphanedSibling(p.Namespace, p.ProjectName)
	case "adopt":
		if denyNamespaceLevel() {
			return
		}
		err = nr.AdoptForeign(p.Namespace, p.ProjectName)
	default:
		client.SendError("invalid action: must be drop_missing | clean_orphaned | adopt")
		return
	}
	if err != nil {
		client.SendError(fmt.Sprintf("namespace recovery failed: %v", err))
		return
	}
	client.SendResponse(MsgNamespaceRecover, map[string]string{"status": "ok"})
}

func (m *Manager) handleGetTree(client *uiws.Client, payload json.RawMessage) {
	var p GetTreePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for GET_TREE")
		return
	}

	tree, err := m.getTree(p.Namespace, p.ProjectName, "")
	if err != nil {
		client.SendError(fmt.Sprintf("Failed to get tree: %v", err))
		return
	}

	client.SendResponse(GetTree, tree)
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

func (m *Manager) handleReadFile(client *uiws.Client, payload json.RawMessage) {
	var p ReadFilePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for READ_FILE")
		return
	}

	content, etag, err := m.storage.ReadFileWithETag(p.Namespace, p.ProjectName, p.Path)
	if err != nil {
		client.SendError(fmt.Sprintf("Failed to read file: %v", err))
		return
	}

	// etag travels with the content so the client can send it back as if_match on
	// a subsequent SAVE_FILE. Clients that ignore the field are unaffected.
	client.SendResponse(ReadFile, map[string]string{
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
func (m *Manager) handleSearchFiles(client *uiws.Client, payload json.RawMessage) {
	var p SearchFilesPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for SEARCH_FILES")
		return
	}

	matches, err := m.storage.SearchFiles(p.Namespace, p.ProjectName, p.Query, p.SearchIn)
	if err != nil {
		client.SendError(fmt.Sprintf("Failed to search files: %v", err))
		return
	}
	if matches == nil {
		matches = []storage.SearchMatch{}
	}

	client.SendResponse(MsgSearchResult, SearchResultPayload{Matches: matches})
}

// handleMoveFile renames/moves a file via storage.Move. Like SAVE_FILE it is the
// operator acting as themselves (identity.WithUser → operator is the commit
// Author) and carries the connection's sender id so the resulting file.move
// NOTIFY is not echoed back to this connection. A conflict (stale if_match, or an
// existing target with no if_match) returns the same CONFLICT frame SAVE_FILE
// uses; success returns MOVE_ACK with the new etag and an always-0
// links_rewritten count (a move is a pure path change; inbound-link rewriting is
// disabled and the rewriter retained dormant per B-33).
func (m *Manager) handleMoveFile(client *uiws.Client, payload json.RawMessage) {
	var p MoveFilePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for MOVE_FILE")
		return
	}

	ctx := identity.WithUser(context.Background(), userIdentity(client))
	ctx = notify.WithSender(ctx, client.ID)

	var ifMatch *string
	if p.IfMatch != "" {
		ifMatch = &p.IfMatch
	}

	newEtag, links, err := m.storage.Move(ctx, "", p.Namespace, p.ProjectName, p.SourcePath, p.TargetPath, ifMatch)
	if err != nil {
		var conflict *storage.VersionConflictError
		if errors.As(err, &conflict) {
			client.SendResponse(MsgConflict, ConflictPayload{
				Path:        p.TargetPath,
				CurrentETag: conflict.Current,
				Message:     "Move rejected: the file was modified or the target already exists",
			})
			return
		}
		client.SendError(fmt.Sprintf("Failed to move file: %v", err))
		return
	}

	client.SendResponse(MsgMoveAck, MoveAckPayload{
		SourcePath:     p.SourcePath,
		TargetPath:     p.TargetPath,
		NewETag:        newEtag,
		LinksRewritten: links,
	})
}

// handleDeleteFile removes a file via the existing storage.Delete, mirroring
// handleMoveFile: like SAVE_FILE/MOVE_FILE it is the operator acting as themselves
// (identity.WithUser → operator is the commit Author) and carries the connection's
// sender id so the resulting file.delete NOTIFY is not echoed back to this
// connection. A stale if_match returns the SAME CONFLICT frame SAVE_FILE/MOVE_FILE
// use (the file changed during the client-side grace), so a mid-grace edit is
// surfaced rather than silently destroyed; success returns DELETE_ACK with the
// deleted path. No storage or tool change — the delete is git-tracked and
// recoverable via History.
func (m *Manager) handleDeleteFile(client *uiws.Client, payload json.RawMessage) {
	var p DeleteFilePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for DELETE_FILE")
		return
	}

	ctx := identity.WithUser(context.Background(), userIdentity(client))
	ctx = notify.WithSender(ctx, client.ID)

	var ifMatch *string
	if p.IfMatch != "" {
		ifMatch = &p.IfMatch
	}

	if err := m.storage.Delete(ctx, "", p.Namespace, p.ProjectName, p.Path, ifMatch); err != nil {
		var conflict *storage.VersionConflictError
		if errors.As(err, &conflict) {
			client.SendResponse(MsgConflict, ConflictPayload{
				Path:        p.Path,
				CurrentETag: conflict.Current,
				Message:     "Delete rejected: the file was modified after it was queued",
			})
			return
		}
		client.SendError(fmt.Sprintf("Failed to delete file: %v", err))
		return
	}

	client.SendResponse(MsgDeleteAck, DeleteAckPayload{Path: p.Path})
}

// handleRecoverProject re-syncs a project's write-path baseline to the actual
// on-disk git HEAD (storage.ResyncToHead) and reports the resulting state. It is the
// Web UI half of the stale-HEAD recovery: a clean-on-disk project stranded in a
// false `corrupted` is restored to healthy and writes re-enable; a project with
// genuine uncommitted drift stays corrupted and the ack says so. Non-destructive —
// no commit, no discard.
func (m *Manager) handleRecoverProject(client *uiws.Client, payload json.RawMessage) {
	var p RecoverProjectPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for RECOVER_PROJECT")
		return
	}
	if p.ProjectName == "" {
		client.SendError("RECOVER_PROJECT requires projectName")
		return
	}

	rec, ok := m.storage.(projectRecoverer)
	if !ok {
		client.SendError("Recovery is not supported by this server")
		return
	}
	state, err := rec.ResyncToHead(p.Namespace, p.ProjectName)
	if err != nil {
		client.SendError(fmt.Sprintf("Failed to recover project: %v", err))
		return
	}

	ack := RecoverAckPayload{
		Namespace: p.Namespace,
		Project:   p.ProjectName,
		State:     string(state),
		Recovered: state == storage.StateHealthy,
	}
	switch state {
	case storage.StateHealthy:
		ack.Message = "Re-synced to the on-disk HEAD; the project is healthy and writes are enabled."
	case storage.StateCorrupted:
		ack.Message = "The working tree has genuine uncommitted drift, so the project stays corrupted. Use the recover dialog's accept-working-tree (adopt) or accept-head (discard) to resolve it."
	case storage.StateDangerous:
		ack.Message = "The project's .git is unreadable or absent (dangerous); it cannot be recovered from here."
	default:
		ack.Message = fmt.Sprintf("Project state: %s.", state)
	}
	client.SendResponse(MsgRecoverAck, ack)
}

// The OAuth/domain/confidential handlers (OAUTH_*/DOMAIN_*/CLIENT_*) live on the
// embedded *CoreHandlers in core_oauth.go (the 2026-06-21 extraction) and are
// promoted onto *Manager, so the dispatch switch reaches them unchanged.

func (m *Manager) handleWriteDraft(client *uiws.Client, payload json.RawMessage) {
	var p WriteDraftPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for WRITE_DRAFT")
		return
	}

	draftPath, err := m.drafts.GetDraftPath(p.Namespace, p.ProjectName, p.Path)
	if err != nil {
		client.SendError(fmt.Sprintf("Failed to get draft path: %v", err))
		return
	}

	if err := m.drafts.SaveDraft(draftPath, []byte(p.Content)); err != nil {
		client.SendError(fmt.Sprintf("Failed to save draft: %v", err))
		return
	}

	client.SendResponse(WriteDraft, map[string]string{
		"status": "ok",
	})
}

func (m *Manager) handleSaveFile(client *uiws.Client, payload json.RawMessage) {
	var p SaveFilePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for SAVE_FILE")
		return
	}

	// A web save is the operator acting as themselves, not an agent: attribute the
	// commit's Author to the configured operator user (identity.WithUser). The
	// empty User keeps single-user mode; a future authenticated request substitutes
	// the actual user at this call site. The ctx-aware Write carries the identity
	// (the old WriteFile path used context.Background() and resolved to the default
	// agent).
	ctx := identity.WithUser(context.Background(), userIdentity(client))
	// Sender identity: this connection originated the write, so the resulting
	// file.write NOTIFY must not be echoed back to it (2026-06-01 directive).
	ctx = notify.WithSender(ctx, client.ID)

	// Resolve Content per content_encoding through the SAME shared helper the MCP
	// write_file tool uses (no duplicate allowlist): empty/"utf8" is literal text
	// (the existing editor/create behaviour); "base64" decodes byte-faithfully and
	// enforces the closed markdown/json/yaml allowlist. This is the external file
	// drag-and-drop ADD path (B-28). A rejection (disallowed format / malformed
	// base64) is surfaced as an ERROR frame and nothing is written.
	content, msg, _, ok := ingest.DecodeContent(p.Path, p.Content, p.ContentEncoding)
	if !ok {
		client.SendError(msg)
		return
	}

	// if_match present → optimistic concurrency; absent → unchecked write (nil),
	// preserving the pre-versioning behaviour for callers that have not adopted it.
	var ifMatch *string
	if p.IfMatch != "" {
		ifMatch = &p.IfMatch
	}

	// No-silent-overwrite on the base64 ingest (drag-and-drop ADD) path (B-28,
	// operator decision ②). A dropped file landing on an existing name must be
	// REFUSED rather than silently clobbered — mirroring move_file's policy
	// (storage.Move: a target that exists with no ifMatch is refused). The guard is
	// scoped to the base64 path so the existing utf8 create/editor flow is
	// unchanged: the client re-sends with the current etag as if_match to confirm an
	// intentional overwrite. The check is server-authoritative (a client cannot
	// bypass it); the etag carried back lets the client confirm-then-overwrite.
	if p.ContentEncoding == "base64" && ifMatch == nil {
		if _, curETag, rerr := m.storage.ReadFileWithETag(p.Namespace, p.ProjectName, p.Path); rerr == nil {
			client.SendResponse(MsgConflict, ConflictPayload{
				Path:        p.Path,
				CurrentETag: curETag,
				Message:     "A file already exists at this path; confirm to overwrite it",
			})
			return
		}
	}

	etag, err := m.storage.Write(ctx, "", p.Namespace, p.ProjectName, p.Path, content, ifMatch)
	if err != nil {
		var conflict *storage.VersionConflictError
		if errors.As(err, &conflict) {
			client.SendResponse(MsgConflict, ConflictPayload{
				Path:        p.Path,
				CurrentETag: conflict.Current,
				Message:     "File was modified by someone else",
			})
			return
		}
		client.SendError(fmt.Sprintf("Failed to save file: %v", err))
		return
	}

	// Return the new etag so the client can use it as if_match for its next save
	// (the editor's read-modify-write loop).
	client.SendResponse(SaveAck, map[string]string{
		"path":   p.Path,
		"status": "ok",
		"etag":   etag,
	})
}
