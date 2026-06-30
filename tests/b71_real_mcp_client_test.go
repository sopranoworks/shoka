package tests

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sopranoworks/shoka/internal/identity"
	"github.com/sopranoworks/shoka/internal/mcpclient"
	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/internal/tools"
	"github.com/sopranoworks/shoka/pkg/auth"
	"github.com/sopranoworks/shoka/pkg/oauth"
	"github.com/sopranoworks/shoka/pkg/oauthstore"
)

// B-71 end-to-end via the REAL test MCP client (the original test-MCP-client lineage). STEP 1
// (identification, in the completion report): the "original test MCP client" used during the
// 2026-06-03 OAuth-core work is the loopback, self-started-server, real-mcp-go-sdk-client
// integration-test pattern (the "B-45b lineage", e.g. cmd/shoka-cli/e2e_test.go). It is not a
// distinct binary today: it was productized into internal/mcpclient (B-46b) — the thin
// Bearer-injecting Streamable-HTTP client cmd/shoka-cli runs — and the standalone full-OAuth-flow
// test client lives at tests/e2e-oauth-proxy/client/main.go (B-61/B-63). This test drives that
// REAL client (internal/mcpclient) over the wire to a throwaway in-process server, exercising the
// whole B-71 OAuth surface (connect + call a tool) under (a)–(e).
//
// FINDING (directive §2b): internal/mcpclient does NOT perform OAuth token ACQUISITION — by design
// it is a thin Bearer client (the richer browser/PKCE OAuthHandler is deferred). So confidential
// (and DCR-domain) token acquisition is driven here by a minimal TEST-SCOPED OAuth-flow helper
// (raw /authorize + /token), then the REAL client connects with the acquired bearer and calls a
// tool. This keeps the product client thin while testing the end-to-end path through the real client.

// b71Env is a throwaway server exposing both the auth-enforced MCP resource (with the live
// tools/call scope gate) and the built-in OAuth authorization server, over one shared oauthstore.
type b71Env struct {
	store    *oauthstore.Store
	mcpURL   string
	oauthURL string
}

func newB71Env(t *testing.T) b71Env {
	t.Helper()
	dir := t.TempDir()

	s, err := storage.NewFSGitStorageWithOptions(dir, storage.Options{
		Identity: identity.Defaults{UserName: "Op Erator", UserEmail: "op@example.test", AgentName: "agent"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	// Two namespaces so a scoped token can be allowed on one and denied on another.
	if err := s.CreateProject("foo", "proj"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject("bar", "proj"); err != nil {
		t.Fatal(err)
	}

	store, err := oauthstore.Open(dir + "/oauth.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// --- The auth-enforced MCP resource, with the tools/call scope gate (production wiring) ---
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "shoka-test", Version: "0.0.0"}, nil)
	mcp.AddTool(mcpSrv, &mcp.Tool{Name: "list_projects", Description: "list projects"}, tools.ListProjectsHandler(s))
	mcp.AddTool(mcpSrv, &mcp.Tool{Name: "list_files", Description: "list files"}, tools.ListFilesHandler(s))
	mcpSrv.AddReceivingMiddleware(tools.AuthzMiddleware()) // the dormant scope gate, now live
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpSrv }, nil)
	// ValidateToken mirrors cmd/shoka: an expired token is rejected (ReasonExpired); the token's
	// Scope flows to the principal so the tools/call gate enforces it.
	validate := func(token string) (auth.Principal, auth.RejectReason, bool) {
		if token == "" {
			return auth.Principal{}, auth.ReasonMissingBearer, false
		}
		rec, lerr := store.Lookup(token, time.Now())
		if lerr != nil {
			return auth.Principal{}, auth.ReasonInvalidToken, false
		}
		scope := rec.Scope
		if scope == "" {
			scope = "*"
		}
		return auth.Principal{Name: rec.Principal.Name, Email: rec.Principal.Email, ClientID: rec.ClientID, Scope: scope}, "", true
	}
	authn := auth.New(auth.Config{Enabled: true, ValidateToken: validate})
	mcpTS := httptest.NewServer(authn.Middleware(mcpHandler))
	t.Cleanup(mcpTS.Close)

	// --- The built-in OAuth authorization server (/authorize, /token, /register) ---
	verifier := oauth.NewVerifier(nil)
	verifier.SetTrustedSource(store.TrustedDomain) // dynamic-domain trust (Stage 2c/2e)
	var oauthTS *httptest.Server
	mux := http.NewServeMux()
	as := oauth.NewAuthServer(store, verifier, oauth.AuthServerConfig{
		// ExternalURL is set after the server starts (below) via a closure-free reassign — but
		// httptest needs the handler first. We instead let serverurl fall back to the request
		// host (ExternalURL empty), which httptest provides, so discovery/resource resolve.
		BoundPrincipal: oauthstore.Principal{Name: "Op Erator", Email: "op@example.test"},
		AccessTTL:      time.Hour,
		RefreshTTL:     24 * time.Hour,
		CodeTTL:        time.Minute,
	})
	as.RegisterEndpoints(mux)
	oauthTS = httptest.NewServer(mux)
	t.Cleanup(oauthTS.Close)

	return b71Env{store: store, mcpURL: mcpTS.URL, oauthURL: oauthTS.URL}
}

func pkcePair(t *testing.T) (verifier, challenge string) {
	t.Helper()
	verifier = "verifier-0123456789-ABCDEFGHIJKLMNOPQRSTUVWXYZ-abcdefghijklmnop"
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// noRedirectClient captures the /authorize 302 (the redirect host never resolves; we only read
// the Location).
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// authorizeApprove POSTs /authorize with approve (+ optional consent credential) and returns the
// authorization code from the 302 Location.
func authorizeApprove(t *testing.T, oauthURL, clientID, redirectURI, challenge, consent string) string {
	t.Helper()
	form := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {"st"},
		"approve":               {"1"},
	}
	if consent != "" {
		form.Set("consent_credential", consent)
	}
	resp, err := noRedirectClient().PostForm(oauthURL+"/authorize", form)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("authorize: want 302, got %d (%s)", resp.StatusCode, body)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in authorize redirect: %s", loc)
	}
	return code
}

