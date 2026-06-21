package ui

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/sopranoworks/shoka/pkg/userstore"
)

// User-management /ws/ui handlers (B-28 stage 3). Authorization (super-user only) is
// enforced upstream by the single dispatch gate (these messages are admin-level in
// wsLevels) — the handlers do NOT re-check the level. The DESTRUCTIVE ops add a
// server-side SELF-GUARD (refuse acting on the caller's own account) as defence in
// depth, even though the management UI never shows self.

// defaultInviteTTL is the lifetime of a generated invite code when the request does
// not specify one.
const defaultInviteTTL = 72 * time.Hour

type adminSetScopeRequest struct {
	Email string `json:"email"`
	Scope string `json:"scope"`
}

type adminRemoveUserRequest struct {
	Email string `json:"email"`
}

type adminSetEnabledRequest struct {
	Email   string `json:"email"`
	Enabled bool   `json:"enabled"`
}

type adminSetPasswordRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type adminCreateInviteRequest struct {
	Email    string `json:"email"`
	Scope    string `json:"scope"`
	TTLHours int    `json:"ttl_hours,omitempty"`
}

type adminRevokeInviteRequest struct {
	CodeHash string `json:"code_hash"`
}

// AdminUsersPayload is the user-list response (self omitted).
type AdminUsersPayload struct {
	Users []userstore.UserInfo `json:"users"`
}

// AdminInvitesPayload is the pending-invites response.
type AdminInvitesPayload struct {
	Invites []userstore.InviteInfo `json:"invites"`
}

// AdminInviteCreatedPayload carries the freshly-minted invite code — shown ONCE to
// the admin to convey out-of-band (Shoka sends no email).
type AdminInviteCreatedPayload struct {
	Code     string    `json:"code"`
	Email    string    `json:"email"`
	Scope    string    `json:"scope"`
	Expiry   time.Time `json:"expiry"`
	CodeHash string    `json:"code_hash"`
}

// AdminAckPayload is a generic ok ack for a mutating admin op.
type AdminAckPayload struct {
	Status string `json:"status"`
}

func (h *CoreHandlers) usersAvailable(client *wsClient) bool {
	if h.users == nil {
		client.sendError("user management is not available on this server")
		return false
	}
	return true
}

// selfEmail returns the connection's own normalized email (empty when no principal).
func (c *wsClient) selfEmail() string {
	if !c.hasPrincipal {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(c.principal.Email))
}

func (h *CoreHandlers) handleAdminListUsers(client *wsClient) {
	if !h.usersAvailable(client) {
		return
	}
	users, err := h.users.ListUsers()
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to list users: %v", err))
		return
	}
	// Omit SELF so self-deletion/demotion is structurally impossible from the UI.
	self := client.selfEmail()
	out := make([]userstore.UserInfo, 0, len(users))
	for _, u := range users {
		if self != "" && strings.EqualFold(u.Email, self) {
			continue
		}
		out = append(out, u)
	}
	client.sendResponse(MsgAdminListUsers, AdminUsersPayload{Users: out})
}

func (h *CoreHandlers) handleAdminSetUserScope(client *wsClient, payload json.RawMessage) {
	if !h.usersAvailable(client) {
		return
	}
	var p adminSetScopeRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for ADMIN_SET_USER_SCOPE")
		return
	}
	if p.Email == "" {
		client.sendError("ADMIN_SET_USER_SCOPE requires an email")
		return
	}
	if client.isSelf(p.Email) {
		client.sendError("you cannot change your own permissions")
		return
	}
	if err := h.users.UpdateUserScope(p.Email, p.Scope); err != nil {
		client.sendError(fmt.Sprintf("Failed to update scope: %v", err))
		return
	}
	client.sendResponse(MsgAdminSetUserScope, AdminAckPayload{Status: "ok"})
}

// handleAdminSetUserEnabled enables or disables an account. Disabling locks the user
// out immediately: SetUserDisabled drops their live UI sessions in its write, and we
// additionally revoke their OAuth/MCP token series here (cross-store) so a live token
// cannot outlive the disable. The isSelf self-guard refuses disabling one's own account.
func (h *CoreHandlers) handleAdminSetUserEnabled(client *wsClient, payload json.RawMessage) {
	if !h.usersAvailable(client) {
		return
	}
	var p adminSetEnabledRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for ADMIN_SET_USER_ENABLED")
		return
	}
	if p.Email == "" {
		client.sendError("ADMIN_SET_USER_ENABLED requires an email")
		return
	}
	if client.isSelf(p.Email) {
		client.sendError("you cannot disable your own account")
		return
	}
	if err := h.users.SetUserDisabled(p.Email, !p.Enabled); err != nil {
		client.sendError(fmt.Sprintf("Failed to update account state: %v", err))
		return
	}
	if !p.Enabled {
		h.revokeOAuthForUser(p.Email)
	}
	client.sendResponse(MsgAdminSetUserEnabled, AdminAckPayload{Status: "ok"})
}

