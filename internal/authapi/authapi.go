// Package authapi is the WebUI multi-user login surface (B-28 stage 1): the HTTP
// /auth/* endpoints (status, first-run register, login, logout, and the WebAuthn
// passkey ceremonies) plus the session middleware that resolves a session cookie
// into an auth.Principal on the request context.
//
// It lives on the WEB surface, strictly separate from the MCP token surface (the
// B-50 phase-3 decoupling): it consults ONLY the user store and never the OAuth
// ValidateToken closure, and a session is a server-side opaque handle in an
// HttpOnly cookie — NOT an OAuth token, and an OAuth token is never accepted here.
//
// The principal it attaches is the SAME auth.Principal shape the MCP path uses, so
// a logged-in WebUI session and an MCP token both feed the one authorization model
// (the role-aware enforcement sweep is a later stage; stage 1 attaches the principal
// and gates access on a valid session).
package authapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/sopranoworks/shoka/internal/auth"
	"github.com/sopranoworks/shoka/internal/authz"
	"github.com/sopranoworks/shoka/internal/storage/userstore"
)

// sessionCookieName is the WebUI login session cookie. It carries an opaque
// server-side session handle, never any credential or token.
const sessionCookieName = "shoka_session"

// ceremonyCookieName correlates a WebAuthn begin/finish pair (the short-lived
// ceremony SessionData is held server-side, keyed by this cookie's value).
const ceremonyCookieName = "shoka_webauthn"

// Handler serves /auth/* and provides the session middleware. It depends only on
// the user store (authentication state) and an optional WebAuthn engine (nil when
// no rp_id is configured — passkeys disabled, password+TOTP floor still works).
type Handler struct {
	users      *userstore.Store
	wa         *webauthn.WebAuthn // nil = passkeys disabled
	rpName     string             // TOTP issuer label (the RP display name)
	sessionTTL time.Duration
	allowFirst bool // allow_first_run_admin
	ceremonies *ceremonyCache
	logger     *slog.Logger
}

// Config configures the Handler.
type Config struct {
	Users              *userstore.Store
	WebAuthn           *webauthn.WebAuthn
	RPDisplayName      string
	SessionTTL         time.Duration
	AllowFirstRunAdmin bool
	Logger             *slog.Logger
}

// New builds the auth Handler.
func New(c Config) *Handler {
	logger := c.Logger
	if logger == nil {
		logger = slog.Default()
	}
	name := c.RPDisplayName
	if name == "" {
		name = "Shoka"
	}
	ttl := c.SessionTTL
	if ttl <= 0 {
		ttl = 720 * time.Hour
	}
	return &Handler{
		users:      c.Users,
		wa:         c.WebAuthn,
		rpName:     name,
		sessionTTL: ttl,
		allowFirst: c.AllowFirstRunAdmin,
		ceremonies: newCeremonyCache(),
		logger:     logger,
	}
}

// ServeHTTP routes the /auth/* endpoints. Mount it at "/auth/".
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimPrefix(r.URL.Path, "/auth") {
	case "/status":
		h.handleStatus(w, r)
	case "/totp/new":
		h.requirePOST(w, r, h.handleTOTPNew)
	case "/invite/info":
		h.requirePOST(w, r, h.handleInviteInfo)
	case "/invite/redeem":
		h.requirePOST(w, r, h.handleInviteRedeem)
	case "/register":
		h.requirePOST(w, r, h.handleRegister)
	case "/login":
		h.requirePOST(w, r, h.handleLogin)
	case "/logout":
		h.requirePOST(w, r, h.handleLogout)
	case "/webauthn/register/begin":
		h.requirePOST(w, r, h.handleWebAuthnRegisterBegin)
	case "/webauthn/register/finish":
		h.requirePOST(w, r, h.handleWebAuthnRegisterFinish)
	case "/webauthn/login/begin":
		h.requirePOST(w, r, h.handleWebAuthnLoginBegin)
	case "/webauthn/login/finish":
		h.requirePOST(w, r, h.handleWebAuthnLoginFinish)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) requirePOST(w http.ResponseWriter, r *http.Request, fn func(http.ResponseWriter, *http.Request)) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	fn(w, r)
}

// --- DTOs --------------------------------------------------------------------

