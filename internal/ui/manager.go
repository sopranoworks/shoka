package ui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sopranoworks/shoka/internal/auth"
	"github.com/sopranoworks/shoka/internal/authz"
	"github.com/sopranoworks/shoka/internal/drafts"
	"github.com/sopranoworks/shoka/internal/identity"
	"github.com/sopranoworks/shoka/internal/ingest"
	"github.com/sopranoworks/shoka/internal/notify"
	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/internal/storage/oauthstore"
	"github.com/sopranoworks/shoka/internal/storage/userstore"
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
	// B-28 ns/proj management part 1: the destructive + identity ops. Project
	// create/delete = admin on the target namespace; namespace create/delete =
	// super-user only (gated via authz.IsSuperUser in authzGate, see wsSuperUserOps).
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
	// MsgDeleteFile removes a file; MsgDeleteAck carries the deleted path back. It
	// is the server half of the B-31 trash-can model: the front-end defers the
	// delete behind a client-side grace timer and sends DELETE_FILE only when the
	// timer elapses (a cancelled reservation never reaches the wire — so there is
	// nothing to undo here). Like SAVE_FILE/MOVE_FILE it carries an optional
	// if_match (captured at enqueue): a stale etag yields the SAME CONFLICT frame
	// SAVE_FILE/MOVE_FILE use, so a file edited mid-grace is not silently destroyed.
	// It wires the EXISTING storage.Delete (git-tracked hard-remove, recoverable via
	// History) — no storage or MCP-tool change.
	MsgDeleteFile MessageType = "DELETE_FILE"
	MsgDeleteAck  MessageType = "DELETE_ACK"
	// MsgListDeleted lists a project's currently-deleted files (the 2026-06-18
	// deleted-log directive) — a cheap O(cap) read of the per-project deleted-file
	// log (no git walk); the response rides the same type carrying the deleted
	// entries. MsgReviveFile re-creates one deleted file forward-only (read the
	// content at the deletion commit's parent, write it back as a NEW commit);
	// MsgReviveAck carries the revived path back, OR a clear divergence error (the
	// deletion commit is gone from git — cap eviction or external rewrite) so the UI
	// surfaces "can no longer be restored", never a silent failure. Both are
	// ADMIN-ONLY (the deleted overlay is an admin affordance), gated server-side via
	// wsLevels — distinct from the client-side grace-period trash-can (DELETE_FILE),
	// which is recent/undoable; this surface is the full git past with no grace.
	MsgListDeleted MessageType = "LIST_DELETED"
	MsgReviveFile  MessageType = "REVIVE_FILE"
	MsgReviveAck   MessageType = "REVIVE_ACK"
	// MsgRecoverProject is the in-product recovery for a project stuck in
	// `corrupted` (uncommitted working-tree drift): it re-syncs the write-path
	// baseline to the ACTUAL on-disk git HEAD and clears a FALSE corrupted flag,
	// re-enabling writes when an external HEAD move (a host `git reset`, an
	// out-of-band landing) stranded a clean project. MsgRecoverAck carries the
	// resulting state back so the badge updates and the operator learns whether the
	// project recovered. It wires storage.ResyncToHead (the same call the MCP
	// recover_project tool uses) — NON-DESTRUCTIVE: it never commits or discards
	// working-tree content, so a genuinely-drifted project stays corrupted (the ack
	// says so, pointing the operator at the destructive accept-working-tree /
	// accept-head modes on the adminapi recover endpoint).
	MsgRecoverProject MessageType = "RECOVER_PROJECT"
	MsgRecoverAck     MessageType = "RECOVER_ACK"
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
	// enabled (no store) — the capability check in the OAUTH_* handlers.
	MsgOAuthList   MessageType = "OAUTH_LIST"
	MsgOAuthRevoke MessageType = "OAUTH_REVOKE"
	// MsgOAuthIssueSelf mints a fresh access token for the current-mode operator
	// (the "token to self" path, B-46b §2.2) and returns it ONCE in the response.
	// This is the single deliberate exception to "no secret crosses /ws/ui": the
	// operator copies the displayed token into their CLI client config. It is
	// admin-gated by the dispatch authz gate (like List/Revoke) and the token is never logged
	// or persisted anywhere on the server beyond the normal token store.
	MsgOAuthIssueSelf MessageType = "OAUTH_ISSUE_SELF"
	// MsgOAuthDenied is the typed refusal frame for the admin-only OAuth requests:
	// reason "forbidden" (the caller is not an administrator) or "oauth_disabled"
	// (OAuth is off, so there is no connection store). It is distinct from the
	// generic ERROR frame so the client can recognise an authorization refusal
	// (and hide the management surface) rather than treat it as a transport error.
	MsgOAuthDenied MessageType = "OAUTH_DENIED"

	// B-71 Stage 2d: domain-mode management — CRUD over the dynamic "domain" registration
	// store (trusted domain + per-domain TTL + per-domain consent), admin-gated.
	MsgDomainList   MessageType = "DOMAIN_LIST"
	MsgDomainCreate MessageType = "DOMAIN_CREATE"
	MsgDomainUpdate MessageType = "DOMAIN_UPDATE"
	MsgDomainDelete MessageType = "DOMAIN_DELETE"
	// B-71 Stage 3: confidential-mode management — issue / list / revoke pre-issued client
	// credentials (Client ID + Secret), admin-gated. The secret is shown ONCE on issue, never
	// returned by list.
	MsgClientIssue  MessageType = "CLIENT_ISSUE"
	MsgClientList   MessageType = "CLIENT_LIST"
	MsgClientRevoke MessageType = "CLIENT_REVOKE"
	// MsgNotify carries one notify.Event pushed from the server to the browser
	// (the 2026-05-31 auto-refresh directive). It is additive: it rides the same
	// {type,payload} envelope as every other message, so existing consumers that
	// switch on type are unaffected.
	MsgNotify MessageType = "NOTIFY"
	Error     MessageType = "ERROR"
	// User-management ops (B-28 stage 3) — all super-user-only (admin level, global),
	// enforced by the stage-2 dispatch gate; the destructive ones additionally refuse
	// the caller's own account (server-side self-guard, defence in depth).
	MsgAdminListUsers      MessageType = "ADMIN_LIST_USERS"
	MsgAdminSetUserScope   MessageType = "ADMIN_SET_USER_SCOPE"
	MsgAdminSetUserEnabled MessageType = "ADMIN_SET_USER_ENABLED"
	MsgAdminRemoveUser     MessageType = "ADMIN_REMOVE_USER"
	MsgAdminCreateInvite   MessageType = "ADMIN_CREATE_INVITE"
	MsgAdminListInvites    MessageType = "ADMIN_LIST_INVITES"
	MsgAdminRevokeInvite   MessageType = "ADMIN_REVOKE_INVITE"
	// MsgPermissionDenied is the authorization-refusal frame for the /ws/ui dispatch
	// gate (the B-28 stage-2 enforcement flip): a session principal whose scope lacks
	// the level the requested operation requires gets this instead of the handler
	// running. Distinct from ERROR so the client can surface a clear, non-fatal "you
	// do not have permission" toast (a read-only user attempting a write).
	MsgPermissionDenied MessageType = "PERMISSION_DENIED"
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
	// ContentEncoding selects how Content is interpreted, identical to the MCP
	// write_file tool: empty/"utf8" is literal text (the existing editor/create
	// behaviour, unchanged); "base64" decodes Content from base64 to raw bytes via
	// the shared ingest helper, enforcing the closed markdown/json/yaml allowlist.
	// The base64 path is the external file drag-and-drop ADD route (B-28): a
	// dropped file is byte-faithful and a name collision is refused, not silently
	// overwritten (see handleSaveFile).
	ContentEncoding string `json:"content_encoding,omitempty"`
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

