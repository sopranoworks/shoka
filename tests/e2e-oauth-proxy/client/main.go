// Command client is the strict OAuth 2.1 + PKCE-S256 + MCP test client for the
// B-61 / B-63 Docker multi-host e2e harness. It drives the COMPLETE proxied connect
// flow against the public TLS proxy URL exactly the way claude.ai does — and one
// step further, to the AUTHENTICATED MCP initialize claude.ai never reaches.
//
// It proves BOTH client-registration paths the AS advertises:
//
//   - CIMD path (the pre-B-63 flow): client_id is an https metadata URL.
//   - DCR path (B-63, RFC 7591): POST client metadata to registration_endpoint,
//     receive an opaque public client_id, then run the flow with it. claude.ai's
//     connector docs REQUIRE DCR, so this is the path the live connect uses.
//
// Per path:
//
//  1. unauthenticated MCP initialize probe         -> expect 401 + WWW-Authenticate
//  2. discovery: Protected Resource Metadata + AS metadata (incl. registration_endpoint)
//  3. (DCR only) register: POST metadata             -> opaque public client_id (no secret)
//  4. authorize: client_id + consent               -> capture the code from the 302
//  5. token: code + PKCE verifier                  -> STRICT parse of the response
//     5b. refresh rotation                            -> new pair + old refresh invalidated
//  6. authenticated MCP initialize + a tool call round-trip (the proof)
//
// It parses the /token response strictly (Content-Type incl. charset, token_type,
// JSON fields) so a spec deviation fails HERE, locally and repeatably, instead of
// only inside claude.ai. Any failed step exits non-zero: that exit code IS the
// end-to-end assertion the harness asserts on. Without the /register endpoint the
// DCR path cannot proceed (no registration_endpoint advertised / 404 POST), so the
// harness fails — exactly the B-63 "fail without /register, pass with it" bar.
//
// Everything is a test fixture (placeholder hostnames, a local CA, a throwaway
// consent credential supplied by the harness) — no operator deployment value.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// env reads a required environment variable or dies.
func env(key string) string {
	v := os.Getenv(key)
	if v == "" {
		fatalf("missing required env %s", key)
	}
	return v
}

func main() {
	var (
		publicURL   = strings.TrimRight(env("SHOKA_PUBLIC_URL"), "/") // https://shoka.test
		clientID    = env("CLIENT_ID")                                // https://client.test/cimd/client.json
		redirectURI = env("REDIRECT_URI")                             // https://client.test/callback
		consentCred = env("CONSENT_CREDENTIAL")
		caPath      = env("CA_CERT")
	)

	step("config", "public=%s client_id=%s redirect_uri=%s", publicURL, clientID, redirectURI)

	// Trust the harness's local CA for every TLS call to the proxy.
	caClient := newCAClient(caPath, "")

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// Wait for the proxy -> shoka path to come up (container start ordering): poll
	// the unauthenticated AS-metadata document until it answers 200 over TLS.
	waitForReady(ctx, caClient, publicURL)

	// --- 1. unauthenticated MCP initialize probe -> 401 + WWW-Authenticate ---
	prmURL := probeUnauthenticated(ctx, caClient, publicURL)

	// --- 2. discovery: PRM + AS metadata (incl. registration_endpoint) ------
	resource, asMeta := discover(ctx, caClient, publicURL, prmURL)

	// --- CIMD path: client_id is the https metadata URL (pre-B-63 flow) ------
	step("PATH", "=== CIMD path (client_id = metadata URL) ===")
	connectAndProve(ctx, caClient, caPath, publicURL, asMeta, resource, clientID, redirectURI, consentCred)

	// --- DCR path (B-63): register first, then run with the issued client_id -
	// claude.ai's connector docs REQUIRE DCR — this is the path the live connect
	// uses. Without /register this step cannot start (no registration_endpoint /
	// 404 POST), so the harness fails: the B-63 "fail without /register" bar.
	step("PATH", "=== DCR path (RFC 7591 register -> issued client_id) ===")
	if asMeta.RegistrationEndpoint == "" {
		fatalf("DCR: AS metadata advertises no registration_endpoint — claude.ai's docs require DCR (B-63)")
	}
	dcrClientID := registerDCRClient(ctx, caClient, asMeta.RegistrationEndpoint, redirectURI)
	connectAndProve(ctx, caClient, caPath, publicURL, asMeta, resource, dcrClientID, redirectURI, consentCred)

	step("DONE", "the complete proxied OAuth + MCP flow succeeded end-to-end on BOTH the CIMD and DCR paths")
	fmt.Println("\nB-63 E2E: PASS")
}