type statusResponse struct {
	UsersExist      bool       `json:"users_exist"`
	Authenticated   bool       `json:"authenticated"`
	FirstRunAllowed bool       `json:"first_run_allowed"`
	PasskeyEnabled  bool       `json:"passkey_enabled"`
	Principal       *principal `json:"principal,omitempty"`
	// ManagesAnyNamespace is the server-derived predicate the B-28 part-2 "Namespace /
	// project management" Settings item is shown for (super-user OR a namespace-admin of ≥1
	// namespace). It is authoritative for the authenticated case; the no-lockout empty-store
	// operator is handled client-side (useIsSuperUser via !users_exist), so the item's full
	// visibility predicate is useIsSuperUser() || manages_any_namespace.
	ManagesAnyNamespace bool `json:"manages_any_namespace"`
}

type principal struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	IsAdmin     bool   `json:"is_admin"`
}

type registerRequest struct {
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Password    string `json:"password"`
	TOTPSecret  string `json:"totp_secret"` // optional; base32, the secret the client enrolled
	TOTPCode    string `json:"totp_code"`   // required iff totp_secret is set — proves the authenticator works
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	TOTPCode string `json:"totp_code"`
}

type totpNewRequest struct {
	Email string `json:"email"`
}

type totpNewResponse struct {
	Secret     string `json:"secret"`
	OtpauthURL string `json:"otpauth_url"`
}

// --- handlers ----------------------------------------------------------------

// handleTOTPNew generates a fresh TOTP secret + otpauth:// provisioning URI for the
// first-run wizard's optional 2FA enrolment. It stores nothing — the secret is only
// persisted by /auth/register once the client proves a current code against it, so
// this generator is side-effect-free.
func (h *Handler) handleTOTPNew(w http.ResponseWriter, r *http.Request) {
	var req totpNewRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Email == "" {
		req.Email = "user"
	}
	key, err := userstore.GenerateTOTP(h.rpName, req.Email)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not generate TOTP secret")
		return
	}
	writeJSON(w, http.StatusOK, totpNewResponse{Secret: key.Secret(), OtpauthURL: key.String()})
}

// handleStatus reports the boot state the SPA branches on: whether any user exists
// (first-run vs login), whether THIS request is authenticated, and the deployment's
// first-run/passkey posture. It reads the session cookie but never requires it.
func (h *Handler) handleStatus(w http.ResponseWriter, r *http.Request) {
	empty, err := h.users.IsEmpty()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "user store unavailable")
		return
	}
	resp := statusResponse{
		UsersExist:      !empty,
		FirstRunAllowed: h.allowFirst,
		PasskeyEnabled:  h.wa != nil,
	}
	if u := h.currentUser(r); u != nil {
		resp.Authenticated = true
		resp.Principal = &principal{Email: u.Email, DisplayName: u.DisplayName, IsAdmin: u.IsAdmin()}
		// "Manages any namespace": super-user, or a namespace-admin of ≥1 namespace (B-28
		// part 2 — the ns/proj-management item's visibility, beyond the super-user-only items).
		adminNs, superUser := authz.AdminNamespaces(u.Scope)
		resp.ManagesAnyNamespace = superUser || len(adminNs) > 0
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRegister is the zero-config first-run wizard: with an empty user store (and
// allow_first_run_admin true), the first registrant becomes the wildcard admin. It
// is refused once any user exists (409) or when first-run is disabled (403). The
// password is required (the universal floor); a TOTP secret, if provided, must be
// proven with a current code before it is enrolled.
func (h *Handler) handleRegister(w http.ResponseWriter, r *http.Request) {
	if !h.allowFirst {
		writeError(w, http.StatusForbidden, "first-run registration is disabled on this deployment")
		return
	}
	empty, err := h.users.IsEmpty()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "user store unavailable")
		return
	}
	if !empty {
		writeError(w, http.StatusConflict, "a user already exists; registration is by invitation only")
		return
	}
	var req registerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Email = strings.TrimSpace(req.Email)
	if req.Email == "" || !strings.Contains(req.Email, "@") {
		writeError(w, http.StatusBadRequest, "a valid email is required")
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
		Email:        req.Email,
		DisplayName:  strings.TrimSpace(req.DisplayName),
		PasswordHash: ph,
	}
	// Optional TOTP enrolment: require a valid current code against the supplied
	// secret so we never store a secret the user cannot actually generate codes for.
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
	if err := h.users.CreateFirstAdmin(rec); err != nil {
		if err == userstore.ErrUsersExist {
			writeError(w, http.StatusConflict, "a user already exists; registration is by invitation only")
			return
		}
		writeError(w, http.StatusInternalServerError, "could not create user")
		return
	}
	h.startSession(w, r, rec.Email)
	h.logger.Info("first-run admin registered", "email", rec.Email)
	writeJSON(w, http.StatusOK, statusResponse{
		UsersExist:    true,
		Authenticated: true,
		Principal:     &principal{Email: rec.Email, DisplayName: rec.DisplayName, IsAdmin: true},
	})
}

