package authapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
	"github.com/sopranoworks/shoka/pkg/auth"
	"github.com/sopranoworks/shoka/pkg/userstore"
)

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i * 7)
	}
	return k
}

func newTestHandler(t *testing.T, allowFirst bool) (*Handler, *userstore.Store) {
	t.Helper()
	us, err := userstore.Open(filepath.Join(t.TempDir(), "users.db"), testKey())
	if err != nil {
		t.Fatalf("open userstore: %v", err)
	}
	t.Cleanup(func() { _ = us.Close() })
	h := New(Config{Users: us, SessionTTL: time.Hour, AllowFirstRunAdmin: allowFirst})
	return h, us
}

func postJSON(t *testing.T, h http.Handler, path string, body any, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(http.MethodPost, path, &buf)
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	return nil
}

func TestStatus_FirstRunThenAuthenticated(t *testing.T) {
	h, _ := newTestHandler(t, true)

	// Empty store: not users_exist, first_run_allowed.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/status", nil))
	var st statusResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if st.UsersExist || !st.FirstRunAllowed || st.Authenticated {
		t.Fatalf("fresh status = %+v", st)
	}

	// Register first admin.
	rec = postJSON(t, h, "/auth/register", registerRequest{Email: "op@example.com", DisplayName: "Op", Password: "hunter2hunter2"})
	if rec.Code != http.StatusOK {
		t.Fatalf("register code = %d body=%s", rec.Code, rec.Body.String())
	}
	ck := sessionCookie(rec)
	if ck == nil {
		t.Fatal("register set no session cookie")
	}

	// Status with the session cookie: authenticated admin.
	req := httptest.NewRequest(http.MethodGet, "/auth/status", nil)
	req.AddCookie(ck)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	if !st.UsersExist || !st.Authenticated || st.Principal == nil || !st.Principal.IsAdmin {
		t.Fatalf("authenticated status = %+v", st)
	}
	if st.Principal.Email != "op@example.com" {
		t.Fatalf("principal email = %q", st.Principal.Email)
	}
}