// connectAndProve runs one full registration-path-agnostic flow: authorize (with
// the given client_id + consent) -> token (STRICT parse) -> refresh rotation ->
// authenticated MCP initialize + tool round-trip. It is called once per
// registration path (CIMD, DCR) so both are proven end-to-end through the proxy.
func connectAndProve(ctx context.Context, c *http.Client, caPath, publicURL string, asMeta asMetadata, resource, clientID, redirectURI, consentCred string) {
	verifier, challenge := newPKCE()
	code := authorize(ctx, c, asMeta.AuthorizationEndpoint, clientID, redirectURI, challenge, resource, consentCred)
	accessToken, refreshToken := exchangeToken(ctx, c, asMeta.TokenEndpoint, clientID, redirectURI, code, verifier)
	// OAuth 2.1 requires public clients to rotate refresh tokens; carry the ROTATED
	// access token forward so the authenticated MCP step proves the rotated token works.
	accessToken = rotateRefresh(ctx, c, asMeta.TokenEndpoint, refreshToken)
	authenticatedMCP(ctx, caPath, publicURL, accessToken)
}

// registrationResponse is the subset of the RFC 7591 registration response the
// harness asserts on.
type registrationResponse struct {
	ClientID                string   `json:"client_id"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	ClientSecret            string   `json:"client_secret"` // MUST be absent (public client)
}

// registerDCRClient POSTs an RFC 7591 client-metadata document to the advertised
// registration_endpoint and returns the issued client_id, asserting it is an
// opaque PUBLIC client (no secret, token_endpoint_auth_method "none", not a URL).
func registerDCRClient(ctx context.Context, c *http.Client, registrationEndpoint, redirectURI string) string {
	meta := map[string]any{
		"client_name":                "b63-e2e-dcr",
		"redirect_uris":              []string{redirectURI},
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	}
	buf, err := json.Marshal(meta)
	must(err, "marshal DCR client metadata")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, registrationEndpoint, bytes.NewReader(buf))
	must(err, "build register POST")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.Do(req)
	must(err, "send register POST")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		fatalf("DCR register: expected 201, got %d; body: %s", resp.StatusCode, truncate(string(body), 400))
	}
	if !strings.HasPrefix(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") {
		fatalf("DCR register: response Content-Type is not application/json: %q", resp.Header.Get("Content-Type"))
	}
	var rr registrationResponse
	if err := json.Unmarshal(body, &rr); err != nil {
		fatalf("DCR register: response is not valid JSON: %v; body: %s", err, truncate(string(body), 400))
	}
	if rr.ClientID == "" {
		fatalf("DCR register: response carried no client_id; body: %s", truncate(string(body), 400))
	}
	if strings.HasPrefix(strings.ToLower(rr.ClientID), "http") {
		fatalf("DCR register: client_id must be an opaque handle, not a URL: %q", rr.ClientID)
	}
	if rr.ClientSecret != "" || strings.Contains(string(body), "client_secret") {
		fatalf("DCR register: a PUBLIC client must NOT receive a client_secret; body: %s", truncate(string(body), 400))
	}
	if rr.TokenEndpointAuthMethod != "none" {
		fatalf("DCR register: token_endpoint_auth_method must be \"none\", got %q", rr.TokenEndpointAuthMethod)
	}
	step("3 DCR register", "registered public client_id=%s… (no secret; token_endpoint_auth_method=none)", head(rr.ClientID, 8))
	return rr.ClientID
}

// waitForReady polls the unauthenticated AS-metadata document until the proxy ->
// shoka path answers 200 over TLS (or the context expires), absorbing container
// start ordering so the test itself does not flake on a cold start.
func waitForReady(ctx context.Context, c *http.Client, publicURL string) {
	asURL := publicURL + "/.well-known/oauth-authorization-server"
	deadline := time.Now().Add(60 * time.Second)
	for attempt := 1; ; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, asURL, nil)
		resp, err := c.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				step("ready", "proxy -> shoka path is up (AS metadata 200 after %d attempt(s))", attempt)
				return
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				fatalf("readiness: proxy/shoka not up within 60s: %v", err)
			}
			fatalf("readiness: proxy/shoka not up within 60s (last status from AS metadata was not 200)")
		}
		time.Sleep(time.Second)
	}
}

// --- step 1 -----------------------------------------------------------------

// probeUnauthenticated sends an MCP initialize POST with no bearer and asserts the
// 401 + WWW-Authenticate: Bearer resource_metadata="..." challenge, returning the
// PRM URL the challenge advertises.
func probeUnauthenticated(ctx context.Context, c *http.Client, publicURL string) string {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{` +
		`"protocolVersion":"2025-06-18","capabilities":{},` +
		`"clientInfo":{"name":"b61-e2e","version":"0.0.1"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, publicURL+"/mcp", strings.NewReader(body))
	must(err, "build initialize probe")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := c.Do(req)
	must(err, "send unauthenticated initialize probe")
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusUnauthorized {
		fatalf("step 1: expected 401 from unauthenticated initialize, got %d", resp.StatusCode)
	}
	wa := resp.Header.Get("WWW-Authenticate")
	if wa == "" {
		fatalf("step 1: 401 carried no WWW-Authenticate header")
	}
	prm := parseResourceMetadata(wa)
	if prm == "" {
		fatalf("step 1: WWW-Authenticate has no resource_metadata parameter: %q", wa)
	}
	step("1 probe", "401 as expected; WWW-Authenticate=%q -> resource_metadata=%s", wa, prm)
	return prm
}

// parseResourceMetadata pulls the resource_metadata="..." value out of a
// WWW-Authenticate: Bearer ... challenge.
func parseResourceMetadata(h string) string {
	const key = `resource_metadata=`
	i := strings.Index(h, key)
	if i < 0 {
		return ""
	}
	v := h[i+len(key):]
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, `"`) {
		v = v[1:]
		if j := strings.Index(v, `"`); j >= 0 {
			v = v[:j]
		}
	} else if j := strings.IndexAny(v, " ,"); j >= 0 {
		v = v[:j]
	}
	return v
}