// DeleteFilePayload is the DELETE_FILE request body. IfMatch is optional and
// carries the same optimistic-concurrency semantic as SAVE_FILE: the delete
// proceeds only if the file's current etag equals it, otherwise a CONFLICT frame
// is returned (the file changed during the client-side grace). Omitted only by a
// caller that did not capture an etag; an empty IfMatch takes the unchecked path.
type DeleteFilePayload struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
	Path        string `json:"path"`
	IfMatch     string `json:"if_match,omitempty"`
}

// DeleteAckPayload is the DELETE_ACK frame's body: the path that was deleted, so
// the client can drop it from its caches/tree and clear the trash item.
type DeleteAckPayload struct {
	Path string `json:"path"`
}

// RecoverProjectPayload is the RECOVER_PROJECT request body: which project to
// re-sync to its on-disk git HEAD.
type RecoverProjectPayload struct {
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
}

// RecoverAckPayload is the RECOVER_ACK frame's body: the project's resulting health
// after the re-sync. Recovered is true iff it is now healthy (writes enabled);
// otherwise State explains why (corrupted = genuine drift, dangerous = unreadable
// .git) and Message carries operator-facing guidance.
type RecoverAckPayload struct {
	Namespace string `json:"namespace"`
	Project   string `json:"project"`
	State     string `json:"state"`
	Recovered bool   `json:"recovered"`
	Message   string `json:"message"`
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
	// Scope is the token's authorization grant (the 2026-06-15 authz foundation):
	// "*" for an all-access (DCR/self-issued) token, or a namespace grant for a
	// future pre-issued scoped token. It is non-secret routing metadata, shown in
	// the admin connections table so the operator can see what each token may reach.
	Scope string `json:"scope"`
	// Domain is the trusted-"domain" entry this connection groups under (B-71 Stage 2d) —
	// the matched entry's identifier (CIMD + DCR connections sit under their domain); "" for
	// the operator self-issued / confidential / untrusted-leftover section. Non-secret.
	Domain string `json:"domain"`
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
	// principal is the authenticated WebUI session principal carried on the upgrade
	// request context (the B-28 stage-1 login: authapi.Middleware attaches it). Zero
	// when no user has logged in yet (the no-lockout single-operator path); when set,
	// hasPrincipal is true and the user's email becomes the git Author on web writes.
	principal    auth.Principal
	hasPrincipal bool
}

// userIdentity returns the owning-user identity for a web write. When the connection
// carries an authenticated session principal (B-28 stage 1), the logged-in user's
// email is the git Author (email = account = git author). Otherwise it returns the
// empty User, which identity.Resolve fills with the configured single operator — the
// pre-login behaviour, preserved so an empty user store is never locked out.
func (c *wsClient) userIdentity() identity.User {
	if c.hasPrincipal {
		return identity.User{Name: c.principal.Name, Email: c.principal.Email}
	}
	return identity.User{}
}

// scope returns the connection's authorization scope: the session principal's scope,
// or "*" (super-user) for the no-principal / no-lockout single-operator connection.
func (c *wsClient) scope() string {
	if c.hasPrincipal {
		return c.principal.Scope
	}
	return "*"
}

