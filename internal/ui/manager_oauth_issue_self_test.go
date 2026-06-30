package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sopranoworks/shoka/internal/identity"
	"github.com/sopranoworks/shoka/internal/storage"

	"github.com/sopranoworks/shoka/pkg/uiws"
)

// OAUTH_ISSUE_SELF (B-46b §2.2) is the admin-gated "token to self" mint: the only
// place a secret access token deliberately crosses /ws/ui, so the operator can
// paste it into their CLI client config. These tests exercise the request/response
// cycle over a real ws connection with a fake issuer and the same admin/store
// gating as List/Revoke.

// newOAuthIssueSelfManager wires an uiws.OAuthConnectionStore (so the oauth!=nil capability
// check passes) and a self-issuer, connecting a /ws/ui client at the given session
// scope (B-28 stage 4: admin authorization is the dispatch authzGate; "" = empty-store
// super-user, a non-super-user scope is denied by the gate).
func newOAuthIssueSelfManager(t *testing.T, scope string, store uiws.OAuthConnectionStore, issuer uiws.OAuthSelfIssuer) *websocket.Conn {
	t.Helper()
	dir := t.TempDir()
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

	m := NewManager(s, mustDrafts(t, dir), nil)
	if store != nil {
		m.SetOAuthStore(store)
	}
	if issuer != nil {
		m.SetOAuthSelfIssuer(issuer)
	}
	var h http.Handler = m
	if scope != "" {
		h = withScope(scope, m)
	}

	server := httptest.NewServer(h)
	t.Cleanup(server.Close)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func TestWSUI_OAuthIssueSelfReturnsToken(t *testing.T) {
	const want = "fresh-access-token-value"
	exp := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)
	var gotReq bool
	var gotTTL time.Duration
	issuer := uiws.OAuthSelfIssuerFunc(func(r *http.Request, _, _ string, accessTTL time.Duration, _ map[string]any) (string, time.Time, error) {
		gotReq = r != nil // the request must reach the issuer (for resource derivation)
		gotTTL = accessTTL
		return want, exp, nil
	})
	conn := newOAuthIssueSelfManager(t, "", &fakeOAuthStore{}, issuer)

	resp := roundTrip(t, conn, uiws.MsgOAuthIssueSelf, `{}`)
	if resp.Type != uiws.MsgOAuthIssueSelf {
		t.Fatalf("type = %s, want OAUTH_ISSUE_SELF (payload=%s)", resp.Type, resp.Payload)
	}
	var out uiws.OAuthIssueSelfPayload
	if err := json.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.AccessToken != want {
		t.Fatalf("access_token = %q, want %q", out.AccessToken, want)
	}
	if !out.AccessExpiry.Equal(exp) {
		t.Fatalf("access_expiry = %v, want %v", out.AccessExpiry, exp)
	}
	if !gotReq {
		t.Fatal("issuer did not receive the request")
	}
	// An empty payload ⇒ the 0 "use the finite default" sentinel reaches the issuer.
	if gotTTL != 0 {
		t.Fatalf("empty payload must pass 0 (default sentinel), got %v", gotTTL)
	}
}

// TestWSUI_OAuthIssueSelfThreadsChosenExpiry (B-71 Stage 4): the operator's chosen per-issuance
// finite expiry reaches the issuer; a NEGATIVE value is rejected. RED proof: drop the
// validity_seconds threading (always pass the old fixed TTL) → gotTTL != the chosen value → fail.
func TestWSUI_OAuthIssueSelfThreadsChosenExpiry(t *testing.T) {
	var gotTTL time.Duration
	issuer := uiws.OAuthSelfIssuerFunc(func(_ *http.Request, _, _ string, accessTTL time.Duration, _ map[string]any) (string, time.Time, error) {
		gotTTL = accessTTL
		return "tok", time.Unix(1, 0), nil
	})
	conn := newOAuthIssueSelfManager(t, "", &fakeOAuthStore{}, issuer)

	// A chosen 7-day expiry is threaded through to the issuer.
	if resp := roundTrip(t, conn, uiws.MsgOAuthIssueSelf, `{"validity_seconds":604800}`); resp.Type != uiws.MsgOAuthIssueSelf {
		t.Fatalf("issue with chosen expiry: %s (%s)", resp.Type, resp.Payload)
	}
	if gotTTL != 7*24*time.Hour {
		t.Fatalf("chosen expiry not threaded: got %v, want 168h", gotTTL)
	}

	// A negative validity is rejected (no indefinite) and the issuer is NOT called.
	gotTTL = -1
	if resp := roundTrip(t, conn, uiws.MsgOAuthIssueSelf, `{"validity_seconds":-5}`); resp.Type != uiws.Error {
		t.Fatalf("a negative validity must error, got %s", resp.Type)
	}
	if gotTTL != -1 {
		t.Fatal("the issuer must not be called for a rejected (negative) validity")
	}
}

func TestWSUI_OAuthIssueSelfRefusedForNonAdmin(t *testing.T) {
	var called bool
	issuer := uiws.OAuthSelfIssuerFunc(func(*http.Request, string, string, time.Duration, map[string]any) (string, time.Time, error) {
		called = true
		return "should-not-be-minted", time.Time{}, nil
	})
	conn := newOAuthIssueSelfManager(t, "namespace:foo:r", &fakeOAuthStore{}, issuer)

	resp := roundTrip(t, conn, uiws.MsgOAuthIssueSelf, `{}`)
	if resp.Type != uiws.MsgPermissionDenied {
		t.Fatalf("type = %s, want PERMISSION_DENIED (the dispatch authz gate)", resp.Type)
	}
	// The gate refuses BEFORE minting.
	if called {
		t.Fatal("non-admin reached the issuer; it must be gated before minting")
	}
}

func TestWSUI_OAuthIssueSelfRefusedWhenDisabled(t *testing.T) {
	// No store and no issuer wired (OAuth off) but admin is the default single-user
	// (true), so the refusal must be "oauth_disabled", not "forbidden".
	conn := newOAuthIssueSelfManager(t, "", nil, nil)

	denied := decodeOAuthDenied(t, roundTrip(t, conn, uiws.MsgOAuthIssueSelf, `{}`))
	if denied.Reason != "oauth_disabled" {
		t.Fatalf("reason = %q, want oauth_disabled", denied.Reason)
	}
}

func TestWSUI_OAuthIssueSelfDeniedWhenIssuerMissing(t *testing.T) {
	// A store is wired (the capability check passes) but no issuer — a defensive path: the
	// action reports oauth_disabled rather than nil-panicking.
	conn := newOAuthIssueSelfManager(t, "", &fakeOAuthStore{}, nil)

	denied := decodeOAuthDenied(t, roundTrip(t, conn, uiws.MsgOAuthIssueSelf, `{}`))
	if denied.Reason != "oauth_disabled" {
		t.Fatalf("reason = %q, want oauth_disabled", denied.Reason)
	}
}