// --- step 2 -----------------------------------------------------------------

type protectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
}

type asMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	ClientIDMetadataDocumentSupported bool     `json:"client_id_metadata_document_supported"`
}

// discover fetches the PRM (from the challenge URL) and the AS metadata, asserting
// the resource identifier and the PKCE/CIMD signalling a strict client requires.
func discover(ctx context.Context, c *http.Client, publicURL, prmURL string) (resource string, as asMetadata) {
	var prm protectedResourceMetadata
	getJSON(ctx, c, prmURL, &prm, "step 2: fetch protected resource metadata")
	if prm.Resource == "" {
		fatalf("step 2: PRM has empty resource")
	}
	wantResource := publicURL + "/mcp"
	if prm.Resource != wantResource {
		fatalf("step 2: PRM resource %q != connect endpoint %q (exact-match is the #1 discovery failure)", prm.Resource, wantResource)
	}
	if len(prm.AuthorizationServers) == 0 {
		fatalf("step 2: PRM advertises no authorization_servers")
	}
	step("2 PRM", "resource=%s authorization_servers=%v", prm.Resource, prm.AuthorizationServers)

	issuer := strings.TrimRight(prm.AuthorizationServers[0], "/")
	asURL := issuer + "/.well-known/oauth-authorization-server"
	getJSON(ctx, c, asURL, &as, "step 2: fetch authorization server metadata")
	if as.AuthorizationEndpoint == "" || as.TokenEndpoint == "" {
		fatalf("step 2: AS metadata missing authorization_endpoint or token_endpoint")
	}
	if !contains(as.CodeChallengeMethodsSupported, "S256") {
		fatalf("step 2: AS metadata does not advertise S256 PKCE (a strict client refuses): %v", as.CodeChallengeMethodsSupported)
	}
	if !as.ClientIDMetadataDocumentSupported {
		fatalf("step 2: AS metadata does not signal client_id_metadata_document_supported (CIMD)")
	}
	// B-63: DCR and CIMD must COEXIST — registration_endpoint advertised alongside CIMD.
	if as.RegistrationEndpoint == "" {
		fatalf("step 2: AS metadata advertises no registration_endpoint — claude.ai's docs require DCR (B-63)")
	}
	step("2 AS", "issuer=%s authorize=%s token=%s register=%s pkce=%v cimd=%v",
		as.Issuer, as.AuthorizationEndpoint, as.TokenEndpoint, as.RegistrationEndpoint, as.CodeChallengeMethodsSupported, as.ClientIDMetadataDocumentSupported)
	return prm.Resource, as
}

// --- step 3 -----------------------------------------------------------------