// canRead reports whether the connection may read the given namespace (the
// global-read result filter, B-28 stage 3).
func (c *wsClient) canRead(namespace string) bool {
	return authz.Authorize(c.scope(), namespace, "", authz.LevelRead) == nil
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
	// RevokeByPrincipalEmail revokes every token series (and pending auth code) for a
	// principal email — the cross-store access cut when a user is disabled or deleted
	// (B-28). Returns the number of series revoked.
	RevokeByPrincipalEmail(email string) (int, error)

	// B-71 Stage 2d — the dynamic "domain" registration store the domain-mode management
	// screen drives (DOMAIN_* ws ops) + the connection grouping. *oauthstore.Store already
	// satisfies these.
	ListRegistrations() ([]oauthstore.RegistrationEntry, error)
	CreateRegistration(mode, identifier string, now time.Time) (oauthstore.RegistrationEntry, error)
	GetRegistration(id string) (oauthstore.RegistrationEntry, error)
	UpdateRegistration(entry oauthstore.RegistrationEntry) error
	DeleteRegistration(id string) error
	// RevokeByDomain revokes every token series under a domain (the Stage 2d domain-delete
	// cascade). Returns the number revoked.
	RevokeByDomain(domain string) (int, error)
	// DomainEntryForClient returns the "domain" entry a connection's client_id belongs to (for
	// grouping); ok=false for a self-issued/confidential or untrusted-leftover connection.
	DomainEntryForClient(clientID string) (oauthstore.RegistrationEntry, bool)
	// IssueConfidentialClient mints a confidential pre-issued client (B-71 Stage 3): a client_id
	// + a high-entropy secret; only the hash is stored; the RAW secret is returned ONCE.
	IssueConfidentialClient(scope string, validity time.Duration, now time.Time) (oauthstore.RegistrationEntry, string, error)
	// RevokeByClientID revokes every token series issued to a client_id (the confidential-client
	// delete cascade). Returns the number revoked.
	RevokeByClientID(clientID string) (int, error)
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

// UserAdminStore is the narrow capability the super-user-only user-management ops
// (B-28 stage 3) depend on — exactly the userstore admin/invite methods. The Manager
// depends on this interface, not the concrete *userstore.Store, so the handle stays
// nil when the user store is absent and tests can inject a fake. *userstore.Store
// already satisfies it.
type UserAdminStore interface {
	ListUsers() ([]userstore.UserInfo, error)
	UpdateUserScope(email, scope string) error
	SetUserDisabled(email string, disabled bool) error
	RemoveUser(email string) error
	CreateInvite(email, scope, createdBy string, now time.Time, ttl time.Duration) (string, userstore.InviteRecord, error)
	ListInvites() ([]userstore.InviteInfo, error)
	RevokeInvite(codeHash string) error
}

// Administrator authorization for the OAUTH_* management requests is enforced by the
// single stage-2 dispatch authzGate (OAUTH_* are admin-level in wsLevels) — there is
// no separate admin seam. The former singleUserAdmin/AdminAuthorizer/adminGate
// (a redundant always-true seam, "config-admin") was retired in stage 4 once the DB
// super-user + the empty-store first-run wizard became the only admin paths (B-28).

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
	// users backs the super-user-only user-management ops (B-28 stage 3). nil when no
	// user store is wired (the ADMIN_* handlers then report it unavailable).
	users UserAdminStore
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

// SetUserStore wires the user store for the super-user-only user-management ops
// (B-28 stage 3). Called in startup; when unset the ADMIN_* handlers report the
// capability unavailable.
func (m *Manager) SetUserStore(u UserAdminStore) {
	m.users = u
}

// NotifyDrops reports how many notify events were dropped because a client's
// send buffer was full (observability; used by tests).
func (m *Manager) NotifyDrops() int64 { return m.notifyDrops.Load() }

// PermissionDeniedPayload is the body of a PERMISSION_DENIED frame: which operation
// was refused, the target namespace, the level it required, and a human reason.
type PermissionDeniedPayload struct {
	Op        string `json:"op"`
	Namespace string `json:"namespace,omitempty"`
	Required  string `json:"required"`
	Message   string `json:"message"`
}

// wsOp is a /ws/ui message's authorization requirement: the level it needs, and
// whether it is a GLOBAL op (no target namespace — its target is the whole server, so
// the gate ignores any payload namespace and uses the principal's max level anywhere).
type wsOp struct {
	level  authz.Level
	global bool
}

// wsLevels is the single registry mapping each /ws/ui message to its required level —
// the WebUI counterpart of the MCP toolLevels table, feeding the SAME authz.Authorize.
// Reads need read; content mutations need write; project recovery and the OAuth
// connection-management ops need admin. A message absent from this table fails CLOSED
// at admin (global), so a newly-added message must be classified before a
// non-super-user can reach it.
var wsLevels = map[MessageType]wsOp{
	GetProjects:    {authz.LevelRead, true}, // global: lists every namespace
	GetTree:        {authz.LevelRead, false},
	ReadFile:       {authz.LevelRead, false},
	MsgSearchFiles: {authz.LevelRead, false},
	MsgGetHistory:  {authz.LevelRead, false},
	MsgGetFileAt:   {authz.LevelRead, false},
	MsgGetDiff:     {authz.LevelRead, false},

	WriteDraft:    {authz.LevelWrite, false},
	SaveFile:      {authz.LevelWrite, false},
	MsgMoveFile:   {authz.LevelWrite, false},
	MsgDeleteFile: {authz.LevelWrite, false},

	// Project create/delete = admin on the target namespace (B-28; create RAISED from
	// write — a write-only principal can no longer create projects). The namespace ops
	// (CREATE_NAMESPACE/DELETE_NAMESPACE) are NOT here: they are super-user only and
	// handled by wsSuperUserOps, not the namespace-targeted gate.
	MsgCreateProject: {authz.LevelAdmin, false},
	MsgDeleteProject: {authz.LevelAdmin, false},

	// RENAME_PROJECT = admin on the namespace (the project stays in its namespace — looser
	// than MOVE_PROJECT, which is super-user). RENAME_NAMESPACE is NOT here: it is super-user
	// only and handled by wsSuperUserOps.
	MsgRenameProject: {authz.LevelAdmin, false},

	MsgRecoverProject: {authz.LevelAdmin, false},

	// Deleted-file log ops (B-28, the 2026-06-18 directive): admin on the target
	// namespace (the recover_project template; namespace-scoped, not global).
	MsgListDeleted: {authz.LevelAdmin, false},
	MsgReviveFile:  {authz.LevelAdmin, false},

	MsgOAuthList:      {authz.LevelAdmin, true},
	MsgOAuthRevoke:    {authz.LevelAdmin, true},
	MsgOAuthIssueSelf: {authz.LevelAdmin, true},

	// Domain-mode management (B-71 Stage 2d): admin, global.
	MsgDomainList:   {authz.LevelAdmin, true},
	MsgDomainCreate: {authz.LevelAdmin, true},
	MsgDomainUpdate: {authz.LevelAdmin, true},
	MsgDomainDelete: {authz.LevelAdmin, true},
	MsgClientIssue:  {authz.LevelAdmin, true},
	MsgClientList:   {authz.LevelAdmin, true},
	MsgClientRevoke: {authz.LevelAdmin, true},

	MsgAdminListUsers:      {authz.LevelAdmin, true},
	MsgAdminSetUserScope:   {authz.LevelAdmin, true},
	MsgAdminSetUserEnabled: {authz.LevelAdmin, true},
	MsgAdminRemoveUser:     {authz.LevelAdmin, true},
	MsgAdminCreateInvite:   {authz.LevelAdmin, true},
	MsgAdminListInvites:    {authz.LevelAdmin, true},
	MsgAdminRevokeInvite:   {authz.LevelAdmin, true},

	// Health read = admin-somewhere (global admin target; the handler filters to the
	// principal's admin namespaces). Recovery = admin on the target namespace (the handler
	// tightens whole-namespace actions to super-user).
	MsgNamespaceHealth:  {authz.LevelAdmin, true},
	MsgNamespaceRecover: {authz.LevelAdmin, false},
}

// wsSuperUserOps are the /ws/ui messages that require a SUPER-USER (wildcard admin), not
// merely admin-on-a-namespace — namespace create/delete (B-28). They are gated via
// authz.IsSuperUser, NOT the namespace-targeted Authorize a namespace-admin would satisfy
// for its own namespace (the loose empty-target footgun). They are deliberately absent
// from wsLevels so authzGate routes them through the super-user check, checked FIRST.
var wsSuperUserOps = map[MessageType]bool{
	MsgCreateNamespace: true,
	MsgDeleteNamespace: true,
	MsgMoveProject:     true,
	MsgRenameNamespace: true, // a namespace rename relabels the whole namespace + all its grants
}

// authzGate applies the shared authz decision to one /ws/ui message before its
// handler runs. It returns true to PROCEED and false when the message was refused
// (a PERMISSION_DENIED frame has been sent). The principal is the connection's session
// principal (stage 1); when absent — the no-lockout empty-store / single-operator path
// that RequireSession let through — the connection is treated as super-user. This is
// the ONE /ws/ui enforcement site (not per-handler), calling the same authz.Authorize
// the MCP middleware uses.
func (m *Manager) authzGate(client *wsClient, msgType MessageType, payload json.RawMessage) bool {
	scope := "*" // no session principal ⇒ super-user (no-lockout / single-operator)
	if client.hasPrincipal {
		scope = client.principal.Scope
	}
	// Super-user-only ops (namespace create/delete) are checked FIRST, via the strict
	// IsSuperUser predicate — never the namespace-targeted Authorize a namespace-admin
	// would satisfy for its own namespace (B-28 ns/proj management).
	if wsSuperUserOps[msgType] {
		if !authz.IsSuperUser(scope) {
			client.sendResponse(MsgPermissionDenied, PermissionDeniedPayload{
				Op:       string(msgType),
				Required: "super-user",
				Message:  "permission denied: namespace management requires a super-user",
			})
			return false
		}
		return true
	}
	op, known := wsLevels[msgType]
	if !known {
		op = wsOp{level: authz.LevelAdmin, global: true} // fail closed
	}
	var ns, proj string
	if !op.global {
		ns, proj = wsTarget(payload)
	}
	if err := authz.Authorize(scope, ns, proj, op.level); err != nil {
		client.sendResponse(MsgPermissionDenied, PermissionDeniedPayload{
			Op:        string(msgType),
			Namespace: ns,
			Required:  op.level.String(),
			Message:   "permission denied: " + err.Error(),
		})
		return false
	}
	return true
}

// wsTarget decodes the target namespace/project from a /ws/ui message payload (the
// uniform `namespace`/`projectName` keys every namespaced payload carries).
func wsTarget(payload json.RawMessage) (namespace, project string) {
	var t struct {
		Namespace   string `json:"namespace"`
		ProjectName string `json:"projectName"`
	}
	if len(payload) > 0 {
		_ = json.Unmarshal(payload, &t)
	}
	return t.Namespace, t.ProjectName
}

func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := m.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade error: %v", err)
		return
	}
	defer conn.Close()
	client := &wsClient{conn: conn, id: fmt.Sprintf("ws-%d", m.connSeq.Add(1)), req: r}
	// The WebUI session principal (B-28 stage 1) rides the upgrade request context,
	// attached by authapi.Middleware from the session cookie. Capturing it here lets
	// web writes be authored as the logged-in user (email = git author) and is the
	// seam the later enforcement sweep reads. Absent when no user has logged in.
	if p, ok := auth.PrincipalFrom(r.Context()); ok {
		client.principal = p
		client.hasPrincipal = true
	}

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

		// The single /ws/ui authorization gate (B-28 stage-2 flip): every message is
		// checked here, before its handler, through the shared authz.Authorize — not
		// scattered into the handlers. A refusal sends PERMISSION_DENIED and skips the
		// handler.
		if !m.authzGate(client, wsMsg.Type, wsMsg.Payload) {
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
		case MsgOAuthList:
			m.handleOAuthList(client)
		case MsgOAuthRevoke:
			m.handleOAuthRevoke(client, wsMsg.Payload)
		case MsgOAuthIssueSelf:
			m.handleOAuthIssueSelf(client)
		case MsgDomainList:
			m.handleDomainList(client)
		case MsgDomainCreate:
			m.handleDomainCreate(client, wsMsg.Payload)
		case MsgDomainUpdate:
			m.handleDomainUpdate(client, wsMsg.Payload)
		case MsgDomainDelete:
			m.handleDomainDelete(client, wsMsg.Payload)
		case MsgClientList:
			m.handleConfidentialList(client)
		case MsgClientIssue:
			m.handleConfidentialIssue(client, wsMsg.Payload)
		case MsgClientRevoke:
			m.handleConfidentialRevoke(client, wsMsg.Payload)
		case MsgAdminListUsers:
			m.handleAdminListUsers(client)
		case MsgAdminSetUserScope:
			m.handleAdminSetUserScope(client, wsMsg.Payload)
		case MsgAdminSetUserEnabled:
			m.handleAdminSetUserEnabled(client, wsMsg.Payload)
		case MsgAdminRemoveUser:
			m.handleAdminRemoveUser(client, wsMsg.Payload)
		case MsgAdminCreateInvite:
			m.handleAdminCreateInvite(client, wsMsg.Payload)
		case MsgAdminListInvites:
			m.handleAdminListInvites(client)
		case MsgAdminRevokeInvite:
			m.handleAdminRevokeInvite(client, wsMsg.Payload)
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
		// Global-read filter (B-28 stage 3, the deferred stage-2 item): a logged-in
		// scoped user sees only the namespaces it has at least read on; a super-user
		// (or the no-principal no-lockout connection) sees all. Result-shaping after
		// the gate already authorized the global read.
		if !client.canRead(ns) {
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

// NamespacePayload carries just a namespace name (CREATE_NAMESPACE / DELETE_NAMESPACE).
type NamespacePayload struct {
	Namespace string `json:"namespace"`
}

func (m *Manager) handleDeleteProject(client *wsClient, payload json.RawMessage) {
	var p CreateProjectPayload // {namespace, projectName} — same shape as create
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for DELETE_PROJECT")
		return
	}
	pd, ok := m.storage.(projectDeleter)
	if !ok {
		client.sendError("project deletion is not available on this server")
		return
	}
	ctx := notify.WithSender(context.Background(), client.id)
	if err := pd.DeleteProject(ctx, p.Namespace, p.ProjectName); err != nil {
		client.sendError(fmt.Sprintf("Failed to delete project: %v", err))
		return
	}
	client.sendResponse(MsgDeleteProject, map[string]string{"status": "ok"})
}

func (m *Manager) handleCreateNamespace(client *wsClient, payload json.RawMessage) {
	var p NamespacePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for CREATE_NAMESPACE")
		return
	}
	nm, ok := m.storage.(namespaceManager)
	if !ok {
		client.sendError("namespace management is not available on this server")
		return
	}
	if err := nm.CreateNamespace(p.Namespace); err != nil {
		client.sendError(fmt.Sprintf("Failed to create namespace: %v", err))
		return
	}
	client.sendResponse(MsgCreateNamespace, map[string]string{"status": "ok"})
}

