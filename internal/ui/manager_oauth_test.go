package ui

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sopranoworks/shoka/internal/identity"
	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/internal/storage/oauthstore"
)

// The OAUTH_LIST/OAUTH_REVOKE requests (the 2026-06-03 MCP OAuth (c) directive)
// are the administrator-only management surface over the (b) oauthstore's no-secret
// List and per-series Revoke. These tests exercise the request/response cycle over
// a real ws connection with a fake store and a settable admin seam.
//
// Confidentiality (directive §0): no concrete client-metadata domain or Shoka
// deployment value appears here — fixtures use RFC 2606 placeholders.

// fakeOAuthStore is an in-memory OAuthConnectionStore for the /ws/ui tests. It
// deliberately models the real store's contract: List returns no-secret
// SeriesInfo (it CANNOT carry a token — there is no field for one), and Revoke is
// idempotent and drops exactly one series.
type fakeOAuthStore struct {
	mu      sync.Mutex
	series  []oauthstore.SeriesInfo
	revoked []string
	listErr error
}

func (f *fakeOAuthStore) List() ([]oauthstore.SeriesInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]oauthstore.SeriesInfo, len(f.series))
	copy(out, f.series)
	return out, nil
}

func (f *fakeOAuthStore) Revoke(seriesID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, seriesID)
	kept := f.series[:0:0]
	for _, s := range f.series {
		if s.SeriesID != seriesID {
			kept = append(kept, s)
		}
	}
	f.series = kept
	return nil
}

func (f *fakeOAuthStore) revokedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.revoked))
	copy(out, f.revoked)
	return out
}

// denyAdmin is a non-admin AdminAuthorizer for exercising the server-side gate.
type denyAdmin struct{}

func (denyAdmin) IsAdmin() bool { return false }

// seedConnections returns two placeholder connections. The client_id values are
// RFC 2606 example domains (NOT a real client metadata domain — §0(b)).
func seedConnections() []oauthstore.SeriesInfo {
	now := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	return []oauthstore.SeriesInfo{
		{
			SeriesID:     "series-aaaa-0000-1111-2222-333344445555",
			ClientID:     "https://connector.example.com/.well-known/client-metadata",
			Principal:    oauthstore.Principal{Name: "Op Erator", Email: "op@example.test"},
			Resource:     "https://shoka.example/mcp",
			IssuedAt:     now,
			AccessExpiry: now.Add(time.Hour),
		},
		{
			SeriesID:     "series-bbbb-6666-7777-8888-99990000aaaa",
			ClientID:     "https://other-client.example.org/cimd",
			Principal:    oauthstore.Principal{Name: "Op Erator", Email: "op@example.test"},
			Resource:     "https://shoka.example/mcp",
			IssuedAt:     now,
			AccessExpiry: now.Add(time.Hour),
		},
	}
}

func newOAuthManager(t *testing.T, admin AdminAuthorizer, store OAuthConnectionStore) *websocket.Conn {
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
	m.SetAdminAuthorizer(admin) // nil is ignored (keeps the single-user default)

	server := httptest.NewServer(m)
	t.Cleanup(server.Close)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func decodeOAuthList(t *testing.T, resp WSMessage) OAuthListPayload {
	t.Helper()
	if resp.Type != MsgOAuthList {
		t.Fatalf("type = %s, want OAUTH_LIST (payload=%s)", resp.Type, resp.Payload)
	}
	var out OAuthListPayload
	if err := json.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatalf("unmarshal OAUTH_LIST: %v", err)
	}
	return out
}

func decodeOAuthDenied(t *testing.T, resp WSMessage) OAuthDeniedPayload {
	t.Helper()
	if resp.Type != MsgOAuthDenied {
		t.Fatalf("type = %s, want OAUTH_DENIED (payload=%s)", resp.Type, resp.Payload)
	}
	var out OAuthDeniedPayload
	if err := json.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatalf("unmarshal OAUTH_DENIED: %v", err)
	}
	return out
}

func TestWSUI_OAuthListReturnsSummaries(t *testing.T) {
	store := &fakeOAuthStore{series: seedConnections()}
	conn := newOAuthManager(t, nil, store) // default single-user admin

	resp := roundTrip(t, conn, MsgOAuthList, `{}`)
	out := decodeOAuthList(t, resp)

	if len(out.Connections) != 2 {
		t.Fatalf("connections = %d, want 2 (%+v)", len(out.Connections), out.Connections)
	}
	c := out.Connections[0]
	if c.SeriesID != "series-aaaa-0000-1111-2222-333344445555" {
		t.Errorf("series_id = %q", c.SeriesID)
	}
	if c.SeriesIDShort != "series-a" {
		t.Errorf("series_id_short = %q, want an 8-char prefix", c.SeriesIDShort)
	}
	if c.ClientID == "" || c.PrincipalEmail == "" {
		t.Errorf("connection summary missing client/principal: %+v", c)
	}
}

func TestWSUI_OAuthListEmptyReturnsEmptySlice(t *testing.T) {
	store := &fakeOAuthStore{} // no connections
	conn := newOAuthManager(t, nil, store)

	resp := roundTrip(t, conn, MsgOAuthList, `{}`)
	out := decodeOAuthList(t, resp)

	if len(out.Connections) != 0 {
		t.Fatalf("connections = %d, want 0", len(out.Connections))
	}
	// The wire shape must be [] not null, so the empty-state client iterates safely.
	if got := string(mustField(t, resp.Payload, "connections")); got != "[]" {
		t.Fatalf("connections field = %s, want []", got)
	}
}