// authorize drives /authorize (GET consent page, then POST approval with the
// consent credential) and returns the authorization code parsed from the 302.
func authorize(ctx context.Context, c *http.Client, authorizeEndpoint, clientID, redirectURI, challenge, resource, consentCred string) string {
	state := randToken(16)
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("resource", resource)
	q.Set("state", state)
	authURL := authorizeEndpoint + "?" + q.Encode()

	// GET: the consent page (proves CIMD verification of client_id succeeded —
	// Shoka fetched https://client.test/cimd/client.json over TLS through the SSRF
	// guard and rendered consent).
	{
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, authURL, nil)
		must(err, "build authorize GET")
		resp, err := c.Do(req)
		must(err, "send authorize GET")
		page, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fatalf("step 3: authorize GET (consent) expected 200, got %d; body: %s", resp.StatusCode, truncate(string(page), 400))
		}
		step("3 consent", "consent page rendered (CIMD verification of client_id passed)")
	}

	// POST: approve with the consent credential. Do NOT follow the redirect —
	// capture the Location and parse the code, like a client receiving the callback.
	form := url.Values{}
	form.Set("response_type", "code")
	form.Set("client_id", clientID)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_challenge", challenge)
	form.Set("code_challenge_method", "S256")
	form.Set("resource", resource)
	form.Set("state", state)
	form.Set("consent_credential", consentCred)
	form.Set("approve", "1")

	noRedirect := *c
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authorizeEndpoint, strings.NewReader(form.Encode()))
	must(err, "build authorize POST")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := noRedirect.Do(req)
	must(err, "send authorize POST")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusSeeOther {
		fatalf("step 3: authorize POST expected 302/303 redirect, got %d; body: %s", resp.StatusCode, truncate(string(body), 400))
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		fatalf("step 3: authorize POST redirect carried no Location")
	}
	lu, err := url.Parse(loc)
	must(err, "parse authorize redirect Location")
	if e := lu.Query().Get("error"); e != "" {
		fatalf("step 3: authorize redirected with error=%s (%s)", e, lu.Query().Get("error_description"))
	}
	if lu.Query().Get("state") != state {
		fatalf("step 3: authorize redirect state mismatch: got %q want %q", lu.Query().Get("state"), state)
	}
	code := lu.Query().Get("code")
	if code == "" {
		fatalf("step 3: authorize redirect carried no code: %s", loc)
	}
	step("3 code", "authorization code obtained from redirect to %s", lu.Scheme+"://"+lu.Host+lu.Path)
	return code
}

// --- step 4 -----------------------------------------------------------------

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// exchangeToken POSTs the code + PKCE verifier and parses the response STRICTLY:
// the exact Content-Type (a strict consumer keys on application/json — optionally
// with a charset), token_type=Bearer, Cache-Control: no-store, Pragma: no-cache, and
// the JSON fields. A deviation fails here, locally, instead of only inside claude.ai.
// Returns the access AND refresh tokens (the refresh feeds the rotation assertion).
func exchangeToken(ctx context.Context, c *http.Client, tokenEndpoint, clientID, redirectURI, code, verifier string) (access, refresh string) {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("client_id", clientID)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", verifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	must(err, "build token POST")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.Do(req)
	must(err, "send token POST")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	step("4 token-resp", "status=%d Content-Type=%q Cache-Control=%q Pragma=%q",
		resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"), resp.Header.Get("Pragma"))

	if resp.StatusCode != http.StatusOK {
		fatalf("step 4: token endpoint returned %d (expected 200); body: %s", resp.StatusCode, truncate(string(body), 400))
	}
	tr := strictTokenResponse(resp, body, "step 4")
	step("4 token", "STRICT parse OK: token_type=%q expires_in=%d access_token=%s… refresh_token_present=%v",
		tr.TokenType, tr.ExpiresIn, head(tr.AccessToken, 6), tr.RefreshToken != "")
	if tr.RefreshToken == "" {
		fatalf("step 4: token response has empty refresh_token (public-client rotation needs one)")
	}
	return tr.AccessToken, tr.RefreshToken
}

// rotateRefresh exercises the OAuth 2.1 public-client refresh-token rotation
// requirement end-to-end: it exchanges the refresh token, strictly parses the new
// response, asserts a NEW access+refresh pair was returned, and asserts the OLD
// refresh token is now INVALIDATED (a second use is rejected with invalid_grant).
// It returns the rotated access token so the caller can prove it authenticates.
func rotateRefresh(ctx context.Context, c *http.Client, tokenEndpoint, oldRefresh string) string {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", oldRefresh)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	must(err, "build refresh POST")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.Do(req)
	must(err, "send refresh POST")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	step("4b refresh-resp", "status=%d Content-Type=%q Cache-Control=%q",
		resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Cache-Control"))
	if resp.StatusCode != http.StatusOK {
		fatalf("step 4b: refresh grant returned %d (expected 200); body: %s", resp.StatusCode, truncate(string(body), 400))
	}
	tr := strictTokenResponse(resp, body, "step 4b")
	if tr.RefreshToken == "" {
		fatalf("step 4b: rotation returned no new refresh_token (OAuth 2.1 public-client rotation)")
	}
	if tr.AccessToken == "" || tr.RefreshToken == oldRefresh {
		fatalf("step 4b: rotation did not issue a NEW refresh token (got same handle back)")
	}

	// The OLD refresh token must be invalidated by that same rotation (return the new
	// refresh in the same response that invalidates the old one).
	form2 := url.Values{}
	form2.Set("grant_type", "refresh_token")
	form2.Set("refresh_token", oldRefresh)
	req2, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form2.Encode()))
	must(err, "build old-refresh-reuse POST")
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp2, err := c.Do(req2)
	must(err, "send old-refresh-reuse POST")
	reuseBody, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest || !strings.Contains(string(reuseBody), "invalid_grant") {
		fatalf("step 4b: OLD refresh token was NOT invalidated by rotation: reuse got %d (%s); want 400 invalid_grant",
			resp2.StatusCode, truncate(string(reuseBody), 400))
	}
	step("4b refresh", "rotation OK: new pair issued, old refresh invalidated (OAuth 2.1 public-client rotation)")
	return tr.AccessToken
}

