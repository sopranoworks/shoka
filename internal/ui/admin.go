package ui

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sopranoworks/shoka/internal/storage/userstore"
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

func (m *Manager) usersAvailable(client *wsClient) bool {
	if m.users == nil {
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

func (m *Manager) handleAdminListUsers(client *wsClient) {
	if !m.usersAvailable(client) {
		return
	}
	users, err := m.users.ListUsers()
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

func (m *Manager) handleAdminSetUserScope(client *wsClient, payload json.RawMessage) {
	if !m.usersAvailable(client) {
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
	if err := m.users.UpdateUserScope(p.Email, p.Scope); err != nil {
		client.sendError(fmt.Sprintf("Failed to update scope: %v", err))
		return
	}
	client.sendResponse(MsgAdminSetUserScope, AdminAckPayload{Status: "ok"})
}

func (m *Manager) handleAdminRemoveUser(client *wsClient, payload json.RawMessage) {
	if !m.usersAvailable(client) {
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
	if err := m.users.RemoveUser(p.Email); err != nil {
		client.sendError(fmt.Sprintf("Failed to remove user: %v", err))
		return
	}
	client.sendResponse(MsgAdminRemoveUser, AdminAckPayload{Status: "ok"})
}

func (m *Manager) handleAdminCreateInvite(client *wsClient, payload json.RawMessage) {
	if !m.usersAvailable(client) {
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
	code, rec, err := m.users.CreateInvite(p.Email, p.Scope, client.selfEmail(), time.Now(), ttl)
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to create invite: %v", err))
		return
	}
	client.sendResponse(MsgAdminCreateInvite, AdminInviteCreatedPayload{
		Code: code, Email: rec.Email, Scope: rec.Scope, Expiry: rec.Expiry, CodeHash: rec.CodeHash,
	})
}

func (m *Manager) handleAdminListInvites(client *wsClient) {
	if !m.usersAvailable(client) {
		return
	}
	invs, err := m.users.ListInvites()
	if err != nil {
		client.sendError(fmt.Sprintf("Failed to list invites: %v", err))
		return
	}
	client.sendResponse(MsgAdminListInvites, AdminInvitesPayload{Invites: invs})
}

func (m *Manager) handleAdminRevokeInvite(client *wsClient, payload json.RawMessage) {
	if !m.usersAvailable(client) {
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
	if err := m.users.RevokeInvite(p.CodeHash); err != nil {
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