func TestRegister_RefusedWhenUsersExist(t *testing.T) {
	h, _ := newTestHandler(t, true)
	rec := postJSON(t, h, "/auth/register", registerRequest{Email: "a@x.com", Password: "passw0rd!"})
	if rec.Code != http.StatusOK {
		t.Fatalf("first register: %d %s", rec.Code, rec.Body.String())
	}
	rec = postJSON(t, h, "/auth/register", registerRequest{Email: "b@x.com", Password: "passw0rd!"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("second register: want 409, got %d", rec.Code)
	}
}

func TestRegister_RefusedWhenFirstRunDisabled(t *testing.T) {
	h, _ := newTestHandler(t, false)
	rec := postJSON(t, h, "/auth/register", registerRequest{Email: "a@x.com", Password: "passw0rd!"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("first-run disabled: want 403, got %d", rec.Code)
	}
}

func TestLogin_PasswordAndTOTP(t *testing.T) {
	h, us := newTestHandler(t, true)

	// Enrol an admin WITH TOTP via the register flow (proving the code).
	key, _ := userstore.GenerateTOTP("Shoka", "op@example.com")
	code, _ := totp.GenerateCode(key.Secret(), time.Now())
	rec := postJSON(t, h, "/auth/register", registerRequest{
		Email: "op@example.com", DisplayName: "Op", Password: "hunter2hunter2",
		TOTPSecret: key.Secret(), TOTPCode: code,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("register with totp: %d %s", rec.Code, rec.Body.String())
	}
	u, _ := us.GetUser("op@example.com")
	if !u.HasTOTP() {
		t.Fatal("admin should have TOTP enrolled")
	}

	// Login without a TOTP code → totp_required signal.
	rec = postJSON(t, h, "/auth/login", loginRequest{Email: "op@example.com", Password: "hunter2hunter2"})
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("login w/o totp: want 401, got %d", rec.Code)
	}
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	if m["totp_required"] != true {
		t.Fatalf("expected totp_required signal, got %v", m)
	}

	// Login with a valid code → session.
	code2, _ := totp.GenerateCode(key.Secret(), time.Now())
	rec = postJSON(t, h, "/auth/login", loginRequest{Email: "op@example.com", Password: "hunter2hunter2", TOTPCode: code2})
	if rec.Code != http.StatusOK || sessionCookie(rec) == nil {
		t.Fatalf("login with totp: code=%d cookie=%v body=%s", rec.Code, sessionCookie(rec), rec.Body.String())
	}

	// Wrong password → 401, no session.
	rec = postJSON(t, h, "/auth/login", loginRequest{Email: "op@example.com", Password: "wrong", TOTPCode: code2})
	if rec.Code != http.StatusUnauthorized || sessionCookie(rec) != nil {
		t.Fatalf("wrong password: code=%d", rec.Code)
	}
}

// TestLogin_DisabledUserRefused: a disabled account is refused at handleLogin AFTER the
// correct password (403, no session); re-enabling restores login. The gate is what gives
// the Disabled flag teeth on the password path (B-28).
func TestLogin_DisabledUserRefused(t *testing.T) {
	h, us := newTestHandler(t, true)
	rec := postJSON(t, h, "/auth/register", registerRequest{
		Email: "op@example.com", DisplayName: "Op", Password: "hunter2hunter2",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	// Enabled + correct password → session.
	rec = postJSON(t, h, "/auth/login", loginRequest{Email: "op@example.com", Password: "hunter2hunter2"})
	if rec.Code != http.StatusOK || sessionCookie(rec) == nil {
		t.Fatalf("enabled login should succeed: code=%d", rec.Code)
	}
	// Disable, then the SAME correct password is refused with no session.
	if err := us.SetUserDisabled("op@example.com", true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}
	rec = postJSON(t, h, "/auth/login", loginRequest{Email: "op@example.com", Password: "hunter2hunter2"})
	if rec.Code != http.StatusForbidden || sessionCookie(rec) != nil {
		t.Fatalf("disabled login must be 403 with no session: code=%d cookie=%v body=%s",
			rec.Code, sessionCookie(rec), rec.Body.String())
	}
	// Re-enable → login works again.
	if err := us.SetUserDisabled("op@example.com", false); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	rec = postJSON(t, h, "/auth/login", loginRequest{Email: "op@example.com", Password: "hunter2hunter2"})
	if rec.Code != http.StatusOK || sessionCookie(rec) == nil {
		t.Fatalf("re-enabled login should succeed: code=%d", rec.Code)
	}
}

func TestLogout_DeletesSession(t *testing.T) {
	h, _ := newTestHandler(t, true)
	rec := postJSON(t, h, "/auth/register", registerRequest{Email: "a@x.com", Password: "passw0rd!"})
	ck := sessionCookie(rec)
	// Logout clears the cookie and deletes the session.
	rec = postJSON(t, h, "/auth/logout", nil, ck)
	if rec.Code != http.StatusOK {
		t.Fatalf("logout: %d", rec.Code)
	}
	// The session no longer authenticates.
	req := httptest.NewRequest(http.MethodGet, "/auth/status", nil)
	req.AddCookie(ck)
	srec := httptest.NewRecorder()
	h.ServeHTTP(srec, req)
	var st statusResponse
	_ = json.Unmarshal(srec.Body.Bytes(), &st)
	if st.Authenticated {
		t.Fatal("session still authenticates after logout")
	}
}

// TestMiddleware_AttachesPrincipalFromSession proves a valid session cookie yields
// an auth.Principal on the context carrying the user's email and scope.
func TestMiddleware_AttachesPrincipalFromSession(t *testing.T) {
	h, us := newTestHandler(t, true)
	ph, _ := userstore.HashPassword("pw")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "op@example.com", DisplayName: "Op", PasswordHash: ph})
	sess, _ := us.CreateSession("op@example.com", time.Now(), time.Hour)

	var gotPrincipal *auth.Principal
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if p, ok := auth.PrincipalFrom(r.Context()); ok {
			gotPrincipal = &p
		}
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/ws/ui", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
	h.Middleware(inner).ServeHTTP(httptest.NewRecorder(), req)

	if gotPrincipal == nil {
		t.Fatal("no principal attached for a valid session")
	}
	if gotPrincipal.Email != "op@example.com" || gotPrincipal.Scope != userstore.AdminScope {
		t.Fatalf("principal = %+v, want email/scope from the user", *gotPrincipal)
	}
}

// TestMiddleware_IgnoresOAuthBearer is the surface-separation proof: the WebUI login
// path is driven ONLY by the session cookie. An OAuth-style bearer token (the MCP
// credential) with NO session cookie yields NO principal — the Web path never
// consults the MCP token surface (B-50).
func TestMiddleware_IgnoresOAuthBearer(t *testing.T) {
	h, _ := newTestHandler(t, true)
	attached := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, attached = auth.PrincipalFrom(r.Context())
	})
	req := httptest.NewRequest(http.MethodGet, "/ws/ui", nil)
	req.Header.Set("Authorization", "Bearer an-mcp-access-token")
	h.Middleware(inner).ServeHTTP(httptest.NewRecorder(), req)
	if attached {
		t.Fatal("an OAuth bearer must NOT yield a WebUI principal (surface separation)")
	}
}

// TestRequireSession_NoLockoutThenGated proves: while the user store is empty the
// gate passes through (no-lockout); once a user exists an un-authenticated request
// is 401 and a valid session passes.
func TestRequireSession_NoLockoutThenGated(t *testing.T) {
	h, us := newTestHandler(t, true)
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	gated := h.Middleware(h.RequireSession(ok))

	// Empty store → pass-through (no-lockout).
	rec := httptest.NewRecorder()
	gated.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws/ui", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("empty-store no-lockout: want 200, got %d", rec.Code)
	}

	// Create a user → now a session is required.
	ph, _ := userstore.HashPassword("pw")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "op@example.com", PasswordHash: ph})

	rec = httptest.NewRecorder()
	gated.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ws/ui", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("users exist + no session: want 401, got %d", rec.Code)
	}

	sess, _ := us.CreateSession("op@example.com", time.Now(), time.Hour)
	req := httptest.NewRequest(http.MethodGet, "/ws/ui", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sess.ID})
	rec = httptest.NewRecorder()
	gated.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid session: want 200, got %d", rec.Code)
	}
}
