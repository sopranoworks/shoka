package ui

import (
	"encoding/json"

	"github.com/sopranoworks/shoka/pkg/authz"
)

// CoreHandlers is the reusable auth/user/OAuth slice of the /ws/ui surface (the
// 2026-06-21 GitYard core-handler extraction, step (b) of the core-extraction line).
//
// It holds ONLY the user + OAuth stores — NO document storage.StorageService, no
// drafts/ingest/identity/notify. Its methods are the ACCOUNT_* (My Account), ADMIN_*
// (user management), and OAUTH_*/DOMAIN_*/CLIENT_* (OAuth/domain/confidential)
// handlers, plus the shared authzGate. Shoka's *Manager EMBEDS a *CoreHandlers, so
// every existing call (the dispatch switch, SetUserStore/SetOAuthStore, the tests,
// cmd/shoka) reaches these methods unchanged via Go method/field promotion — Shoka's
// runtime behaviour is identical. A SECOND program (GitYard, a feature-reduced Shoka
// with no document store) can construct a CoreHandlers with just the two stores and
// mount these handlers on its OWN ws manager, without supplying a StorageService.
//
// The handler BODIES were already document-store-free (they call only the user +
// OAuth stores); this type makes that independence structural rather than incidental,
// so the eventual physical package move (step (a)) can lift the slice cleanly.
type CoreHandlers struct {
	// oauth is the OAuth connection store for the admin management requests. It is
	// nil when OAuth is disabled (set via SetOAuthStore inside the oauth-enabled
	// wiring), in which case OAUTH_LIST/OAUTH_REVOKE return MsgOAuthDenied with
	// reason "oauth_disabled" rather than nil-panicking.
	oauth OAuthConnectionStore
	// selfIssuer mints the operator's "token to self" (B-46b §2.2). nil when OAuth
	// is disabled (wired via SetOAuthSelfIssuer in the oauth-enabled startup path),
	// in which case OAUTH_ISSUE_SELF returns MsgOAuthDenied ("oauth_disabled").
	selfIssuer OAuthSelfIssuer
	// users backs the super-user-only user-management ops (B-28 stage 3) and the
	// self-service My Account ops. nil when no user store is wired (the ADMIN_*/
	// ACCOUNT_* handlers then report it unavailable).
	users UserAdminStore
}

// SetOAuthStore wires the OAuth connection store for the admin management
// requests. Called only in the oauth-enabled startup path; when unset the store
// is nil and OAUTH_LIST/OAUTH_REVOKE return MsgOAuthDenied ("oauth_disabled").
func (h *CoreHandlers) SetOAuthStore(s OAuthConnectionStore) {
	h.oauth = s
}

// SetOAuthSelfIssuer wires the token-to-self minter for OAUTH_ISSUE_SELF. Called
// only in the oauth-enabled startup path; when unset the issuer is nil and
// OAUTH_ISSUE_SELF returns MsgOAuthDenied ("oauth_disabled").
func (h *CoreHandlers) SetOAuthSelfIssuer(i OAuthSelfIssuer) {
	h.selfIssuer = i
}

// SetUserStore wires the user store for the super-user-only user-management ops
// (B-28 stage 3). Called in startup; when unset the ADMIN_* handlers report the
// capability unavailable.
func (h *CoreHandlers) SetUserStore(u UserAdminStore) {
	h.users = u
}

// authzGate applies the shared authz decision to one /ws/ui message before its
// handler runs. It returns true to PROCEED and false when the message was refused
// (a PERMISSION_DENIED frame has been sent). The principal is the connection's session
// principal (stage 1); when absent — the no-lockout empty-store / single-operator path
// that RequireSession let through — the connection is treated as super-user. This is
// the ONE /ws/ui enforcement site (not per-handler), calling the same authz.Authorize
// the MCP middleware uses.
//
// It lives on CoreHandlers (it touches no Manager state) so both the core slice and
// the remaining file/ns-proj handlers gate through the same decision: Shoka's *Manager
// reaches it by promotion (the dispatch loop calls m.authzGate for EVERY message), and
// a second program gates its core ops through the holder directly.
func (h *CoreHandlers) authzGate(client *wsClient, msgType MessageType, payload json.RawMessage) bool {
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