// handleLogin verifies password (+ TOTP when enrolled) and mints a session. It does
// not distinguish unknown-email from wrong-password in its message (no user
// enumeration), but it DOES signal when a TOTP code is required so the SPA can
// prompt for it.
func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	u, err := h.users.GetUser(req.Email)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	ok, err := userstore.VerifyPassword(req.Password, u.PasswordHash)
	if err != nil || !ok {
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	if u.HasTOTP() {
		if req.TOTPCode == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "totp required", "totp_required": true})
			return
		}
		ok, err := h.users.VerifyTOTP(u, req.TOTPCode, time.Now())
		if err != nil || !ok {
			writeError(w, http.StatusUnauthorized, "invalid TOTP code")
			return
		}
	}
	// A disabled account is refused AFTER credential verification (so this never leaks
	// account existence to an unauthenticated caller) and BEFORE any session is minted.
	if u.Disabled {
		writeError(w, http.StatusForbidden, "this account has been disabled")
		return
	}
	h.startSession(w, r, u.Email)
	writeJSON(w, http.StatusOK, statusResponse{
		UsersExist:    true,
		Authenticated: true,
		Principal:     &principal{Email: u.Email, DisplayName: u.DisplayName, IsAdmin: u.IsAdmin()},
	})
}

// handleLogout deletes the session and clears the cookie.
func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
		_ = h.users.DeleteSession(c.Value)
	}
	h.clearSessionCookie(w, r)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- session cookie helpers --------------------------------------------------

func (h *Handler) startSession(w http.ResponseWriter, r *http.Request, email string) {
	sess, err := h.users.CreateSession(email, time.Now(), h.sessionTTL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not create session")
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sess.ID,
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  sess.Expiry,
	})
}

func (h *Handler) clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// currentUser resolves the session cookie to a user, or nil. The look-up sweeps an
// expired session (LookupSession deletes it), so a stale cookie resolves to nil.
func (h *Handler) currentUser(r *http.Request) *userstore.UserRecord {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	sess, err := h.users.LookupSession(c.Value, time.Now())
	if err != nil {
		return nil
	}
	u, err := h.users.GetUser(sess.Email)
	if err != nil {
		return nil
	}
	return u
}

// isHTTPS reports whether the request reached Shoka over TLS, directly or via a
// terminating reverse proxy. Drives the cookie Secure flag (set on HTTPS; omitted
// on a bare-IP HTTP deployment where the link is otherwise protected).
func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// --- session middleware (principal attach + gate) ----------------------------

// Middleware resolves the session cookie into an auth.Principal on the request
// context (the SAME shape the MCP path uses), for EVERY web request. It never
// blocks — the gate is RequireSession, applied only where a session is mandatory.
// It consults only the user store: an OAuth bearer token is irrelevant here, which
// is what keeps the WebUI login surface separate from the MCP token surface (B-50).
func (h *Handler) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := h.currentUser(r); u != nil {
			p := auth.Principal{Name: u.DisplayName, Email: u.Email, Scope: u.Scope}
			r = r.WithContext(auth.WithPrincipal(r.Context(), p))
		}
		next.ServeHTTP(w, r)
	})
}

// RequireSession gates a handler on a valid session — but ONLY once a user exists.
// While the user store is empty (no-lockout), it passes through unchanged so the
// existing single-operator behaviour is intact and a fresh deployment is usable. It
// must be wrapped INSIDE Middleware (which attaches the principal it checks for).
func (h *Handler) RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		empty, err := h.users.IsEmpty()
		if err != nil {
			http.Error(w, "user store unavailable", http.StatusInternalServerError)
			return
		}
		if empty {
			next.ServeHTTP(w, r) // no-lockout: pre-first-user behaviour preserved
			return
		}
		if _, ok := auth.PrincipalFrom(r.Context()); !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// --- JSON helpers ------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return false
	}
	return true
}
