package ui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sopranoworks/shoka/internal/identity"
	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/pkg/oauthstore"

	"github.com/sopranoworks/shoka/pkg/uiws"
)

// The OAUTH_LIST/OAUTH_REVOKE requests (the 2026-06-03 MCP OAuth (c) directive)
// are the administrator-only management surface over the (b) oauthstore's no-secret
// List and per-series Revoke. These tests exercise the request/response cycle over
// a real ws connection with a fake store and a settable admin seam.
//
// Confidentiality (directive §0): no concrete client-metadata domain or Shoka
// deployment value appears here — fixtures use RFC 2606 placeholders.

// fakeOAuthStore is an in-memory uiws.OAuthConnectionStore for the /ws/ui tests. It
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

// B-71 Stage 2d: the fake has no dynamic "domain" registration store — these stubs satisfy
// the extended uiws.OAuthConnectionStore interface for the basic OAUTH_LIST/REVOKE tests. The
// DOMAIN_* + grouping tests use a REAL *oauthstore.Store instead.
func (f *fakeOAuthStore) ListRegistrations() ([]oauthstore.RegistrationEntry, error) { return nil, nil }
func (f *fakeOAuthStore) CreateRegistration(string, string, time.Time) (oauthstore.RegistrationEntry, error) {
	return oauthstore.RegistrationEntry{}, nil
}
func (f *fakeOAuthStore) GetRegistration(string) (oauthstore.RegistrationEntry, error) {
	return oauthstore.RegistrationEntry{}, oauthstore.ErrNotFound
}
func (f *fakeOAuthStore) UpdateRegistration(oauthstore.RegistrationEntry) error { return nil }
func (f *fakeOAuthStore) DeleteRegistration(string) error                       { return nil }
func (f *fakeOAuthStore) RevokeByDomain(string) (int, error)                    { return 0, nil }
func (f *fakeOAuthStore) GenerateDomainConsent(string) (string, error)          { return "", nil }
func (f *fakeOAuthStore) DomainEntryForClient(string) (oauthstore.RegistrationEntry, bool) {
	return oauthstore.RegistrationEntry{}, false
}
func (f *fakeOAuthStore) IssueConfidentialClient(string, string, time.Duration, time.Time) (oauthstore.RegistrationEntry, string, error) {
	return oauthstore.RegistrationEntry{}, "", nil
}
func (f *fakeOAuthStore) RevokeByClientID(string) (int, error) { return 0, nil }

func (f *fakeOAuthStore) RevokeByPrincipalEmail(email string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	target := strings.ToLower(strings.TrimSpace(email))
	kept := f.series[:0:0]
	n := 0
	for _, s := range f.series {
		if strings.EqualFold(strings.TrimSpace(s.Principal.Email), target) {
			f.revoked = append(f.revoked, s.SeriesID)
			n++
			continue
		}
		kept = append(kept, s)
	}
	f.series = kept
	return n, nil
}

func (f *fakeOAuthStore) revokedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.revoked))
	copy(out, f.revoked)
	return out
}

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

// newOAuthManager wires a manager with an optional OAuth store and connects a /ws/ui
// client carrying the given session scope (B-28 stage 4: admin authorization for
// OAUTH_* is the stage-2 dispatch authzGate, not a removed admin seam). An empty scope
// = no session principal = the empty-store super-user pass-through (an admin-equivalent
// connection); a non-super-user scope (e.g. "namespace:foo:r") is denied OAUTH_* by the
// gate with a PERMISSION_DENIED frame.
func newOAuthManager(t *testing.T, scope string, store uiws.OAuthConnectionStore) *websocket.Conn {
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

func decodeOAuthList(t *testing.T, resp uiws.WSMessage) uiws.OAuthListPayload {
	t.Helper()
	if resp.Type != uiws.MsgOAuthList {
		t.Fatalf("type = %s, want OAUTH_LIST (payload=%s)", resp.Type, resp.Payload)
	}
	var out uiws.OAuthListPayload
	if err := json.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatalf("unmarshal OAUTH_LIST: %v", err)
	}
	return out
}

