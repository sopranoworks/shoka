package authapi

import (
	"net/http"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/sopranoworks/shoka/pkg/userstore"
)

// ceremonyTTL bounds how long a begun WebAuthn ceremony may stay pending before its
// SessionData is discarded. Ceremonies are interactive and complete in seconds.
const ceremonyTTL = 5 * time.Minute

// ceremony holds the in-flight WebAuthn SessionData for one begin/finish pair, keyed
// by the ceremony cookie. Held in memory (not the store): it is ephemeral, and a
// lost ceremony (restart) simply fails and is retried.
type ceremony struct {
	email  string
	data   webauthn.SessionData
	expiry time.Time
}

type ceremonyCache struct {
	mu sync.Mutex
	m  map[string]ceremony
}

func newCeremonyCache() *ceremonyCache { return &ceremonyCache{m: make(map[string]ceremony)} }

func (c *ceremonyCache) put(id, email string, data webauthn.SessionData) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	// Opportunistic GC of expired entries so the map cannot grow unbounded.
	for k, v := range c.m {
		if now.After(v.expiry) {
			delete(c.m, k)
		}
	}
	c.m[id] = ceremony{email: email, data: data, expiry: now.Add(ceremonyTTL)}
}

// take returns and removes the ceremony for id (single-use). ok is false if unknown
// or expired.
func (c *ceremonyCache) take(id string) (ceremony, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cer, ok := c.m[id]
	if !ok {
		return ceremony{}, false
	}
	delete(c.m, id)
	if time.Now().After(cer.expiry) {
		return ceremony{}, false
	}
	return cer, true
}

// loginBeginRequest names the account to assert a passkey for. WebAuthn login here
// is non-discoverable (the email is known), which works on every browser.
type loginBeginRequest struct {
	Email string `json:"email"`
}

// handleWebAuthnRegisterBegin starts a passkey enrolment for the LOGGED-IN user
// (passkey is added on top of the password floor). Returns the CredentialCreation
// options and sets the ceremony cookie.
func (h *Handler) handleWebAuthnRegisterBegin(w http.ResponseWriter, r *http.Request) {
	if h.wa == nil {
		writeError(w, http.StatusBadRequest, "passkeys are not enabled on this deployment")
		return
	}
	u := h.currentUser(r)
	if u == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	options, sessionData, err := h.wa.BeginRegistration(u)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not begin registration")
		return
	}
	h.storeCeremony(w, r, u.Email, *sessionData)
	writeJSON(w, http.StatusOK, options)
}

// handleWebAuthnRegisterFinish completes a passkey enrolment: it validates the
// attestation and appends the credential to the user.
func (h *Handler) handleWebAuthnRegisterFinish(w http.ResponseWriter, r *http.Request) {
	if h.wa == nil {
		writeError(w, http.StatusBadRequest, "passkeys are not enabled on this deployment")
		return
	}
	u := h.currentUser(r)
	if u == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	cer, ok := h.takeCeremony(r)
	if !ok || cer.email != u.Email {
		writeError(w, http.StatusBadRequest, "no matching registration ceremony")
		return
	}
	cred, err := h.wa.FinishRegistration(u, cer.data, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "passkey registration failed")
		return
	}
	u.Credentials = append(u.Credentials, *cred)
	if err := h.users.PutUser(u); err != nil {
		writeError(w, http.StatusInternalServerError, "could not save passkey")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "credentials": len(u.Credentials)})
}

// handleWebAuthnLoginBegin starts a passkey assertion for a named account. Returns
// the CredentialAssertion options and sets the ceremony cookie.
func (h *Handler) handleWebAuthnLoginBegin(w http.ResponseWriter, r *http.Request) {
	if h.wa == nil {
		writeError(w, http.StatusBadRequest, "passkeys are not enabled on this deployment")
		return
	}
	var req loginBeginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	u, err := h.users.GetUser(req.Email)
	if err != nil || len(u.WebAuthnCredentials()) == 0 {
		writeError(w, http.StatusUnauthorized, "no passkey for this account")
		return
	}
	options, sessionData, err := h.wa.BeginLogin(u)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not begin login")
		return
	}
	h.storeCeremony(w, r, u.Email, *sessionData)
	writeJSON(w, http.StatusOK, options)
}

// handleWebAuthnLoginFinish completes a passkey assertion: on success it mints a
// session, persists the updated credential (sign count), and returns the principal.
func (h *Handler) handleWebAuthnLoginFinish(w http.ResponseWriter, r *http.Request) {
	if h.wa == nil {
		writeError(w, http.StatusBadRequest, "passkeys are not enabled on this deployment")
		return
	}
	cer, ok := h.takeCeremony(r)
	if !ok {
		writeError(w, http.StatusBadRequest, "no matching login ceremony")
		return
	}
	u, err := h.users.GetUser(cer.email)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid login")
		return
	}
	cred, err := h.wa.FinishLogin(u, cer.data, r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "passkey assertion failed")
		return
	}
	// A disabled account is refused AFTER the assertion verifies and BEFORE any session
	// is minted (mirrors handleLogin) — the flag is inert without BOTH login gates.
	if u.Disabled {
		writeError(w, http.StatusForbidden, "this account has been disabled")
		return
	}
	// Persist the updated authenticator sign count (clone-detection hygiene).
	updateCredential(u, cred)
	_ = h.users.PutUser(u)
	h.startSession(w, r, u.Email)
	writeJSON(w, http.StatusOK, statusResponse{
		UsersExist:    true,
		Authenticated: true,
		Principal:     &principal{Email: u.Email, DisplayName: u.DisplayName, IsAdmin: u.IsAdmin()},
	})
}

// storeCeremony stashes the SessionData under a fresh ceremony id and sets the
// ceremony cookie so finish can correlate it.
func (h *Handler) storeCeremony(w http.ResponseWriter, r *http.Request, email string, data webauthn.SessionData) {
	id, err := userstore.NewHandle()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not start ceremony")
		return
	}
	h.ceremonies.put(id, email, data)
	http.SetCookie(w, &http.Cookie{
		Name:     ceremonyCookieName,
		Value:    id,
		Path:     "/auth",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(ceremonyTTL.Seconds()),
	})
}

func (h *Handler) takeCeremony(r *http.Request) (ceremony, bool) {
	c, err := r.Cookie(ceremonyCookieName)
	if err != nil || c.Value == "" {
		return ceremony{}, false
	}
	return h.ceremonies.take(c.Value)
}

// updateCredential replaces the stored credential matching cred.ID with the updated
// one (the post-assertion sign count), leaving the user's other passkeys untouched.
func updateCredential(u *userstore.UserRecord, cred *webauthn.Credential) {
	for i := range u.Credentials {
		if string(u.Credentials[i].ID) == string(cred.ID) {
			u.Credentials[i] = *cred
			return
		}
	}
}
