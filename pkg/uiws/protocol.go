package uiws

import (
	"encoding/json"

	"github.com/sopranoworks/shoka/pkg/authz"
)

// MessageType is the discriminator of a /ws/ui {type,payload} frame. The transport +
// core message types live here; a consumer (Shoka's document Manager, GitYard's
// Git/PR handlers) declares its own additional MessageType constants of this type.
type MessageType string

// WSMessage is the uniform /ws/ui envelope: a type plus an opaque payload.
type WSMessage struct {
	Type    MessageType     `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

const (
	// Error is the generic error frame carrying a human-readable message.
	Error MessageType = "ERROR"
	// MsgPermissionDenied is the authorization-refusal frame for the /ws/ui dispatch
	// gate (the B-28 stage-2 enforcement flip): a session principal whose scope lacks
	// the level the requested operation requires gets this instead of the handler
	// running. Distinct from ERROR so the client can surface a clear, non-fatal "you
	// do not have permission" toast (a read-only user attempting a write).
	MsgPermissionDenied MessageType = "PERMISSION_DENIED"

	// --- OAuth/MCP connection management (admin-only) ---
	//
	// MsgOAuthList enumerates the live OAuth/MCP connections (token series) the
	// built-in authorization server holds; MsgOAuthRevoke revokes one by series id.
	// NO SECRETS cross the wire (oauthstore.SeriesInfo only). Both are
	// ADMINISTRATOR-ONLY (the dispatch gate) and refused (MsgOAuthDenied) when OAuth
	// is not enabled (no store).
	MsgOAuthList   MessageType = "OAUTH_LIST"
	MsgOAuthRevoke MessageType = "OAUTH_REVOKE"
	// MsgOAuthIssueSelf mints a fresh access token for the current-mode operator (the
	// "token to self" path, B-46b §2.2) and returns it ONCE — the single deliberate
	// exception to "no secret crosses /ws/ui". Admin-gated; never logged or persisted
	// beyond the normal token store.
	MsgOAuthIssueSelf MessageType = "OAUTH_ISSUE_SELF"
	// MsgOAuthDenied is the typed refusal frame for the admin-only OAuth requests:
	// reason "forbidden" or "oauth_disabled". Distinct from ERROR so the client can
	// recognise an authorization refusal rather than a transport error.
	MsgOAuthDenied MessageType = "OAUTH_DENIED"

	// B-71 Stage 2d: domain-mode management — CRUD over the dynamic "domain"
	// registration store (trusted domain + per-domain TTL + per-domain consent),
	// admin-gated.
	MsgDomainList   MessageType = "DOMAIN_LIST"
	MsgDomainCreate MessageType = "DOMAIN_CREATE"
	MsgDomainUpdate MessageType = "DOMAIN_UPDATE"
	MsgDomainDelete MessageType = "DOMAIN_DELETE"
	// MsgDomainGenerateConsent mints (or re-rolls) a domain's per-domain consent value
	// and returns it PLAINTEXT (2026-06-20 model — operator-readable, refreshable).
	MsgDomainGenerateConsent MessageType = "DOMAIN_GENERATE_CONSENT"
	// B-71 Stage 3: confidential-mode management — issue / list / revoke pre-issued
	// client credentials (Client ID + Secret), admin-gated. The secret is shown ONCE
	// on issue, never returned by list.
	MsgClientIssue  MessageType = "CLIENT_ISSUE"
	MsgClientList   MessageType = "CLIENT_LIST"
	MsgClientRevoke MessageType = "CLIENT_REVOKE"

	// User-management ops (B-28 stage 3) — all super-user-only (admin level, global),
	// enforced by the dispatch gate; the destructive ones additionally refuse the
	// caller's own account (server-side self-guard, defence in depth).
	MsgAdminListUsers      MessageType = "ADMIN_LIST_USERS"
	MsgAdminSetUserScope   MessageType = "ADMIN_SET_USER_SCOPE"
	MsgAdminSetUserEnabled MessageType = "ADMIN_SET_USER_ENABLED"
	// MsgAdminSetUserPassword resets a target user's password (admin recovery for a
	// forgotten password — B-28 password recovery case 1). Admin-gated; argon2id
	// re-hash; on success the target's sessions are dropped and their OAuth revoked.
	MsgAdminSetUserPassword MessageType = "ADMIN_SET_USER_PASSWORD"
	MsgAdminRemoveUser      MessageType = "ADMIN_REMOVE_USER"
	MsgAdminCreateInvite    MessageType = "ADMIN_CREATE_INVITE"
	MsgAdminListInvites     MessageType = "ADMIN_LIST_INVITES"
	MsgAdminRevokeInvite    MessageType = "ADMIN_REVOKE_INVITE"

	// Self-service "My Account" ops (B-28) — reachable by ANY authenticated user for
	// THEIR OWN account (read-level, global — NOT admin-gated). Self-access is
	// STRUCTURAL: these act on the connection's session identity only and carry NO
	// target-id field. Email is the account id and has no setter.
	MsgAccountGet         MessageType = "ACCOUNT_GET"
	MsgAccountSetName     MessageType = "ACCOUNT_SET_NAME"
	MsgAccountSetPassword MessageType = "ACCOUNT_SET_PASSWORD"
)

// PermissionDeniedPayload is the body of a PERMISSION_DENIED frame: which operation
// was refused, the target namespace, the level it required, and a human reason.
type PermissionDeniedPayload struct {
	Op        string `json:"op"`
	Namespace string `json:"namespace,omitempty"`
	Required  string `json:"required"`
	Message   string `json:"message"`
}

// Op is a /ws/ui message's authorization requirement: the level it needs, and whether
// it is a GLOBAL op (no target namespace — its target is the whole server, so the gate
// ignores any payload namespace and uses the principal's max level anywhere).
type Op struct {
	Level  authz.Level
	Global bool
}

// CoreLevels maps each auth/user/OAuth core message to its required level — the core
// rows of the /ws/ui authorization table. Reads (the self-service ACCOUNT_* ops) need
// read; the admin management ops (ADMIN_*/OAUTH_*/DOMAIN_*/CLIENT_*) need admin. A
// consumer merges this with its own (document/Git/PR) rows and passes the union to
// Client.Gate. The core contributes NO super-user ops. The values here are
// byte-identical to the corresponding rows of the pre-extraction internal/ui.wsLevels.
var CoreLevels = map[MessageType]Op{
	MsgOAuthList:      {authz.LevelAdmin, true},
	MsgOAuthRevoke:    {authz.LevelAdmin, true},
	MsgOAuthIssueSelf: {authz.LevelAdmin, true},

	MsgDomainList:            {authz.LevelAdmin, true},
	MsgDomainCreate:          {authz.LevelAdmin, true},
	MsgDomainUpdate:          {authz.LevelAdmin, true},
	MsgDomainDelete:          {authz.LevelAdmin, true},
	MsgDomainGenerateConsent: {authz.LevelAdmin, true},
	MsgClientIssue:           {authz.LevelAdmin, true},
	MsgClientList:            {authz.LevelAdmin, true},
	MsgClientRevoke:          {authz.LevelAdmin, true},

	// Self-service account ops: read-level, GLOBAL — reachable by ANY authenticated
	// user (a global read check passes for any principal with read access anywhere).
	// NOT admin: self-access is structural (no target id), enforced in the handlers.
	MsgAccountGet:         {authz.LevelRead, true},
	MsgAccountSetName:     {authz.LevelRead, true},
	MsgAccountSetPassword: {authz.LevelRead, true},

	MsgAdminListUsers:       {authz.LevelAdmin, true},
	MsgAdminSetUserScope:    {authz.LevelAdmin, true},
	MsgAdminSetUserEnabled:  {authz.LevelAdmin, true},
	MsgAdminSetUserPassword: {authz.LevelAdmin, true},
	MsgAdminRemoveUser:      {authz.LevelAdmin, true},
	MsgAdminCreateInvite:    {authz.LevelAdmin, true},
	MsgAdminListInvites:     {authz.LevelAdmin, true},
	MsgAdminRevokeInvite:    {authz.LevelAdmin, true},
}

// target decodes the target namespace/project from a /ws/ui message payload (the
// uniform `namespace`/`projectName` keys every namespaced payload carries).
func target(payload json.RawMessage) (namespace, project string) {
	var t struct {
		Namespace   string `json:"namespace"`
		ProjectName string `json:"projectName"`
	}
	if len(payload) > 0 {
		_ = json.Unmarshal(payload, &t)
	}
	return t.Namespace, t.ProjectName
}

// Gate applies the shared authz decision to one /ws/ui message before its handler
// runs. It returns true to PROCEED and false when the message was refused (a
// PERMISSION_DENIED frame has been sent). The principal is the connection's session
// principal (stage 1); when absent — the no-lockout empty-store / single-operator path
// that RequireSession let through — the connection is treated as super-user (scope
// "*", via Client.Scope). This is the ONE /ws/ui enforcement site (not per-handler),
// calling the same authz.Authorize the MCP middleware uses.
//
// levels and superOps are supplied by the consumer (Shoka merges uiws.CoreLevels with
// its document rows; GitYard merges CoreLevels with its Git/PR rows), so the same gate
// serves both surfaces. superOps (the super-user-only ops, e.g. namespace management)
// are checked FIRST, via the strict IsSuperUser predicate — never the namespace-targeted
// Authorize a namespace-admin would satisfy for its own namespace. A message absent from
// levels fails CLOSED at admin (global).
func (c *Client) Gate(msgType MessageType, payload json.RawMessage, levels map[MessageType]Op, superOps map[MessageType]bool) bool {
	scope := c.Scope()
	if superOps[msgType] {
		if !authz.IsSuperUser(scope) {
			c.SendResponse(MsgPermissionDenied, PermissionDeniedPayload{
				Op:       string(msgType),
				Required: "super-user",
				Message:  "permission denied: namespace management requires a super-user",
			})
			return false
		}
		return true
	}
	op, known := levels[msgType]
	if !known {
		op = Op{Level: authz.LevelAdmin, Global: true} // fail closed
	}
	var ns, proj string
	if !op.Global {
		ns, proj = target(payload)
	}
	if err := authz.Authorize(scope, ns, proj, op.Level); err != nil {
		c.SendResponse(MsgPermissionDenied, PermissionDeniedPayload{
			Op:        string(msgType),
			Namespace: ns,
			Required:  op.Level.String(),
			Message:   "permission denied: " + err.Error(),
		})
		return false
	}
	return true
}