func decodeOAuthDenied(t *testing.T, resp uiws.WSMessage) uiws.OAuthDeniedPayload {
	t.Helper()
	if resp.Type != uiws.MsgOAuthDenied {
		t.Fatalf("type = %s, want OAUTH_DENIED (payload=%s)", resp.Type, resp.Payload)
	}
	var out uiws.OAuthDeniedPayload
	if err := json.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatalf("unmarshal OAUTH_DENIED: %v", err)
	}
	return out
}

func TestWSUI_OAuthListReturnsSummaries(t *testing.T) {
	store := &fakeOAuthStore{series: seedConnections()}
	conn := newOAuthManager(t, "", store) // default single-user admin

	resp := roundTrip(t, conn, uiws.MsgOAuthList, `{}`)
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

// TestWSUI_OAuthListSortedNewestFirstAndCarriesScope pins the 2026-06-15 admin-UI
// fixes: handleOAuthList returns connections in issued_at-DESCENDING order
// (replacing Store.List()'s random bbolt-key order) and the wire DTO carries the
// token Scope so the admin page can show it.
func TestWSUI_OAuthListSortedNewestFirstAndCarriesScope(t *testing.T) {
	base := time.Date(2026, 6, 3, 12, 0, 0, 0, time.UTC)
	// Seed deliberately OUT OF ORDER (oldest first) so a no-sort handler would fail.
	store := &fakeOAuthStore{series: []oauthstore.SeriesInfo{
		{
			SeriesID: "series-old", ClientID: "https://a.example/cimd",
			Principal: oauthstore.Principal{Name: "Op", Email: "op@example.test"},
			IssuedAt:  base, AccessExpiry: base.Add(time.Hour), Scope: "*",
		},
		{
			SeriesID: "series-new", ClientID: "https://b.example/cimd",
			Principal: oauthstore.Principal{Name: "Op", Email: "op@example.test"},
			IssuedAt:  base.Add(2 * time.Hour), AccessExpiry: base.Add(3 * time.Hour), Scope: "namespace:foo",
		},
	}}
	conn := newOAuthManager(t, "", store)

	out := decodeOAuthList(t, roundTrip(t, conn, uiws.MsgOAuthList, `{}`))
	if len(out.Connections) != 2 {
		t.Fatalf("connections = %d, want 2", len(out.Connections))
	}
	// Newest (issued later) first.
	if out.Connections[0].SeriesID != "series-new" || out.Connections[1].SeriesID != "series-old" {
		t.Fatalf("order = [%s, %s], want newest-first [series-new, series-old]",
			out.Connections[0].SeriesID, out.Connections[1].SeriesID)
	}
	// Scope rides the wire DTO, verbatim per series.
	if out.Connections[0].Scope != "namespace:foo" || out.Connections[1].Scope != "*" {
		t.Fatalf("scopes = [%q, %q], want [namespace:foo, *]",
			out.Connections[0].Scope, out.Connections[1].Scope)
	}
}

func TestWSUI_OAuthListEmptyReturnsEmptySlice(t *testing.T) {
	store := &fakeOAuthStore{} // no connections
	conn := newOAuthManager(t, "", store)

	resp := roundTrip(t, conn, uiws.MsgOAuthList, `{}`)
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
	conn := newOAuthManager(t, "", store)

	resp := roundTrip(t, conn, uiws.MsgOAuthList, `{}`)
	wire := string(resp.Payload)
	for _, secretKey := range []string{"access_token", "refresh_token", "\"code\"", "code_verifier", "code_challenge", "refresh"} {
		if strings.Contains(wire, secretKey) {
			t.Fatalf("OAUTH_LIST wire frame leaks a secret field %q: %s", secretKey, wire)
		}
	}
}

func TestWSUI_OAuthRevokeTargetsOneSeries(t *testing.T) {
	store := &fakeOAuthStore{series: seedConnections()}
	conn := newOAuthManager(t, "", store)

	target := "series-aaaa-0000-1111-2222-333344445555"
	resp := roundTrip(t, conn, uiws.MsgOAuthRevoke, `{"series_id":"`+target+`"}`)
	if resp.Type != uiws.MsgOAuthRevoke {
		t.Fatalf("type = %s, want OAUTH_REVOKE ack (payload=%s)", resp.Type, resp.Payload)
	}
	var ack uiws.OAuthRevokePayload
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
	out := decodeOAuthList(t, roundTrip(t, conn, uiws.MsgOAuthList, `{}`))
	if len(out.Connections) != 1 || out.Connections[0].SeriesID == target {
		t.Fatalf("after revoke, connections = %+v; want the other series intact", out.Connections)
	}
}

func TestWSUI_OAuthRevokeEmptyIDIsError(t *testing.T) {
	store := &fakeOAuthStore{series: seedConnections()}
	conn := newOAuthManager(t, "", store)

	resp := roundTrip(t, conn, uiws.MsgOAuthRevoke, `{"series_id":""}`)
	if resp.Type != uiws.Error {
		t.Fatalf("type = %s, want ERROR for an absent series_id", resp.Type)
	}
	if len(store.revokedIDs()) != 0 {
		t.Fatalf("an absent series_id must not revoke anything; revoked=%v", store.revokedIDs())
	}
}

// A non-super-user session is denied OAUTH_* by the SOLE admin gate now — the stage-2
// dispatch authzGate — with a PERMISSION_DENIED frame, BEFORE the handler runs (the
// retired adminGate/singleUserAdmin seam's OAUTH_DENIED "forbidden" is gone).
func TestWSUI_OAuthListRefusedForNonAdmin(t *testing.T) {
	store := &fakeOAuthStore{series: seedConnections()}
	conn := newOAuthManager(t, "namespace:foo:r", store)

	resp := roundTrip(t, conn, uiws.MsgOAuthList, `{}`)
	if resp.Type != uiws.MsgPermissionDenied {
		t.Fatalf("type = %s, want PERMISSION_DENIED (the dispatch authz gate)", resp.Type)
	}
}

func TestWSUI_OAuthRevokeRefusedForNonAdmin(t *testing.T) {
	store := &fakeOAuthStore{series: seedConnections()}
	conn := newOAuthManager(t, "namespace:foo:r", store)

	target := "series-aaaa-0000-1111-2222-333344445555"
	resp := roundTrip(t, conn, uiws.MsgOAuthRevoke, `{"series_id":"`+target+`"}`)
	if resp.Type != uiws.MsgPermissionDenied {
		t.Fatalf("type = %s, want PERMISSION_DENIED", resp.Type)
	}
	// The gate refuses BEFORE the handler: nothing revoked.
	if len(store.revokedIDs()) != 0 {
		t.Fatalf("non-admin revoke must not reach the store; revoked=%v", store.revokedIDs())
	}
}

func TestWSUI_OAuthRefusedWhenOAuthDisabled(t *testing.T) {
	// No store wired (OAuth off) but admin is the default single-user (true), so the
	// refusal must be the distinct "oauth_disabled" reason, not "forbidden".
	conn := newOAuthManager(t, "", nil)

	list := decodeOAuthDenied(t, roundTrip(t, conn, uiws.MsgOAuthList, `{}`))
	if list.Reason != "oauth_disabled" {
		t.Fatalf("OAUTH_LIST reason = %q, want oauth_disabled", list.Reason)
	}
	revoke := decodeOAuthDenied(t, roundTrip(t, conn, uiws.MsgOAuthRevoke, `{"series_id":"x"}`))
	if revoke.Reason != "oauth_disabled" {
		t.Fatalf("OAUTH_REVOKE reason = %q, want oauth_disabled", revoke.Reason)
	}
}

// Authorization is checked before capability: a non-admin is PERMISSION_DENIED by the
// dispatch gate even when OAuth is also disabled, so a non-admin never learns the OAuth
// state (the capability check never runs).
func TestWSUI_OAuthNonAdminTakesPrecedenceOverDisabled(t *testing.T) {
	conn := newOAuthManager(t, "namespace:foo:r", nil) // non-admin AND no store

	resp := roundTrip(t, conn, uiws.MsgOAuthList, `{}`)
	if resp.Type != uiws.MsgPermissionDenied {
		t.Fatalf("type = %s, want PERMISSION_DENIED (authz before capability)", resp.Type)
	}
}