// strictTokenResponse parses a 200 /token response STRICTLY against RFC 6749 §5.1:
// Content-Type application/json (charset absent or utf-8), Cache-Control: no-store,
// Pragma: no-cache, token_type=Bearer, expires_in>0, non-empty access_token. A
// deviation fails here, locally, instead of only inside claude.ai.
func strictTokenResponse(resp *http.Response, body []byte, where string) tokenResponse {
	ctHeader := resp.Header.Get("Content-Type")
	if err := strictJSONContentType(ctHeader); err != nil {
		fatalf("%s: STRICT token-response Content-Type rejected: %v (raw %q); body: %s", where, err, ctHeader, truncate(string(body), 400))
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(strings.ToLower(cc), "no-store") {
		fatalf("%s: token response missing Cache-Control: no-store (RFC 6749 §5.1); got %q", where, cc)
	}
	if pragma := resp.Header.Get("Pragma"); strings.ToLower(strings.TrimSpace(pragma)) != "no-cache" {
		fatalf("%s: token response missing Pragma: no-cache (RFC 6749 §5.1); got %q", where, pragma)
	}

	var tr tokenResponse
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(&tr); err != nil {
		fatalf("%s: token response is not valid JSON: %v; body: %s", where, err, truncate(string(body), 400))
	}
	if tr.AccessToken == "" {
		fatalf("%s: token response has empty access_token; body: %s", where, truncate(string(body), 400))
	}
	if !strings.EqualFold(tr.TokenType, "Bearer") {
		fatalf("%s: token_type=%q, strict client requires \"Bearer\"", where, tr.TokenType)
	}
	if tr.ExpiresIn <= 0 {
		fatalf("%s: expires_in=%d, expected a positive lifetime", where, tr.ExpiresIn)
	}
	return tr
}

// strictJSONContentType validates that ct's media type is application/json with no
// charset, or charset=utf-8. Mirrors what a conformant strict OAuth client enforces.
func strictJSONContentType(ct string) error {
	if ct == "" {
		return fmt.Errorf("missing Content-Type")
	}
	parts := strings.Split(ct, ";")
	essence := strings.ToLower(strings.TrimSpace(parts[0]))
	if essence != "application/json" {
		return fmt.Errorf("media type %q is not application/json", essence)
	}
	for _, p := range parts[1:] {
		p = strings.ToLower(strings.TrimSpace(p))
		if strings.HasPrefix(p, "charset=") {
			cs := strings.TrimPrefix(p, "charset=")
			cs = strings.Trim(cs, `"`)
			if cs != "utf-8" && cs != "us-ascii" {
				return fmt.Errorf("unexpected charset %q", cs)
			}
		}
	}
	return nil
}

// --- step 5 -----------------------------------------------------------------

// authenticatedMCP performs the step claude.ai never reaches: the authenticated
// MCP initialize with the bearer token, then a tool round-trip — all through the
// TLS reverse proxy. This proves the connect locally.
func authenticatedMCP(ctx context.Context, caPath, publicURL, accessToken string) {
	httpClient := newCAClient(caPath, accessToken)
	transport := &mcp.StreamableClientTransport{Endpoint: publicURL + "/mcp", HTTPClient: httpClient}
	client := mcp.NewClient(&mcp.Implementation{Name: "b61-e2e", Version: "0.0.1"}, nil)

	sess, err := client.Connect(ctx, transport, nil)
	if err != nil {
		fatalf("step 5: authenticated MCP initialize (Connect) failed through the proxy: %v", err)
	}
	defer sess.Close()
	step("5 initialize", "authenticated MCP session established (session id assigned)")

	lt, err := sess.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		fatalf("step 5: tools/list failed: %v", err)
	}
	if len(lt.Tools) == 0 {
		fatalf("step 5: tools/list returned no tools")
	}
	step("5 tools/list", "%d tools returned (e.g. %s)", len(lt.Tools), firstToolNames(lt.Tools, 5))

	// A real tool CALL round-trip: pick a tool with no required arguments and call
	// it (discovered, never hard-coded — matches the live-suite convention).
	name := firstNoArgTool(lt.Tools)
	if name == "" {
		step("5 tools/call", "no zero-arg tool to call; tools/list round-trip already proves the session")
		return
	}
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
	if err != nil {
		fatalf("step 5: tools/call %s failed: %v", name, err)
	}
	if res.IsError {
		fatalf("step 5: tools/call %s returned an MCP error result", name)
	}
	step("5 tools/call", "called %s — round-trip OK (the connect is proven through the proxy)", name)
}

