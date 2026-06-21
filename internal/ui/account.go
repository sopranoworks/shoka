package ui

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/sopranoworks/shoka/internal/storage/userstore"
)

// Self-service "My Account" /ws/ui handlers (B-28). Unlike the ADMIN_* ops these are
// NOT admin-gated — the dispatch gate admits any authenticated user (read-level,
// global). Authorization that the caller may touch ONLY their own account is
// STRUCTURAL, not a checkable-and-forgettable guard: every handler acts on the
// connection's session identity (client.selfEmail()) and the request structs carry
// NO target email/id, so a caller cannot name another account. Email is the account
// id — it is returned for display but has no setter.

// accountSetNameRequest changes the acting user's display name. It deliberately has
// NO email field: the target is always the session user.
type accountSetNameRequest struct {
	DisplayName string `json:"display_name"`
}

// accountSetPasswordRequest resets the acting user's password. It requires the
// current password (defence against a hijacked session) and carries NO email field:
// the target is always the session user.
type accountSetPasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// AccountInfoPayload is the ACCOUNT_GET / ACCOUNT_SET_NAME response: the acting
// user's OWN info. It never carries the password hash or the TOTP secret. Email is
// shown read-only (it is the account id). IsAdmin/Scope describe the role.
type AccountInfoPayload struct {
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	Scope       string    `json:"scope"`
	IsAdmin     bool      `json:"is_admin"`
	HasTOTP     bool      `json:"has_totp"`
	CreatedAt   time.Time `json:"created_at"`
}

// selfRecord resolves the connection's own user record, or sends an error and
// returns false. It enforces that a user store is wired AND the connection carries a
// session principal (a logged-in user). The no-lockout single-operator path has no
// account record, so "My Account" is only meaningful once signed in.
func (m *Manager) selfRecord(client *wsClient) (*userstore.UserRecord, bool) {
	if !m.usersAvailable(client) {
		return nil, false
	}
	email := client.selfEmail()
	if email == "" {
		client.sendError("you are not signed in")
		return nil, false
	}
	rec, err := m.users.GetUser(email)
	if err != nil {
		client.sendError("your account could not be loaded")
		return nil, false
	}
	return rec, true
}

func accountInfo(rec *userstore.UserRecord) AccountInfoPayload {
	return AccountInfoPayload{
		Email:       rec.Email,
		DisplayName: rec.DisplayName,
		Scope:       rec.Scope,
		IsAdmin:     rec.IsAdmin(),
		HasTOTP:     rec.HasTOTP(),
		CreatedAt:   rec.CreatedAt,
	}
}

// handleAccountGet returns the acting user's OWN account info (never a secret).
func (m *Manager) handleAccountGet(client *wsClient) {
	rec, ok := m.selfRecord(client)
	if !ok {
		return
	}
	client.sendResponse(MsgAccountGet, accountInfo(rec))
}

// handleAccountSetName changes the acting user's display name (non-empty). It acts on
// the session identity only — the payload has no target email.
func (m *Manager) handleAccountSetName(client *wsClient, payload json.RawMessage) {
	rec, ok := m.selfRecord(client)
	if !ok {
		return
	}
	var p accountSetNameRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for ACCOUNT_SET_NAME")
		return
	}
	name := strings.TrimSpace(p.DisplayName)
	if name == "" {
		client.sendError("display name must not be empty")
		return
	}
	rec.DisplayName = name
	if err := m.users.PutUser(rec); err != nil {
		client.sendError("could not save your name")
		return
	}
	client.sendResponse(MsgAccountSetName, accountInfo(rec))
}

// handleAccountSetPassword resets the acting user's password: it verifies the CURRENT
// password, enforces the password policy on the new one, re-hashes with argon2id and
// persists. It acts on the session identity only (no target email). The current
// session stays valid; other sessions are deliberately NOT invalidated (operator
// floor). No password value is ever logged.
func (m *Manager) handleAccountSetPassword(client *wsClient, payload json.RawMessage) {
	rec, ok := m.selfRecord(client)
	if !ok {
		return
	}
	var p accountSetPasswordRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.sendError("Invalid payload for ACCOUNT_SET_PASSWORD")
		return
	}
	matched, err := userstore.VerifyPassword(p.CurrentPassword, rec.PasswordHash)
	if err != nil || !matched {
		client.sendError("current password is incorrect")
		return
	}
	if err := userstore.ValidatePassword(p.NewPassword); err != nil {
		client.sendError(err.Error())
		return
	}
	hash, err := userstore.HashPassword(p.NewPassword)
	if err != nil {
		client.sendError("could not hash the new password")
		return
	}
	rec.PasswordHash = hash
	if err := m.users.PutUser(rec); err != nil {
		client.sendError("could not save your new password")
		return
	}
	client.sendResponse(MsgAccountSetPassword, AdminAckPayload{Status: "ok"})
}