func (m *Manager) handleDeleteNamespace(client *wsClient, payload json.RawMessage) {
	var p NamespacePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for DELETE_NAMESPACE")
		return
	}
	nm, ok := m.storage.(namespaceManager)
	if !ok {
		client.sendError("namespace management is not available on this server")
		return
	}
	ctx := notify.WithSender(context.Background(), client.id)
	if err := nm.DeleteNamespace(ctx, p.Namespace); err != nil {
		client.sendError(fmt.Sprintf("Failed to delete namespace: %v", err))
		return
	}
	client.sendResponse(MsgDeleteNamespace, map[string]string{"status": "ok"})
}

// MoveProjectPayload is the /ws/ui project-move request (B-28). namespace is the SOURCE;
// newNamespace the TARGET (must pre-exist). super-user-gated (wsSuperUserOps).
type MoveProjectPayload struct {
	Namespace    string `json:"namespace"`
	ProjectName  string `json:"projectName"`
	NewNamespace string `json:"newNamespace"`
}

func (m *Manager) handleMoveProject(client *wsClient, payload json.RawMessage) {
	var p MoveProjectPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for MOVE_PROJECT")
		return
	}
	mv, ok := m.storage.(projectMover)
	if !ok {
		client.sendError("project move is not available on this server")
		return
	}
	ctx := notify.WithSender(context.Background(), client.id)
	if err := mv.MoveProject(ctx, p.Namespace, p.ProjectName, p.NewNamespace); err != nil {
		client.sendError(fmt.Sprintf("Failed to move project: %v", err))
		return
	}
	client.sendResponse(MsgMoveProject, map[string]string{"status": "ok"})
}

