package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/shoka/mcp-server/internal/auth"
	"github.com/shoka/mcp-server/internal/clientconfig"
	"github.com/shoka/mcp-server/internal/drafts"
	"github.com/shoka/mcp-server/internal/identity"
	"github.com/shoka/mcp-server/internal/mcpclient"
	"github.com/shoka/mcp-server/internal/storage"
	"github.com/shoka/mcp-server/internal/storage/oauthstore"
	"github.com/shoka/mcp-server/internal/tools"
	"github.com/shoka/mcp-server/internal/ui"
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
	validate := func(token string) (auth.Principal, bool) {
		if token == "" {
			return auth.Principal{}, false
		}
		rec, lerr := store.Lookup(token, time.Now())
		if lerr != nil {
			return auth.Principal{}, false
		}
		return auth.Principal{Name: rec.Principal.Name, Email: rec.Principal.Email}, true
	}
	authn := auth.New(auth.Config{Enabled: true, ValidateToken: validate})
	mcpTS := httptest.NewServer(authn.Middleware(mcpHandler))
	t.Cleanup(mcpTS.Close)

	// --- The /ws/ui manager with the token-to-self mint (mirrors cmd/server) -----
	dm, err := drafts.NewManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	m := ui.NewManager(s, dm, nil)
	m.SetOAuthStore(store) // *oauthstore.Store satisfies OAuthConnectionStore
	m.SetOAuthSelfIssuer(ui.OAuthSelfIssuerFunc(func(*http.Request) (string, time.Time, error) {
		rec, nerr := store.NewSeries(
			"shoka-cli",
			oauthstore.Principal{Name: "Osamu Takahashi", Email: "forte.nit@gmail.com"},
			"https://example.invalid/mcp",
			time.Now(), time.Hour, 24*time.Hour,
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
