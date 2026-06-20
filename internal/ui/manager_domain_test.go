package ui

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/storage/oauthstore"
)

// B-71 Stage 2d: the DOMAIN_* ws ops + domain-grouped OAUTH_LIST, against a REAL oauthstore
// (the fake has no registration store).

func realOAuthStore(t *testing.T) *oauthstore.Store {
	t.Helper()
	s, err := oauthstore.Open(filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatalf("open oauth store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func decodeDomainList(t *testing.T, resp WSMessage) DomainListPayload {
	t.Helper()
	if resp.Type != MsgDomainList {
		t.Fatalf("type = %s, want DOMAIN_LIST (payload=%s)", resp.Type, resp.Payload)
	}
	var out DomainListPayload
	if err := json.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatalf("unmarshal DOMAIN_LIST: %v", err)
	}
	return out
}

// TestWSUI_DomainCRUD: create → list → update (TTL + clear consent) → delete, through the ws
// ops. The per-domain consent VALUE never appears in any payload (only a set/unset indicator).
func TestWSUI_DomainCRUD(t *testing.T) {
	const secret = "the-per-domain-consent-secret"
	conn := newOAuthManager(t, "", realOAuthStore(t))

	// CREATE with TTL + consent.
	resp := roundTrip(t, conn, MsgDomainCreate,
		fmt.Sprintf(`{"domain":"connector.example","access_ttl_seconds":3600,"refresh_ttl_seconds":86400,"consent":%q}`, secret))
	if resp.Type != MsgDomainCreate {
		t.Fatalf("create type = %s (%s)", resp.Type, resp.Payload)
	}
	if strings.Contains(string(resp.Payload), secret) {
		t.Fatal("the consent secret must NEVER appear in a DOMAIN payload")
	}
	var created DomainInfo
	_ = json.Unmarshal(resp.Payload, &created)
	if created.Domain != "connector.example" || created.AccessTTLSeconds != 3600 || created.RefreshTTLSeconds != 86400 || !created.ConsentSet {
		t.Fatalf("created: %+v", created)
	}
	id := created.ID

	// LIST shows it; consent SET indicator true; no consent value.
	resp = roundTrip(t, conn, MsgDomainList, `{}`)
	if strings.Contains(string(resp.Payload), secret) {
		t.Fatal("DOMAIN_LIST must not carry the consent secret")
	}
	list := decodeDomainList(t, resp)
	if len(list.Domains) != 1 || list.Domains[0].ID != id || !list.Domains[0].ConsentSet {
		t.Fatalf("list: %+v", list.Domains)
	}

	// UPDATE: change the access TTL, leave refresh unset (0 ⇒ global default), CLEAR consent.
	resp = roundTrip(t, conn, MsgDomainUpdate,
		fmt.Sprintf(`{"id":%q,"access_ttl_seconds":7200,"refresh_ttl_seconds":0,"set_consent":""}`, id))
	if resp.Type != MsgDomainUpdate {
		t.Fatalf("update type = %s (%s)", resp.Type, resp.Payload)
	}
	var updated DomainInfo
	_ = json.Unmarshal(resp.Payload, &updated)
	if updated.AccessTTLSeconds != 7200 || updated.RefreshTTLSeconds != 0 || updated.ConsentSet {
		t.Fatalf("updated: %+v", updated)
	}

	// DELETE.
	resp = roundTrip(t, conn, MsgDomainDelete, fmt.Sprintf(`{"id":%q}`, id))
	if resp.Type != MsgDomainDelete {
		t.Fatalf("delete type = %s (%s)", resp.Type, resp.Payload)
	}
	if got := decodeDomainList(t, roundTrip(t, conn, MsgDomainList, `{}`)); len(got.Domains) != 0 {
		t.Fatalf("deleted domain still listed: %+v", got.Domains)
	}
}

// TestWSUI_DomainDeleteRevokesTokens: deleting a domain revokes its live tokens (the stated
// Stage 2d policy) — the CIMD/DCR series under it are cut; a self-issued series is untouched.
func TestWSUI_DomainDeleteRevokesTokens(t *testing.T) {
	store := realOAuthStore(t)
	now := time.Now()
	p := oauthstore.Principal{Name: "Op", Email: "op@example.test"}
	dom, err := store.CreateRegistration(oauthstore.RegistrationModeDomain, "connector.example", now)
	if err != nil {
		t.Fatalf("create domain: %v", err)
	}
	// A CIMD series under the domain + a self-issued series (not under any domain).
	under, _ := store.NewSeries("https://connector.example/meta", p, "res", "*", now, time.Hour, 24*time.Hour)
	self, _ := store.NewSeries(oauthstore.SelfIssuedClientID, p, "res", "*", now, time.Hour, 24*time.Hour)

	conn := newOAuthManager(t, "", store)
	resp := roundTrip(t, conn, MsgDomainDelete, fmt.Sprintf(`{"id":%q}`, dom.ID))
	var del DomainDeletePayload
	_ = json.Unmarshal(resp.Payload, &del)
	if del.RevokedTokens != 1 {
		t.Fatalf("delete must revoke the 1 token under the domain; got %d", del.RevokedTokens)
	}
	// The under-domain token no longer resolves; the self-issued one still does.
	if _, err := store.Lookup(under.AccessToken, now); err == nil {
		t.Fatal("the under-domain token must be revoked on domain delete")
	}
	if _, err := store.Lookup(self.AccessToken, now); err != nil {
		t.Fatalf("the self-issued token must survive a domain delete: %v", err)
	}
}

// TestWSUI_OAuthListGroupsByDomain: each connection is tagged with the trusted-domain entry it
// groups under; a self-issued connection has an empty domain (the separate section).
func TestWSUI_OAuthListGroupsByDomain(t *testing.T) {
	store := realOAuthStore(t)
	now := time.Now()
	p := oauthstore.Principal{Name: "Op", Email: "op@example.test"}
	if _, err := store.CreateRegistration(oauthstore.RegistrationModeDomain, "connector.example", now); err != nil {
		t.Fatalf("create domain: %v", err)
	}
	// A CIMD connection whose host is a SUBDOMAIN of the entry (groups under it), + self-issued.
	_, _ = store.NewSeries("https://sub.connector.example/meta", p, "res", "*", now, time.Hour, 24*time.Hour)
	_, _ = store.NewSeries(oauthstore.SelfIssuedClientID, p, "res", "*", now, time.Hour, 24*time.Hour)

	conn := newOAuthManager(t, "", store)
	list := decodeOAuthList(t, roundTrip(t, conn, MsgOAuthList, `{}`))
	if len(list.Connections) != 2 {
		t.Fatalf("connections = %d, want 2", len(list.Connections))
	}
	for _, c := range list.Connections {
		switch c.ClientID {
		case "https://sub.connector.example/meta":
			if c.Domain != "connector.example" {
				t.Fatalf("CIMD subdomain connection must group under connector.example; got %q", c.Domain)
			}
		case oauthstore.SelfIssuedClientID:
			if c.Domain != "" {
				t.Fatalf("self-issued connection must have an empty domain; got %q", c.Domain)
			}
		}
	}
}

// TestWSUI_DomainOpsAdminGated: a non-admin scope is refused the DOMAIN_* ops by the dispatch
// authz gate (PERMISSION_DENIED), before the handler runs.
func TestWSUI_DomainOpsAdminGated(t *testing.T) {
	conn := newOAuthManager(t, "namespace:foo:r", realOAuthStore(t))
	for _, mt := range []MessageType{MsgDomainList, MsgDomainCreate, MsgDomainUpdate, MsgDomainDelete} {
		resp := roundTrip(t, conn, mt, `{}`)
		if resp.Type != MsgPermissionDenied {
			t.Fatalf("%s by a non-admin: type = %s, want PERMISSION_DENIED", mt, resp.Type)
		}
	}
}
