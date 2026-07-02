package uiws

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/sopranoworks/shoka/pkg/userstore"
)

// Self-service "My Account" /ws/ui handlers (B-28). Unlike the ADMIN_* ops these are
// NOT admin-gated — the dispatch gate admits any authenticated user (read-level,
// global). Authorization that the caller may touch ONLY their own account is
// STRUCTURAL, not a checkable-and-forgettable guard: every handler acts on the
// connection's session identity (client.selfEmail()) and the request structs carry
// NO target email/id, so a caller cannot name another account. Email is the account
// id — it is returned for display but has no setter.

// AccountSetNameRequest changes the acting user's display name. It deliberately has
// NO email field: the target is always the session user.
type AccountSetNameRequest struct {
	DisplayName string `json:"display_name"`
}

// AccountSetPasswordRequest resets the acting user's password. It requires the
// current password (defence against a hijacked session) and carries NO email field:
// the target is always the session user.
type AccountSetPasswordRequest struct {
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
func (h *CoreHandlers) selfRecord(client *Client) (*userstore.UserRecord, bool) {
	if !h.usersAvailable(client) {
		return nil, false
	}
	email := client.selfEmail()
	if email == "" {
		client.SendError("you are not signed in")
		return nil, false
	}
	rec, err := h.users.GetUser(email)
	if err != nil {
		client.SendError("your account could not be loaded")
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
func (h *CoreHandlers) handleAccountGet(client *Client) {
	rec, ok := h.selfRecord(client)
	if !ok {
		return
	}
	client.SendResponse(MsgAccountGet, accountInfo(rec))
}

// handleAccountSetName changes the acting user's display name (non-empty). It acts on
// the session identity only — the payload has no target email.
func (h *CoreHandlers) handleAccountSetName(client *Client, payload json.RawMessage) {
	rec, ok := h.selfRecord(client)
	if !ok {
		return
	}
	var p AccountSetNameRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for ACCOUNT_SET_NAME")
		return
	}
	name := strings.TrimSpace(p.DisplayName)
	if name == "" {
		client.SendError("display name must not be empty")
		return
	}
	rec.DisplayName = name
	if err := h.users.PutUser(rec); err != nil {
		client.SendError("could not save your name")
		return
	}
	client.SendResponse(MsgAccountSetName, accountInfo(rec))
}

// --- TOTP self-service ---

// AccountTOTPEnrollResponse carries the secret + otpauth URI the client renders.
type AccountTOTPEnrollResponse struct {
	Secret     string `json:"secret"`
	OtpauthURL string `json:"otpauth_url"`
}

// AccountTOTPVerifyRequest proves the user's authenticator has the secret enrolled.
type AccountTOTPVerifyRequest struct {
	TOTPSecret string `json:"totp_secret"`
	TOTPCode   string `json:"totp_code"`
}

// handleAccountTOTPEnroll generates a fresh TOTP secret + otpauth URI for the acting
// user. Side-effect-free: the secret is only persisted after ACCOUNT_TOTP_VERIFY proves
// a current code against it.
func (h *CoreHandlers) handleAccountTOTPEnroll(client *Client) {
	rec, ok := h.selfRecord(client)
	if !ok {
		return
	}
	key, err := userstore.GenerateTOTP(h.rpName, rec.Email)
	if err != nil {
		client.SendError("could not generate TOTP secret")
		return
	}
	client.SendResponse(MsgAccountTOTPEnroll, AccountTOTPEnrollResponse{
		Secret:     key.Secret(),
		OtpauthURL: key.String(),
	})
}

// handleAccountTOTPVerify enrolls TOTP on the acting user's account: it seals the
// provided secret, verifies the code against it, and persists the encrypted secret.
func (h *CoreHandlers) handleAccountTOTPVerify(client *Client, payload json.RawMessage) {
	rec, ok := h.selfRecord(client)
	if !ok {
		return
	}
	var p AccountTOTPVerifyRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("invalid payload for ACCOUNT_TOTP_VERIFY")
		return
	}
	if p.TOTPSecret == "" || p.TOTPCode == "" {
		client.SendError("secret and code are required")
		return
	}
	enc, err := h.users.SealTOTPSecret(p.TOTPSecret)
	if err != nil {
		client.SendError("could not process TOTP secret")
		return
	}
	probe := &userstore.UserRecord{TOTPSecretEnc: enc}
	valid, err := h.users.VerifyTOTP(probe, p.TOTPCode, time.Now())
	if err != nil || !valid {
		client.SendError("invalid TOTP code")
		return
	}
	rec.TOTPSecretEnc = enc
	if err := h.users.PutUser(rec); err != nil {
		client.SendError("could not save TOTP enrollment")
		return
	}
	client.SendResponse(MsgAccountTOTPVerify, accountInfo(rec))
}

// handleAccountTOTPDisable removes TOTP from the acting user's account.
func (h *CoreHandlers) handleAccountTOTPDisable(client *Client) {
	rec, ok := h.selfRecord(client)
	if !ok {
		return
	}
	rec.TOTPSecretEnc = nil
	if err := h.users.PutUser(rec); err != nil {
		client.SendError("could not disable TOTP")
		return
	}
	client.SendResponse(MsgAccountTOTPDisable, accountInfo(rec))
}

// handleAccountSetPassword resets the acting user's password: it verifies the CURRENT
// password, enforces the password policy on the new one, re-hashes with argon2id and
// persists. It acts on the session identity only (no target email). The current
// session stays valid; other sessions are deliberately NOT invalidated (operator
// floor). No password value is ever logged.
func (h *CoreHandlers) handleAccountSetPassword(client *Client, payload json.RawMessage) {
	rec, ok := h.selfRecord(client)
	if !ok {
		return
	}
	var p AccountSetPasswordRequest
	if err := json.Unmarshal(payload, &p); err != nil {
		client.SendError("Invalid payload for ACCOUNT_SET_PASSWORD")
		return
	}
	matched, err := userstore.VerifyPassword(p.CurrentPassword, rec.PasswordHash)
	if err != nil || !matched {
		client.SendError("current password is incorrect")
		return
	}
	if err := userstore.ValidatePassword(p.NewPassword); err != nil {
		client.SendError(err.Error())
		return
	}
	hash, err := userstore.HashPassword(p.NewPassword)
	if err != nil {
		client.SendError("could not hash the new password")
		return
	}
	rec.PasswordHash = hash
	if err := h.users.PutUser(rec); err != nil {
		client.SendError("could not save your new password")
		return
	}
	client.SendResponse(MsgAccountSetPassword, AdminAckPayload{Status: "ok"})
}