// revokeOAuthForUser revokes every OAuth/MCP token series bound to email when an OAuth
// store is wired (nil = OAuth disabled — nothing to revoke). Best-effort: the user's
// lockout already holds via the login gate + dropped sessions, so a rare revoke error is
// logged, not surfaced to the caller (the disable/delete itself succeeded).
func (h *CoreHandlers) revokeOAuthForUser(email string) {
	if h.oauth == nil {
		return
	}
	if _, err := h.oauth.RevokeByPrincipalEmail(email); err != nil {
		log.Printf("oauth revoke for disabled/removed user failed: %v", err)
	}
}

// handleAdminSetUserPassword resets a target user's password (B-28 password recovery
// case 1: an admin recovers any user who forgot, EXCEPT a locked-out sole admin — that
// is the server startup-flag's job). Admin-gated by the dispatch gate. It re-hashes the
// new password (argon2id, shared ValidatePassword policy) and, via SetUserPassword,
// drops the target's sessions in the same tx; it then revokes their OAuth (cross-store),
// so the user must re-login with the new password. NO isSelf refusal — an admin may
// reset their own password — but the existing password hash is never returned.
func (h *CoreHandlers) handleAdminSetUserPassword(client *wsClient, payload json.RawMessage) {
	if !h.usersAvailable(client) {
		return
	}
	var p adminSetPasswordRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for ADMIN_SET_USER_PASSWORD")
		return
	}
	if p.Email == "" {
		client.sendError("ADMIN_SET_USER_PASSWORD requires an email")
		return
	}
	if err := userstore.ValidatePassword(p.Password); err != nil {
		client.sendError(err.Error())
		return
	}
	hash, err := userstore.HashPassword(p.Password)
	if err != nil {
		client.sendError("could not hash the new password")
		return
	}
	if err := h.users.SetUserPassword(p.Email, hash); err != nil {
		client.sendError(fmt.Sprintf("Failed to reset password: %v", err))
		return
	}
	// Cross-store: revoke the user's OAuth/MCP token series so a reset also cuts any
	// live token (mirrors the disable/remove cascade).
	h.revokeOAuthForUser(p.Email)
	client.sendResponse(MsgAdminSetUserPassword, AdminAckPayload{Status: "ok"})
}

func (h *CoreHandlers) handleAdminRemoveUser(client *wsClient, payload json.RawMessage) {
	if !h.usersAvailable(client) {
		return
	}
	var p adminRemoveUserRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for ADMIN_REMOVE_USER")
		return
	}
	if p.Email == "" {
		client.sendError("ADMIN_REMOVE_USER requires an email")
		return
	}
	if client.isSelf(p.Email) {
		client.sendError("you cannot remove your own account")
		return
	}
	if err := h.users.RemoveUser(p.Email); err != nil {
		client.sendError(fmt.Sprintf("Failed to remove user: %v", err))
		return
	}
	// RemoveUser cascades the user's sessions; close the cross-store gap by revoking
	// their OAuth/MCP token series too, so a removed user's MCP access stops at once.
	h.revokeOAuthForUser(p.Email)
	client.sendResponse(MsgAdminRemoveUser, AdminAckPayload{Status: "ok"})
}

func (h *CoreHandlers) handleAdminCreateInvite(client *wsClient, payload json.RawMessage) {
	if !h.usersAvailable(client) {
		return
	}
	var p adminCreateInviteRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for ADMIN_CREATE_INVITE")
		return
	}
	if p.Email == "" {
		client.sendError("ADMIN_CREATE_INVITE requires an email")
		return
	}
	ttl := defaultInviteTTL
	if p.TTLHours > 0 {
		ttl = time.Duration(p.TTLHours) * time.Hour
	}
	code, rec, err := h.users.CreateInvite(p.Email, p.Scope, client.selfEmail(), time.Now(), ttl)
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to create invite: %v", err))
		return
	}
	client.sendResponse(MsgAdminCreateInvite, AdminInviteCreatedPayload{
		Code: code, Email: rec.Email, Scope: rec.Scope, Expiry: rec.Expiry, CodeHash: rec.CodeHash,
	})
}

func (h *CoreHandlers) handleAdminListInvites(client *wsClient) {
	if !h.usersAvailable(client) {
		return
	}
	invs, err := h.users.ListInvites()
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to list invites: %v", err))
		return
	}
	client.sendResponse(MsgAdminListInvites, AdminInvitesPayload{Invites: invs})
}

func (h *CoreHandlers) handleAdminRevokeInvite(client *wsClient, payload json.RawMessage) {
	if !h.usersAvailable(client) {
		return
	}
	var p adminRevokeInviteRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for ADMIN_REVOKE_INVITE")
		return
	}
	if p.CodeHash == "" {
		client.sendError("ADMIN_REVOKE_INVITE requires a code_hash")
		return
	}
	if err := h.users.RevokeInvite(p.CodeHash); err != nil {
		client.sendError(fmt.Sprintf("Failed to revoke invite: %v", err))
		return
	}
	client.sendResponse(MsgAdminRevokeInvite, AdminAckPayload{Status: "ok"})
}

// isSelf reports whether email is the connection's own account (the self-guard).
func (c *wsClient) isSelf(email string) bool {
	self := c.selfEmail()
	return self != "" && strings.EqualFold(strings.TrimSpace(email), self)
}