// The directive's non-negotiable constraint: NO secrets cross the wire. The store
// view structurally cannot carry a token, but this guards the DTO against ever
// reintroducing one (e.g. a future widening that leaks a field).
func TestWSUI_OAuthListCarriesNoSecretFields(t *testing.T) {
	store := &fakeOAuthStore{series: seedConnections()}
	conn := newOAuthManager(t, nil, store)

	resp := roundTrip(t, conn, MsgOAuthList, `{}`)
	wire := string(resp.Payload)
	for _, secretKey := range []string{"access_token", "refresh_token", "\"code\"", "code_verifier", "code_challenge", "refresh"} {
		if strings.Contains(wire, secretKey) {
			t.Fatalf("OAUTH_LIST wire frame leaks a secret field %q: %s", secretKey, wire)
		}
	}
}

func TestWSUI_OAuthRevokeTargetsOneSeries(t *testing.T) {
	store := &fakeOAuthStore{series: seedConnections()}
	conn := newOAuthManager(t, nil, store)

	target := "series-aaaa-0000-1111-2222-333344445555"
	resp := roundTrip(t, conn, MsgOAuthRevoke, `{"series_id":"`+target+`"}`)
	if resp.Type != MsgOAuthRevoke {
		t.Fatalf("type = %s, want OAUTH_REVOKE ack (payload=%s)", resp.Type, resp.Payload)
	}
	var ack OAuthRevokePayload
	if err := json.Unmarshal(resp.Payload, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.SeriesID != target || ack.Status != "ok" {
		t.Fatalf("ack = %+v, want {%s ok}", ack, target)
	}
	if got := store.revokedIDs(); len(got) != 1 || got[0] != target {
		t.Fatalf("revoked = %v, want exactly [%s]", got, target)
	}
	// The other series survives: a follow-up LIST shows exactly one connection.
	out := decodeOAuthList(t, roundTrip(t, conn, MsgOAuthList, `{}`))
	if len(out.Connections) != 1 || out.Connections[0].SeriesID == target {
		t.Fatalf("after revoke, connections = %+v; want the other series intact", out.Connections)
	}
}

func TestWSUI_OAuthRevokeEmptyIDIsError(t *testing.T) {
	store := &fakeOAuthStore{series: seedConnections()}
	conn := newOAuthManager(t, nil, store)

	resp := roundTrip(t, conn, MsgOAuthRevoke, `{"series_id":""}`)
	if resp.Type != Error {
		t.Fatalf("type = %s, want ERROR for an absent series_id", resp.Type)
	}
	if len(store.revokedIDs()) != 0 {
		t.Fatalf("an absent series_id must not revoke anything; revoked=%v", store.revokedIDs())
	}
}

func TestWSUI_OAuthListRefusedForNonAdmin(t *testing.T) {
	store := &fakeOAuthStore{series: seedConnections()}
	conn := newOAuthManager(t, denyAdmin{}, store)

	denied := decodeOAuthDenied(t, roundTrip(t, conn, MsgOAuthList, `{}`))
	if denied.Reason != "forbidden" {
		t.Fatalf("reason = %q, want forbidden", denied.Reason)
	}
}

func TestWSUI_OAuthRevokeRefusedForNonAdmin(t *testing.T) {
	store := &fakeOAuthStore{series: seedConnections()}
	conn := newOAuthManager(t, denyAdmin{}, store)

	target := "series-aaaa-0000-1111-2222-333344445555"
	denied := decodeOAuthDenied(t, roundTrip(t, conn, MsgOAuthRevoke, `{"series_id":"`+target+`"}`))
	if denied.Reason != "forbidden" {
		t.Fatalf("reason = %q, want forbidden", denied.Reason)
	}
	// The authoritative gate refuses BEFORE touching the store: nothing revoked.
	if len(store.revokedIDs()) != 0 {
		t.Fatalf("non-admin revoke must not reach the store; revoked=%v", store.revokedIDs())
	}
}

func TestWSUI_OAuthRefusedWhenOAuthDisabled(t *testing.T) {
	// No store wired (OAuth off) but admin is the default single-user (true), so the
	// refusal must be the distinct "oauth_disabled" reason, not "forbidden".
	conn := newOAuthManager(t, nil, nil)

	list := decodeOAuthDenied(t, roundTrip(t, conn, MsgOAuthList, `{}`))
	if list.Reason != "oauth_disabled" {
		t.Fatalf("OAUTH_LIST reason = %q, want oauth_disabled", list.Reason)
	}
	revoke := decodeOAuthDenied(t, roundTrip(t, conn, MsgOAuthRevoke, `{"series_id":"x"}`))
	if revoke.Reason != "oauth_disabled" {
		t.Fatalf("OAUTH_REVOKE reason = %q, want oauth_disabled", revoke.Reason)
	}
}

// Authorization is checked before capability: a non-admin gets "forbidden" even
// when OAuth is also disabled, so a non-admin never learns the OAuth state.
func TestWSUI_OAuthNonAdminTakesPrecedenceOverDisabled(t *testing.T) {
	conn := newOAuthManager(t, denyAdmin{}, nil) // non-admin AND no store

	denied := decodeOAuthDenied(t, roundTrip(t, conn, MsgOAuthList, `{}`))
	if denied.Reason != "forbidden" {
		t.Fatalf("reason = %q, want forbidden (authz before capability)", denied.Reason)
	}
}
