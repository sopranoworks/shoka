package authapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/sopranoworks/shoka/pkg/userstore"
)

// Invite redemption (B-28 stage 3): the unauthenticated flow off the login screen.
// An invitee enters a code, sees the fixed email/scope it grants, and sets up their
// own credentials (password floor + optional TOTP; a passkey is added after the
// session exists, via the authenticated WebAuthn enrolment, exactly as first-run).

type inviteInfoRequest struct {
	Code string `json:"code"`
}

type inviteInfoResponse struct {
	Email string `json:"email"`
	Scope string `json:"scope"`
}

type inviteRedeemRequest struct {
	Code        string `json:"code"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
	TOTPSecret  string `json:"totp_secret"`
	TOTPCode    string `json:"totp_code"`
}

// handleInviteInfo resolves a code to the email/scope it grants WITHOUT consuming it,
// so the redeem screen can show the invitee what they are accepting.
func (h *Handler) handleInviteInfo(w http.ResponseWriter, r *http.Request) {
	var req inviteInfoRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	info, err := h.users.InviteByCode(strings.TrimSpace(req.Code), time.Now())
	if err != nil {
		writeError(w, http.StatusBadRequest, "this invite is invalid, expired, or already used")
		return
	}
	writeJSON(w, http.StatusOK, inviteInfoResponse{Email: info.Email, Scope: info.Scope})
}

// handleInviteRedeem consumes a valid invite and creates the invitee's account with
// the invite's fixed email + scope and the credentials they supply (password
// required; optional TOTP proven by a current code). It is atomic single-use — a
// second redeem of the same code fails. On success it establishes the session.
func (h *Handler) handleInviteRedeem(w http.ResponseWriter, r *http.Request) {
	var req inviteRedeemRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, "password must be at least 8 characters")
		return
	}
	ph, err := userstore.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not hash password")
		return
	}
	rec := &userstore.UserRecord{
		DisplayName:  strings.TrimSpace(req.DisplayName),
		PasswordHash: ph,
	}
	if req.TOTPSecret != "" {
		enc, err := h.users.SealTOTPSecret(req.TOTPSecret)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not store TOTP secret")
			return
		}
		probe := &userstore.UserRecord{TOTPSecretEnc: enc}
		ok, err := h.users.VerifyTOTP(probe, req.TOTPCode, time.Now())
		if err != nil || !ok {
			writeError(w, http.StatusBadRequest, "TOTP code did not verify against the provided secret")
			return
		}
		rec.TOTPSecretEnc = enc
	}
	if err := h.users.RedeemInvite(strings.TrimSpace(req.Code), time.Now(), rec); err != nil {
		switch err {
		case userstore.ErrInvalidInvite:
			writeError(w, http.StatusBadRequest, "this invite is invalid, expired, or already used")
		case userstore.ErrExists:
			writeError(w, http.StatusConflict, "an account for this email already exists")
		default:
			writeError(w, http.StatusInternalServerError, "could not redeem invite")
		}
		return
	}
	// DisplayName defaults to the email when the invitee left it blank.
	if rec.DisplayName == "" {
		rec.DisplayName = rec.Email
		_ = h.users.PutUser(rec)
	}
	h.startSession(w, r, rec.Email)
	h.logger.Info("invite redeemed", "email", rec.Email)
	writeJSON(w, http.StatusOK, statusResponse{
		UsersExist:    true,
		Authenticated: true,
		Principal:     &principal{Email: rec.Email, DisplayName: rec.DisplayName, IsAdmin: rec.IsAdmin()},
	})
}