// tokenExchange POSTs /token (authorization_code) with the PKCE verifier and optional client
// secret, returning the access token. wantStatus lets a caller assert a rejection.
func tokenExchange(t *testing.T, oauthURL, clientID, redirectURI, code, verifier, secret string) string {
	t.Helper()
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	}
	if secret != "" {
		form.Set("client_secret", secret)
	}
	resp, err := http.PostForm(oauthURL+"/token", form)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("token: want 200, got %d (%s)", resp.StatusCode, body)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tr); err != nil {
		t.Fatalf("token decode: %v", err)
	}
	if tr.AccessToken == "" {
		t.Fatal("empty access token")
	}
	return tr.AccessToken
}

// connectAndList drives the REAL MCP client: connect with the bearer + call a tool. callErr
// reports a tool-level (authz) denial separately from a connect (auth) failure.
func connectAndCall(t *testing.T, mcpURL, token, tool string, args map[string]any) (connectErr error, isError bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := mcpclient.Connect(ctx, mcpURL, token)
	if err != nil {
		return err, false
	}
	defer func() { _ = sess.Close() }()
	res, err := sess.CallTool(ctx, tool, args)
	if err != nil {
		t.Fatalf("call %s transport error: %v", tool, err)
	}
	return nil, res.IsError
}

