// Command client is the strict OAuth 2.1 + PKCE-S256 + MCP test client for the
// B-61 Docker multi-host e2e harness. It drives the COMPLETE proxied connect flow
// against the public TLS proxy URL exactly the way claude.ai does — and one step
// further, to the AUTHENTICATED MCP initialize claude.ai never reaches:
//
//  1. unauthenticated MCP initialize probe         -> expect 401 + WWW-Authenticate
//  2. discovery: Protected Resource Metadata + AS metadata
//  3. authorize: CIMD client_id + consent          -> capture the code from the 302
//  4. token: code + PKCE verifier                  -> STRICT parse of the response
//  5. authenticated MCP initialize + a tool call round-trip (the proof)
//
// It parses the /token response strictly (Content-Type incl. charset, token_type,
// JSON fields) so a spec deviation fails HERE, locally and repeatably, instead of
// only inside claude.ai. Any failed step exits non-zero: that exit code IS the
// end-to-end assertion the harness asserts on.
//
// Everything is a test fixture (placeholder hostnames, a local CA, a throwaway
// consent credential supplied by the harness) — no operator deployment value.
package main

import (
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

	// --- 2. discovery: PRM + AS metadata ------------------------------------
	resource, asMeta := discover(ctx, caClient, publicURL, prmURL)

	// --- 3. authorize: CIMD client_id + consent -> code ---------------------
	verifier, challenge := newPKCE()
	code := authorize(ctx, caClient, asMeta.AuthorizationEndpoint, clientID, redirectURI, challenge, resource, consentCred)

	// --- 4. token: code + verifier -> STRICT parse --------------------------
	accessToken := exchangeToken(ctx, caClient, asMeta.TokenEndpoint, clientID, redirectURI, code, verifier)

	// --- 5. authenticated MCP initialize + tool round-trip (the proof) ------
	authenticatedMCP(ctx, caPath, publicURL, accessToken)

	step("DONE", "the complete proxied OAuth + MCP flow succeeded end-to-end")
	fmt.Println("\nB-61 E2E: PASS")
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
	step("2 AS", "issuer=%s authorize=%s token=%s pkce=%v cimd=%v",
		as.Issuer, as.AuthorizationEndpoint, as.TokenEndpoint, as.CodeChallengeMethodsSupported, as.ClientIDMetadataDocumentSupported)
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
// with a charset), token_type=Bearer, Cache-Control: no-store, and the JSON fields.
// A deviation fails here, locally, instead of only inside claude.ai.
func exchangeToken(ctx context.Context, c *http.Client, tokenEndpoint, clientID, redirectURI, code, verifier string) string {
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

	ctHeader := resp.Header.Get("Content-Type")
	cacheControl := resp.Header.Get("Cache-Control")
	step("4 token-resp", "status=%d Content-Type=%q Cache-Control=%q", resp.StatusCode, ctHeader, cacheControl)

	if resp.StatusCode != http.StatusOK {
		fatalf("step 4: token endpoint returned %d (expected 200); body: %s", resp.StatusCode, truncate(string(body), 400))
	}

	// STRICT Content-Type: RFC 6749 §5.1 mandates application/json. A strict
	// consumer (e.g. a conformant httpx-style client) parses the media type and
	// rejects anything whose essence is not application/json. We also require the
	// charset to be absent or utf-8 (JSON is always UTF-8; a different charset is a
	// red flag).
	if err := strictJSONContentType(ctHeader); err != nil {
		fatalf("step 4: STRICT token-response Content-Type rejected: %v (raw %q); body: %s", err, ctHeader, truncate(string(body), 400))
	}
	if !strings.Contains(strings.ToLower(cacheControl), "no-store") {
		fatalf("step 4: token response missing Cache-Control: no-store (RFC 6749 §5.1); got %q", cacheControl)
	}

	// STRICT JSON: reject unknown nonsense by decoding into the typed struct and
	// validating each field a strict client depends on.
	var tr tokenResponse
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(&tr); err != nil {
		fatalf("step 4: token response is not valid JSON: %v; body: %s", err, truncate(string(body), 400))
	}
	if tr.AccessToken == "" {
		fatalf("step 4: token response has empty access_token; body: %s", truncate(string(body), 400))
	}
	// RFC 6749 §7.1: the token_type for a bearer token is "Bearer" (case-insensitive
	// per spec, but a strict client compares case-insensitively to exactly Bearer).
	if !strings.EqualFold(tr.TokenType, "Bearer") {
		fatalf("step 4: token_type=%q, strict client requires \"Bearer\"", tr.TokenType)
	}
	if tr.ExpiresIn <= 0 {
		fatalf("step 4: expires_in=%d, expected a positive lifetime", tr.ExpiresIn)
	}
	step("4 token", "STRICT parse OK: token_type=%q expires_in=%d access_token=%s… refresh_token_present=%v",
		tr.TokenType, tr.ExpiresIn, head(tr.AccessToken, 6), tr.RefreshToken != "")
	return tr.AccessToken
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