func firstNoArgTool(tools []*mcp.Tool) string {
	for _, t := range tools {
		if hasNoRequiredArgs(t.InputSchema) {
			return t.Name
		}
	}
	return ""
}

// hasNoRequiredArgs reports whether a tool's input schema declares no required
// fields (callable with empty arguments). The SDK types InputSchema as `any`
// (decoded JSON), so we inspect the raw object — matching the live-suite helper.
func hasNoRequiredArgs(schema any) bool {
	m, ok := schema.(map[string]any)
	if !ok {
		return true
	}
	req, ok := m["required"]
	if !ok {
		return true
	}
	arr, ok := req.([]any)
	if !ok {
		return true
	}
	return len(arr) == 0
}

func firstToolNames(tools []*mcp.Tool, n int) string {
	var names []string
	for i, t := range tools {
		if i >= n {
			break
		}
		names = append(names, t.Name)
	}
	return strings.Join(names, ", ")
}

// --- helpers ----------------------------------------------------------------

// newCAClient builds an http.Client that trusts the local CA at caPath. When
// bearer is non-empty every request carries Authorization: Bearer <bearer>.
func newCAClient(caPath, bearer string) *http.Client {
	pem, err := os.ReadFile(caPath)
	must(err, "read CA cert "+caPath)
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		fatalf("could not parse CA cert at %s", caPath)
	}
	var base http.RoundTripper = &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
	}
	if bearer != "" {
		base = bearerRoundTripper{base: base, token: bearer}
	}
	return &http.Client{Transport: base, Timeout: 30 * time.Second}
}

type bearerRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (rt bearerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r2 := r.Clone(r.Context())
	r2.Header.Set("Authorization", "Bearer "+rt.token)
	return rt.base.RoundTrip(r2)
}

func getJSON(ctx context.Context, c *http.Client, urlStr string, out any, what string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	must(err, "build GET "+what)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	must(err, what)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fatalf("%s: status %d; body: %s", what, resp.StatusCode, truncate(string(body), 400))
	}
	if err := json.Unmarshal(body, out); err != nil {
		fatalf("%s: invalid JSON: %v; body: %s", what, err, truncate(string(body), 400))
	}
}

// newPKCE returns a fresh (verifier, S256 challenge) pair.
func newPKCE() (verifier, challenge string) {
	verifier = randToken(48)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge
}

// randToken returns n bytes of randomness as a base64url string. Uses crypto/rand.
func randToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		fatalf("crypto/rand failed: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func step(tag, format string, args ...any) {
	fmt.Printf("[%s] %s\n", tag, fmt.Sprintf(format, args...))
}

func must(err error, what string) {
	if err != nil {
		fatalf("%s: %v", what, err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Printf("\n[FAIL] %s\n", fmt.Sprintf(format, args...))
	fmt.Println("\nB-61 E2E: FAIL")
	os.Exit(1)
}