// RenameProjectPayload is the /ws/ui project-rename request (B-28). namespace + projectName
// identify the project; newProjectName is the new name (must be free in the namespace).
// admin-on-namespace-gated (wsLevels). Keys match wsTarget (namespace/projectName).
type RenameProjectPayload struct {
	Namespace      string `json:"namespace"`
	ProjectName    string `json:"projectName"`
	NewProjectName string `json:"newProjectName"`
}

func (m *Manager) handleRenameProject(client *wsClient, payload json.RawMessage) {
	var p RenameProjectPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for RENAME_PROJECT")
		return
	}
	rn, ok := m.storage.(projectRenamer)
	if !ok {
		client.sendError("project rename is not available on this server")
		return
	}
	ctx := notify.WithSender(context.Background(), client.id)
	if err := rn.RenameProject(ctx, p.Namespace, p.ProjectName, p.NewProjectName); err != nil {
		client.sendError(fmt.Sprintf("Failed to rename project: %v", err))
		return
	}
	client.sendResponse(MsgRenameProject, map[string]string{"status": "ok"})
}

// RenameNamespacePayload is the /ws/ui namespace-rename request (B-28). namespace is the
// current name; newNamespace the new name. super-user-gated (wsSuperUserOps).
type RenameNamespacePayload struct {
	Namespace    string `json:"namespace"`
	NewNamespace string `json:"newNamespace"`
}

func (m *Manager) handleRenameNamespace(client *wsClient, payload json.RawMessage) {
	var p RenameNamespacePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for RENAME_NAMESPACE")
		return
	}
	rn, ok := m.storage.(namespaceRenamer)
	if !ok {
		client.sendError("namespace rename is not available on this server")
		return
	}
	ctx := notify.WithSender(context.Background(), client.id)
	if err := rn.RenameNamespace(ctx, p.Namespace, p.NewNamespace); err != nil {
		client.sendError(fmt.Sprintf("Failed to rename namespace: %v", err))
		return
	}
	client.sendResponse(MsgRenameNamespace, map[string]string{"status": "ok"})
}

// NamespaceRecoverPayload is the /ws/ui recovery request. ProjectName empty ⇒ a
// whole-namespace action (super-user only). Keys match wsTarget (namespace/projectName).
type NamespaceRecoverPayload struct {
	Action      string `json:"action"`
	Namespace   string `json:"namespace"`
	ProjectName string `json:"projectName"`
}

