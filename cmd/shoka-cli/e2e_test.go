package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sopranoworks/shoka/internal/auth"
	"github.com/sopranoworks/shoka/internal/clientconfig"
	"github.com/sopranoworks/shoka/internal/drafts"
	"github.com/sopranoworks/shoka/internal/identity"
	"github.com/sopranoworks/shoka/internal/mcpclient"
	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/internal/storage/oauthstore"
	"github.com/sopranoworks/shoka/internal/tools"
	"github.com/sopranoworks/shoka/internal/ui"
)

// TestEndToEndCredentialPath proves the B-46b foundation end to end, in the B-45b
// real-client lineage (loopback, self-started servers, no mocked transport):
//
//  1. the admin-gated OAUTH_ISSUE_SELF action mints a token over a real /ws/ui
//     connection (the token-to-self path);
//  2. `shoka-cli auth` stores it in the XDG client config (here exercised via the
//     same clientconfig.Save the auth subcommand calls);
//  3. the thin cmd/shoka-cli client loads it and connects to a real auth-enforced
//     MCP server with the Bearer token and calls a read-only tool (list_projects)
//     successfully; and
//  4. a bad token is rejected by the same enforcement.
func TestEndToEndCredentialPath(t *testing.T) {
	dir := t.TempDir()
	// Point os.UserConfigDir (used by clientconfig) at a temp tree.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	// Shared storage + a project so list_projects returns a real result.
	s, err := storage.NewFSGitStorageWithOptions(dir, storage.Options{
		Identity: identity.Defaults{
			UserName:  "Osamu Takahashi",
			UserEmail: "forte.nit@gmail.com",
			AgentName: "shoka-agent",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatal(err)
	}

	// The shared OAuth token store: the mint writes here, the MCP enforcement reads
	// here — one store, exactly as production wires it.
	store, err := oauthstore.Open(dir + "/oauth.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// --- The auth-enforced MCP server (the resource the CLI calls) ---------------
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "shoka-test", Version: "0.0.0"}, nil)
	mcp.AddTool(mcpSrv, &mcp.Tool{Name: "list_projects", Description: "list projects"}, tools.ListProjectsHandler(s))
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpSrv }, nil)
	validate := func(token string) (auth.Principal, auth.RejectReason, bool) {
		if token == "" {
			return auth.Principal{}, auth.ReasonMissingBearer, false
		}
		rec, lerr := store.Lookup(token, time.Now())
		if lerr != nil {
			return auth.Principal{}, auth.ReasonInvalidToken, false
		}
		return auth.Principal{Name: rec.Principal.Name, Email: rec.Principal.Email}, "", true
	}
	authn := auth.New(auth.Config{Enabled: true, ValidateToken: validate})
	mcpTS := httptest.NewServer(authn.Middleware(mcpHandler))
	t.Cleanup(mcpTS.Close)

	// --- The /ws/ui manager with the token-to-self mint (mirrors cmd/shoka) -----
	dm, err := drafts.NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := ui.NewManager(s, dm, nil)
	m.SetOAuthStore(store) // *oauthstore.Store satisfies OAuthConnectionStore
	m.SetOAuthSelfIssuer(ui.OAuthSelfIssuerFunc(func(_ *http.Request, accessTTL time.Duration) (string, time.Time, error) {
		if accessTTL <= 0 {
			accessTTL = time.Hour
		}
		rec, nerr := store.NewSeries(
			"shoka-cli",
			oauthstore.Principal{Name: "Osamu Takahashi", Email: "forte.nit@gmail.com"},
			"https://example.invalid/mcp",
			"*",
			time.Now(), accessTTL, 24*time.Hour,
		)
		if nerr != nil {
			return "", time.Time{}, nerr
		}
		return rec.AccessToken, rec.AccessExpiry, nil
	}))
	wsTS := httptest.NewServer(m)
	t.Cleanup(wsTS.Close)

	// (1) Mint the token over a real ws connection (the admin action).
	token := issueTokenOverWS(t, wsTS.URL)
	if token == "" {
		t.Fatal("mint returned an empty token")
	}

	// (2) Store it via the same client-config writer `shoka-cli auth` uses.
	if err := clientconfig.Save(clientconfig.DefaultEnvironment, &clientconfig.Config{
		Endpoint: mcpTS.URL,
		Token:    token,
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	// (3) The thin client loads the stored token and calls a read-only tool.
	cfg, err := clientconfig.Load(clientconfig.DefaultEnvironment)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess, err := mcpclient.Connect(ctx, cfg.Endpoint, cfg.Token)
	if err != nil {
		t.Fatalf("connect with stored token: %v", err)
	}
	defer func() { _ = sess.Close() }()
	res, err := sess.CallTool(ctx, "list_projects", nil)
	if err != nil {
		t.Fatalf("list_projects call: %v", err)
	}
	if res.IsError {
		t.Fatalf("list_projects returned a tool error: %v", res.Content)
	}

	// (4) A bad token is rejected by the same enforcement (connect fails).
	if _, err := mcpclient.Connect(ctx, mcpTS.URL, "not-a-valid-token"); err == nil {
		t.Fatal("connect with a bad token unexpectedly succeeded; enforcement is not gating")
	}
}

// TestEndToEndFileAdd proves `shoka-cli file add` end to end against a real
// auth-enforced MCP server (the B-45b/B-46b real-client lineage): the actual
// cmdFileAdd subcommand reads a local file, base64-encodes it, resolves the B-47
// destination, and calls the real write_file tool over a Bearer-authenticated
// Streamable-HTTP connection. It exercises the five directive cases:
//
//	(a) a UTF-8 markdown file lands byte-faithful at the resolved dest;
//	(b) a file with genuinely non-UTF-8 bytes survives intact via base64
//	    (the B-46a failure case, closed);
//	(c) a disallowed format is rejected server-side (the client surfaces it);
//	(d) a relative dest uses the config default namespace/project;
//	(e) an absolute /namespace/project/path dest is split and honoured.
func TestEndToEndFileAdd(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())

	s, err := storage.NewFSGitStorageWithOptions(dir, storage.Options{
		Identity: identity.Defaults{
			UserName:  "Osamu Takahashi",
			UserEmail: "forte.nit@gmail.com",
			AgentName: "shoka-agent",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.CreateProject("ns", "proj"); err != nil {
		t.Fatal(err)
	}

	store, err := oauthstore.Open(dir + "/oauth.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The auth-enforced MCP server exposing the real write_file tool.
	mcpSrv := mcp.NewServer(&mcp.Implementation{Name: "shoka-test", Version: "0.0.0"}, nil)
	mcp.AddTool(mcpSrv, &mcp.Tool{Name: "write_file", Description: "write a file"}, tools.WriteFileHandler(s))
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpSrv }, nil)
	validate := func(token string) (auth.Principal, auth.RejectReason, bool) {
		if token == "" {
			return auth.Principal{}, auth.ReasonMissingBearer, false
		}
		rec, lerr := store.Lookup(token, time.Now())
		if lerr != nil {
			return auth.Principal{}, auth.ReasonInvalidToken, false
		}
		return auth.Principal{Name: rec.Principal.Name, Email: rec.Principal.Email}, "", true
	}
	authn := auth.New(auth.Config{Enabled: true, ValidateToken: validate})
	mcpTS := httptest.NewServer(authn.Middleware(mcpHandler))
	t.Cleanup(mcpTS.Close)

	// A real token in the shared store; stored via the same client-config writer.
	rec, err := store.NewSeries(
		"shoka-cli",
		oauthstore.Principal{Name: "Osamu Takahashi", Email: "forte.nit@gmail.com"},
		"https://example.invalid/mcp",
		"*",
		time.Now(), time.Hour, 24*time.Hour,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := clientconfig.Save(clientconfig.DefaultEnvironment, &clientconfig.Config{
		Endpoint:         mcpTS.URL,
		Token:            rec.AccessToken,
		DefaultNamespace: "ns",
		DefaultProject:   "proj",
	}); err != nil {
		t.Fatalf("save config: %v", err)
	}

	writeLocal := func(name string, data []byte) string {
		p := dir + "/" + name
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatalf("write local fixture %s: %v", name, err)
		}
		return p
	}

	// (a)+(d) UTF-8 markdown, relative dest via config default ns/project.
	utf8Doc := []byte("# 見出し\n\nByte-faithful 本文。\n")
	if err := cmdFileAdd([]string{writeLocal("doc.md", utf8Doc), "notes/doc.md"}); err != nil {
		t.Fatalf("(a/d) file add UTF-8 relative: %v", err)
	}
	if got, _, _ := s.ReadFileWithETag("ns", "proj", "notes/doc.md"); got != string(utf8Doc) {
		t.Fatalf("(a/d) round-trip mismatch:\n got %q\nwant %q", got, utf8Doc)
	}

	// (b) Non-UTF-8 bytes survive intact via base64 — a .md path keeps it on the
	// allowed ingest list.
	rawBytes := []byte{0xff, 0xfe, 0x00, 0xe9, 0x41, 0x42}
	if err := cmdFileAdd([]string{writeLocal("raw.bin", rawBytes), "raw.md"}); err != nil {
		t.Fatalf("(b) file add non-UTF-8: %v", err)
	}
	if got, _, _ := s.ReadFileWithETag("ns", "proj", "raw.md"); got != string(rawBytes) {
		t.Fatalf("(b) non-UTF-8 round-trip mismatch:\n got %x\nwant %x", got, rawBytes)
	}

	// (e) Absolute /namespace/project/path dest.
	absDoc := []byte("absolute dest\n")
	if err := cmdFileAdd([]string{writeLocal("abs.md", absDoc), "/ns/proj/sub/abs.md"}); err != nil {
		t.Fatalf("(e) file add absolute: %v", err)
	}
	if got, _, _ := s.ReadFileWithETag("ns", "proj", "sub/abs.md"); got != string(absDoc) {
		t.Fatalf("(e) absolute dest mismatch:\n got %q\nwant %q", got, absDoc)
	}

	// (c) A disallowed format is rejected server-side; the client surfaces an error
	// and nothing is written.
	if err := cmdFileAdd([]string{writeLocal("foreign.pdf", []byte("%PDF-1.7")), "foreign.pdf"}); err == nil {
		t.Fatal("(c) disallowed format must be rejected, but file add succeeded")
	}
	if _, _, rerr := s.ReadFileWithETag("ns", "proj", "foreign.pdf"); rerr == nil {
		t.Fatal("(c) rejected ingest must not write the file")
	}
}

// issueTokenOverWS opens a real /ws/ui connection, sends OAUTH_ISSUE_SELF, and
// returns the minted access token from the response frame.
func issueTokenOverWS(t *testing.T, httpURL string) string {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(httpURL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteJSON(ui.WSMessage{Type: ui.MsgOAuthIssueSelf}); err != nil {
		t.Fatalf("ws write: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var resp ui.WSMessage
		if err := conn.ReadJSON(&resp); err != nil {
			t.Fatalf("ws read: %v", err)
		}
		switch resp.Type {
		case ui.MsgOAuthIssueSelf:
			var p ui.OAuthIssueSelfPayload
			if err := json.Unmarshal(resp.Payload, &p); err != nil {
				t.Fatalf("unmarshal issue-self payload: %v", err)
			}
			return p.AccessToken
		case ui.MsgOAuthDenied, ui.Error:
			t.Fatalf("mint refused: %s %s", resp.Type, resp.Payload)
		default:
			// Ignore any unrelated frame and keep reading for the response.
		}
	}
}