// (a) self-issued parity: the operator self-issued bearer still connects + calls a tool after B-71.
func TestB71RealClient_SelfIssued(t *testing.T) {
	env := newB71Env(t)
	rec, err := env.store.NewSeries(oauthstore.SelfIssuedClientID,
		oauthstore.Principal{Name: "Op Erator", Email: "op@example.test"},
		"https://rs.example/mcp", "*", "", time.Now(), time.Hour, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	connErr, isErr := connectAndCall(t, env.mcpURL, rec.AccessToken, "list_projects", nil)
	if connErr != nil {
		t.Fatalf("self-issued connect: %v", connErr)
	}
	if isErr {
		t.Fatal("self-issued all-access token must be allowed on list_projects")
	}
}

// (b) confidential (Client ID + Secret + PKCE): the real client connects with a token acquired via
// the confidential OAuth flow and calls a tool.
func TestB71RealClient_Confidential(t *testing.T) {
	env := newB71Env(t)
	entry, secret, err := env.store.IssueConfidentialClient("*", "", time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	verifier, challenge := pkcePair(t)
	redirectURI := "https://app.example.com/cb" // confidential accepts any https
	code := authorizeApprove(t, env.oauthURL, entry.Identifier, redirectURI, challenge, "")
	token := tokenExchange(t, env.oauthURL, entry.Identifier, redirectURI, code, verifier, secret)

	connErr, isErr := connectAndCall(t, env.mcpURL, token, "list_projects", nil)
	if connErr != nil {
		t.Fatalf("confidential connect: %v", connErr)
	}
	if isErr {
		t.Fatal("an all-access confidential token must be allowed on list_projects")
	}
}

// (c) scoped token (allowed vs denied namespace), through the real client — the now-live gate.
// RED proof: widen the issued scope to "*" → the bar call would be ALLOWED → the deny assertion
// below fails. The all-access (a)/(b) tokens demonstrate exactly that contrast.
func TestB71RealClient_ScopedAllowedVsDenied(t *testing.T) {
	env := newB71Env(t)
	entry, secret, err := env.store.IssueConfidentialClient("namespace:foo:rw", "", time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	verifier, challenge := pkcePair(t)
	redirectURI := "https://app.example.com/cb"
	code := authorizeApprove(t, env.oauthURL, entry.Identifier, redirectURI, challenge, "")
	token := tokenExchange(t, env.oauthURL, entry.Identifier, redirectURI, code, verifier, secret)

	// Allowed in its granted namespace.
	if connErr, isErr := connectAndCall(t, env.mcpURL, token, "list_files", map[string]any{"namespace": "foo", "project_name": "proj"}); connErr != nil || isErr {
		t.Fatalf("scoped token must be ALLOWED on its namespace foo: connErr=%v isErr=%v", connErr, isErr)
	}
	// Denied in another namespace (the live scope gate).
	connErr, isErr := connectAndCall(t, env.mcpURL, token, "list_files", map[string]any{"namespace": "bar", "project_name": "proj"})
	if connErr != nil {
		t.Fatalf("scoped token connect (bar): %v", connErr)
	}
	if !isErr {
		t.Fatal("scoped namespace:foo token must be DENIED on namespace bar (scope enforced through the real client)")
	}
}

// (d) finite expiry: a real client's short-lived token stops authorizing after it expires.
// RED proof: if there were no finite floor / the token never expired, the post-expiry connect
// would succeed → this fails.
func TestB71RealClient_FiniteExpiry(t *testing.T) {
	env := newB71Env(t)
	rec, err := env.store.NewSeries(oauthstore.SelfIssuedClientID,
		oauthstore.Principal{Name: "Op Erator", Email: "op@example.test"},
		"https://rs.example/mcp", "*", "", time.Now(), 600*time.Millisecond, 600*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	// Before expiry: authorizes.
	if connErr, _ := connectAndCall(t, env.mcpURL, rec.AccessToken, "list_projects", nil); connErr != nil {
		t.Fatalf("a fresh finite token must authorize: %v", connErr)
	}
	// After expiry: the same token no longer authorizes (connect is rejected).
	time.Sleep(900 * time.Millisecond)
	if connErr, _ := connectAndCall(t, env.mcpURL, rec.AccessToken, "list_projects", nil); connErr == nil {
		t.Fatal("an EXPIRED token must no longer authorize the real client")
	}
}

// (e) dynamic-domain trust: a DCR client under a TRUSTED dynamic "domain" entry completes the full
// flow and the real client calls a tool; an UNTRUSTED domain is rejected at /register, so no token
// is ever issued. (The MCP client is a bearer client — domain trust is enforced at the OAuth
// registration/authorize layer the harness drives, "where applicable" per the directive.)
func TestB71RealClient_DynamicDomainTrust(t *testing.T) {
	env := newB71Env(t)
	now := time.Now()

	// Trust "trusted.example" with a per-domain consent (Stage 2c/2e).
	dom, err := env.store.CreateRegistration(oauthstore.RegistrationModeDomain, "trusted.example", now)
	if err != nil {
		t.Fatal(err)
	}
	const consent = "the-per-domain-consent"
	dom.SetConsent(consent)
	if err := env.store.UpdateRegistration(dom); err != nil {
		t.Fatal(err)
	}

	// A DCR client whose redirect host is a subdomain of the trusted domain registers (201).
	trustedRedirect := "https://app.trusted.example/cb"
	clientID := dcrRegister(t, env.oauthURL, trustedRedirect, http.StatusCreated)
	verifier, challenge := pkcePair(t)
	code := authorizeApprove(t, env.oauthURL, clientID, trustedRedirect, challenge, consent)
	token := tokenExchange(t, env.oauthURL, clientID, trustedRedirect, code, verifier, "")
	if connErr, isErr := connectAndCall(t, env.mcpURL, token, "list_projects", nil); connErr != nil || isErr {
		t.Fatalf("a trusted-domain DCR token must connect + call: connErr=%v isErr=%v", connErr, isErr)
	}

	// An UNTRUSTED domain (no "domain" entry) is rejected at /register — no token is ever issued.
	dcrRegister(t, env.oauthURL, "https://app.untrusted.example/cb", http.StatusBadRequest)
}

// dcrRegister POSTs an RFC 7591 registration and asserts the status, returning the client_id on a
// successful (201) registration.
func dcrRegister(t *testing.T, oauthURL, redirectURI string, wantStatus int) string {
	t.Helper()
	body := `{"redirect_uris":["` + redirectURI + `"],"token_endpoint_auth_method":"none",` +
		`"grant_types":["authorization_code","refresh_token"],"response_types":["code"]}`
	resp, err := http.Post(oauthURL+"/register", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != wantStatus {
		t.Fatalf("register %s: want %d, got %d (%s)", redirectURI, wantStatus, resp.StatusCode, raw)
	}
	if wantStatus != http.StatusCreated {
		return ""
	}
	var r struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		t.Fatalf("register decode: %v", err)
	}
	return r.ClientID
}