func (m *Manager) handleNamespaceHealth(client *wsClient) {
	hr, ok := m.storage.(namespaceHealthReader)
	if !ok {
		client.sendError("namespace health is not available on this server")
		return
	}
	// The gate authorized admin-somewhere; filter the picture to the principal's admin
	// namespaces (a super-user sees all, incl. base-level foreign namespaces).
	client.sendResponse(MsgNamespaceHealth, filterHealthByAdminScope(hr.CheckAllHealth(), client.scope()))
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

func (m *Manager) handleNamespaceRecover(client *wsClient, payload json.RawMessage) {
	var p NamespaceRecoverPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for NAMESPACE_RECOVER")
		return
	}
	nr, ok := m.storage.(namespaceRecoverer)
	if !ok {
		client.sendError("namespace recovery is not available on this server")
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
		if nsLevel && !authz.IsSuperUser(client.scope()) {
			client.sendResponse(MsgPermissionDenied, PermissionDeniedPayload{
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
			client.sendError("clean_orphaned requires projectName (the stray's base name)")
			return
		}
		err = nr.CleanOrphanedSibling(p.Namespace, p.ProjectName)
	case "adopt":
		if denyNamespaceLevel() {
			return
		}
		err = nr.AdoptForeign(p.Namespace, p.ProjectName)
	default:
		client.sendError("invalid action: must be drop_missing | clean_orphaned | adopt")
		return
	}
	if err != nil {
		client.sendError(fmt.Sprintf("namespace recovery failed: %v", err))
		return
	}
	client.sendResponse(MsgNamespaceRecover, map[string]string{"status": "ok"})
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

	ctx := identity.WithUser(context.Background(), client.userIdentity())
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

// handleDeleteFile removes a file via the existing storage.Delete, mirroring
// handleMoveFile: like SAVE_FILE/MOVE_FILE it is the operator acting as themselves
// (identity.WithUser → operator is the commit Author) and carries the connection's
// sender id so the resulting file.delete NOTIFY is not echoed back to this
// connection. A stale if_match returns the SAME CONFLICT frame SAVE_FILE/MOVE_FILE
// use (the file changed during the client-side grace), so a mid-grace edit is
// surfaced rather than silently destroyed; success returns DELETE_ACK with the
// deleted path. No storage or tool change — the delete is git-tracked and
// recoverable via History.
func (m *Manager) handleDeleteFile(client *wsClient, payload json.RawMessage) {
	var p DeleteFilePayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for DELETE_FILE")
		return
	}

	ctx := identity.WithUser(context.Background(), client.userIdentity())
	ctx = notify.WithSender(ctx, client.id)

	var ifMatch *string
	if p.IfMatch != "" {
		ifMatch = &p.IfMatch
	}

	if err := m.storage.Delete(ctx, "", p.Namespace, p.ProjectName, p.Path, ifMatch); err != nil {
		var conflict *storage.VersionConflictError
		if errors.As(err, &conflict) {
			client.sendResponse(MsgConflict, ConflictPayload{
				Path:        p.Path,
				CurrentETag: conflict.Current,
				Message:     "Delete rejected: the file was modified after it was queued",
			})
			return
		}
		client.sendError(fmt.Sprintf("Failed to delete file: %v", err))
		return
	}

	client.sendResponse(MsgDeleteAck, DeleteAckPayload{Path: p.Path})
}

// handleRecoverProject re-syncs a project's write-path baseline to the actual
// on-disk git HEAD (storage.ResyncToHead) and reports the resulting state. It is the
// Web UI half of the stale-HEAD recovery: a clean-on-disk project stranded in a
// false `corrupted` is restored to healthy and writes re-enable; a project with
// genuine uncommitted drift stays corrupted and the ack says so. Non-destructive —
// no commit, no discard.
func (m *Manager) handleRecoverProject(client *wsClient, payload json.RawMessage) {
	var p RecoverProjectPayload
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for RECOVER_PROJECT")
		return
	}
	if p.ProjectName == "" {
		client.sendError("RECOVER_PROJECT requires projectName")
		return
	}

	rec, ok := m.storage.(projectRecoverer)
	if !ok {
		client.sendError("Recovery is not supported by this server")
		return
	}
	state, err := rec.ResyncToHead(p.Namespace, p.ProjectName)
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to recover project: %v", err))
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
	client.sendResponse(MsgRecoverAck, ack)
}

// oauthAvailable is the OAUTH_* CAPABILITY check: it returns false (sending an
// OAUTH_DENIED "oauth_disabled" frame) when OAuth is off so there is no store, so the
// handlers never nil-panic. Administrator AUTHORIZATION is NOT checked here — it is
// enforced upstream by the single stage-2 dispatch authzGate (OAUTH_* are admin-level
// in wsLevels). This replaced the retired adminGate/singleUserAdmin seam (stage 4).
func (m *Manager) oauthAvailable(client *wsClient) bool {
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
// (oauthstore.SeriesInfo). Administrator-only (the dispatch authz gate). Read-only — no commit,
// no NOTIFY — so, like handleSearchFiles, it carries no identity or sender
// context. The Connections slice is always non-nil so the wire shape is always
// {"connections": [...]} (the empty-state client renders [] as "no connections").
func (m *Manager) handleOAuthList(client *wsClient) {
	if !m.oauthAvailable(client) {
		return
	}
	infos, err := m.oauth.List()
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to list OAuth connections: %v", err))
		return
	}
	// Defined order (the 2026-06-15 admin-UI directive): newest connection first.
	// Store.List() iterates bbolt key order (= random series-id order), which left
	// the admin table in an unexplained sequence; the handler is the sort owner so
	// the wire response is authoritatively issued_at-descending. Ties (same issue
	// instant) fall back to the series id for a stable, deterministic order.
	sort.Slice(infos, func(i, j int) bool {
		if infos[i].IssuedAt.Equal(infos[j].IssuedAt) {
			return infos[i].SeriesID < infos[j].SeriesID
		}
		return infos[i].IssuedAt.After(infos[j].IssuedAt)
	})
	conns := make([]OAuthConnectionInfo, 0, len(infos))
	for _, s := range infos {
		c := toOAuthConnectionInfo(s)
		// B-71 Stage 2d: tag each connection with the trusted-"domain" entry it groups under
		// (CIMD + DCR sit under their domain; self-issued/confidential/untrusted ⇒ ""). The
		// UI groups by this; OAUTH_LIST stays a flat array so existing readers are unaffected.
		if entry, ok := m.oauth.DomainEntryForClient(s.ClientID); ok {
			c.Domain = entry.Identifier
		}
		conns = append(conns, c)
	}
	client.sendResponse(MsgOAuthList, OAuthListPayload{Connections: conns})
}

// handleOAuthRevoke revokes one connection by series id (oauthstore.Revoke).
// Administrator-only (the dispatch authz gate). Revoking one series leaves every other intact
// (the store guarantees it). An absent series_id is a typed error rather than a
// silent no-op; a well-formed but already-revoked id succeeds idempotently (the
// store's Revoke is idempotent — the right behaviour when two admins race or the
// row is already gone).
func (m *Manager) handleOAuthRevoke(client *wsClient, payload json.RawMessage) {
	if !m.oauthAvailable(client) {
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

// --- B-71 Stage 2d: domain-mode management (DOMAIN_* ws ops) ---

// DomainInfo is the no-secret view of a "domain" RegistrationEntry: its identifier, per-domain
// TTL (seconds; 0 = unset → the finite global default), and whether a per-domain consent is
// set. The consent VALUE/hash is NEVER returned — only the set/unset indicator (Stage 0/2b).
type DomainInfo struct {
	ID                string `json:"id"`
	Domain            string `json:"domain"`
	AccessTTLSeconds  int64  `json:"access_ttl_seconds"`
	RefreshTTLSeconds int64  `json:"refresh_ttl_seconds"`
	ConsentSet        bool   `json:"consent_set"`
}

// DomainListPayload is the DOMAIN_LIST response (sorted by identifier).
type DomainListPayload struct {
	Domains []DomainInfo `json:"domains"`
}

// DomainCreateRequest creates a "domain" entry; Consent (optional) is hashed on write.
type DomainCreateRequest struct {
	Domain            string `json:"domain"`
	AccessTTLSeconds  int64  `json:"access_ttl_seconds"`
	RefreshTTLSeconds int64  `json:"refresh_ttl_seconds"`
	Consent           string `json:"consent"`
}

// DomainUpdateRequest edits a domain's TTL and optionally sets/clears its consent: SetConsent
// nil = leave unchanged; "" = clear; non-empty = set (hashed).
type DomainUpdateRequest struct {
	ID                string  `json:"id"`
	AccessTTLSeconds  int64   `json:"access_ttl_seconds"`
	RefreshTTLSeconds int64   `json:"refresh_ttl_seconds"`
	SetConsent        *string `json:"set_consent"`
}

// DomainDeleteRequest / DomainDeletePayload — delete a domain (revoking its tokens).
type DomainDeleteRequest struct {
	ID string `json:"id"`
}
type DomainDeletePayload struct {
	ID            string `json:"id"`
	RevokedTokens int    `json:"revoked_tokens"`
	Status        string `json:"status"`
}

func domainInfoOf(e oauthstore.RegistrationEntry) DomainInfo {
	di := DomainInfo{ID: e.ID, Domain: e.Identifier, ConsentSet: e.Consent != nil && e.Consent.Hash != ""}
	if e.TTL != nil {
		di.AccessTTLSeconds = e.TTL.AccessSeconds
		di.RefreshTTLSeconds = e.TTL.RefreshSeconds
	}
	return di
}

func (m *Manager) handleDomainList(client *wsClient) {
	if !m.oauthAvailable(client) {
		return
	}
	entries, err := m.oauth.ListRegistrations()
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to list domains: %v", err))
		return
	}
	out := make([]DomainInfo, 0)
	for _, e := range entries {
		if e.RegistrationMode == oauthstore.RegistrationModeDomain {
			out = append(out, domainInfoOf(e))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	client.sendResponse(MsgDomainList, DomainListPayload{Domains: out})
}

func (m *Manager) handleDomainCreate(client *wsClient, payload json.RawMessage) {
	if !m.oauthAvailable(client) {
		return
	}
	var p DomainCreateRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for DOMAIN_CREATE")
		return
	}
	if strings.TrimSpace(p.Domain) == "" {
		client.sendError("DOMAIN_CREATE requires a domain")
		return
	}
	if p.AccessTTLSeconds < 0 || p.RefreshTTLSeconds < 0 {
		client.sendError("DOMAIN_CREATE TTL must not be negative")
		return
	}
	entry, err := m.oauth.CreateRegistration(oauthstore.RegistrationModeDomain, p.Domain, time.Now())
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to create domain: %v", err))
		return
	}
	if p.AccessTTLSeconds > 0 || p.RefreshTTLSeconds > 0 {
		entry.TTL = &oauthstore.EntryTTL{AccessSeconds: p.AccessTTLSeconds, RefreshSeconds: p.RefreshTTLSeconds}
	}
	if p.Consent != "" {
		entry.SetConsent(p.Consent)
	}
	if err := m.oauth.UpdateRegistration(entry); err != nil {
		client.sendError(fmt.Sprintf("Failed to set domain TTL/consent: %v", err))
		return
	}
	client.sendResponse(MsgDomainCreate, domainInfoOf(entry))
}

func (m *Manager) handleDomainUpdate(client *wsClient, payload json.RawMessage) {
	if !m.oauthAvailable(client) {
		return
	}
	var p DomainUpdateRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for DOMAIN_UPDATE")
		return
	}
	if p.ID == "" {
		client.sendError("DOMAIN_UPDATE requires an id")
		return
	}
	if p.AccessTTLSeconds < 0 || p.RefreshTTLSeconds < 0 {
		client.sendError("DOMAIN_UPDATE TTL must not be negative")
		return
	}
	entry, err := m.oauth.GetRegistration(p.ID)
	if err != nil {
		client.sendError("Domain not found")
		return
	}
	if entry.RegistrationMode != oauthstore.RegistrationModeDomain {
		client.sendError("not a domain entry")
		return
	}
	if p.AccessTTLSeconds > 0 || p.RefreshTTLSeconds > 0 {
		entry.TTL = &oauthstore.EntryTTL{AccessSeconds: p.AccessTTLSeconds, RefreshSeconds: p.RefreshTTLSeconds}
	} else {
		entry.TTL = nil // both 0 ⇒ unset → the finite global default
	}
	if p.SetConsent != nil {
		entry.SetConsent(*p.SetConsent) // "" clears; non-empty sets (hashed — never returned)
	}
	if err := m.oauth.UpdateRegistration(entry); err != nil {
		client.sendError(fmt.Sprintf("Failed to update domain: %v", err))
		return
	}
	client.sendResponse(MsgDomainUpdate, domainInfoOf(entry))
}

func (m *Manager) handleDomainDelete(client *wsClient, payload json.RawMessage) {
	if !m.oauthAvailable(client) {
		return
	}
	var p DomainDeleteRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for DOMAIN_DELETE")
		return
	}
	if p.ID == "" {
		client.sendError("DOMAIN_DELETE requires an id")
		return
	}
	entry, err := m.oauth.GetRegistration(p.ID)
	if err != nil {
		client.sendError("Domain not found")
		return
	}
	// Policy (B-71 Stage 2d): deleting a domain makes it untrusted AND revokes its live tokens
	// (mirroring the user-delete OAuth-revoke cascade) — its connections cannot keep working.
	revoked, _ := m.oauth.RevokeByDomain(entry.Identifier)
	if err := m.oauth.DeleteRegistration(p.ID); err != nil {
		client.sendError(fmt.Sprintf("Failed to delete domain: %v", err))
		return
	}
	client.sendResponse(MsgDomainDelete, DomainDeletePayload{ID: p.ID, RevokedTokens: revoked, Status: "ok"})
}

// --- Confidential-client management (B-71 Stage 3) --------------------------

// ConfidentialClientInfo is the no-secret view of a "confidential" RegistrationEntry: its issued
// client_id, pre-issued scope, finite credential expiry, and creation time. The secret/hash is
// NEVER returned (only the issue response carries the raw secret, once).
type ConfidentialClientInfo struct {
	ID        string `json:"id"`
	ClientID  string `json:"client_id"`
	Scope     string `json:"scope"`
	ExpiresAt string `json:"expires_at"` // RFC3339
	CreatedAt string `json:"created_at"` // RFC3339
}

// ConfidentialListPayload is the CLIENT_LIST response (newest first).
type ConfidentialListPayload struct {
	Clients []ConfidentialClientInfo `json:"clients"`
}

// ConfidentialIssueRequest issues a confidential client: a pre-issued scope + a finite validity
// in seconds (no indefinite).
type ConfidentialIssueRequest struct {
	Scope           string `json:"scope"`
	ValiditySeconds int64  `json:"validity_seconds"`
}

// ConfidentialIssuePayload returns the no-secret info PLUS the raw client_secret — shown ONCE at
// issuance and never persisted or returned again.
type ConfidentialIssuePayload struct {
	ConfidentialClientInfo
	ClientSecret string `json:"client_secret"`
}

// ConfidentialRevokeRequest / ConfidentialRevokePayload — revoke a confidential client (deleting
// its entry and cascade-revoking the tokens it issued).
type ConfidentialRevokeRequest struct {
	ID string `json:"id"`
}
type ConfidentialRevokePayload struct {
	ID            string `json:"id"`
	RevokedTokens int    `json:"revoked_tokens"`
	Status        string `json:"status"`
}

func confidentialInfoOf(e oauthstore.RegistrationEntry) ConfidentialClientInfo {
	return ConfidentialClientInfo{
		ID:        e.ID,
		ClientID:  e.Identifier,
		Scope:     e.Scope,
		ExpiresAt: e.ExpiresAt.UTC().Format(time.RFC3339),
		CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339),
	}
}

func (m *Manager) handleConfidentialList(client *wsClient) {
	if !m.oauthAvailable(client) {
		return
	}
	entries, err := m.oauth.ListRegistrations()
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to list confidential clients: %v", err))
		return
	}
	out := make([]ConfidentialClientInfo, 0)
	for _, e := range entries {
		if e.RegistrationMode == oauthstore.RegistrationModeConfidential {
			out = append(out, confidentialInfoOf(e))
		}
	}
	// Newest first (CreatedAt is RFC3339, so lexical order is chronological).
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	client.sendResponse(MsgClientList, ConfidentialListPayload{Clients: out})
}

func (m *Manager) handleConfidentialIssue(client *wsClient, payload json.RawMessage) {
	if !m.oauthAvailable(client) {
		return
	}
	var p ConfidentialIssueRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for CLIENT_ISSUE")
		return
	}
	if strings.TrimSpace(p.Scope) == "" {
		client.sendError("CLIENT_ISSUE requires a scope")
		return
	}
	if p.ValiditySeconds <= 0 {
		client.sendError("CLIENT_ISSUE requires a finite positive validity (no indefinite)")
		return
	}
	entry, secret, err := m.oauth.IssueConfidentialClient(p.Scope, time.Duration(p.ValiditySeconds)*time.Second, time.Now())
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to issue confidential client: %v", err))
		return
	}
	// The raw secret crosses /ws/ui ONCE here (admin-gated), like OAUTH_ISSUE_SELF; it is never
	// logged and never returned again.
	client.sendResponse(MsgClientIssue, ConfidentialIssuePayload{
		ConfidentialClientInfo: confidentialInfoOf(entry),
		ClientSecret:           secret,
	})
}

func (m *Manager) handleConfidentialRevoke(client *wsClient, payload json.RawMessage) {
	if !m.oauthAvailable(client) {
		return
	}
	var p ConfidentialRevokeRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for CLIENT_REVOKE")
		return
	}
	if p.ID == "" {
		client.sendError("CLIENT_REVOKE requires an id")
		return
	}
	entry, err := m.oauth.GetRegistration(p.ID)
	if err != nil {
		client.sendError("Confidential client not found")
		return
	}
	if entry.RegistrationMode != oauthstore.RegistrationModeConfidential {
		client.sendError("not a confidential client")
		return
	}
	// Cascade: revoking the credential cuts the tokens it issued, then deletes the entry.
	revoked, _ := m.oauth.RevokeByClientID(entry.Identifier)
	if err := m.oauth.DeleteRegistration(p.ID); err != nil {
		client.sendError(fmt.Sprintf("Failed to revoke confidential client: %v", err))
		return
	}
	client.sendResponse(MsgClientRevoke, ConfidentialRevokePayload{ID: p.ID, RevokedTokens: revoked, Status: "ok"})
}

// handleOAuthIssueSelf mints a fresh access token bound to the current-mode
// operator (the "token to self" path, B-46b §2.2) and returns it ONCE. It is the
// only place a secret token crosses /ws/ui — a deliberate, admin-gated exception
// so the operator can paste the token into their CLI client config. The token is
// NOT logged (no log statement carries it) and is persisted only in the normal
// token store. Administrator-only via the dispatch authz gate, like List/Revoke; the
// issuer being nil (OAuth disabled) is reported as oauth_disabled.
func (m *Manager) handleOAuthIssueSelf(client *wsClient) {
	if !m.oauthAvailable(client) {
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
		Scope:          s.Scope,
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
	ctx := identity.WithUser(context.Background(), client.userIdentity())
	// Sender identity: this connection originated the write, so the resulting
	// file.write NOTIFY must not be echoed back to it (2026-06-01 directive).
	ctx = notify.WithSender(ctx, client.id)

	// Resolve Content per content_encoding through the SAME shared helper the MCP
	// write_file tool uses (no duplicate allowlist): empty/"utf8" is literal text
	// (the existing editor/create behaviour); "base64" decodes byte-faithfully and
	// enforces the closed markdown/json/yaml allowlist. This is the external file
	// drag-and-drop ADD path (B-28). A rejection (disallowed format / malformed
	// base64) is surfaced as an ERROR frame and nothing is written.
	content, msg, _, ok := ingest.DecodeContent(p.Path, p.Content, p.ContentEncoding)
	if !ok {
		client.sendError(msg)
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
			client.sendResponse(MsgConflict, ConflictPayload{
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
