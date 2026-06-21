package ui

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/pkg/oauthstore"

	"github.com/sopranoworks/shoka/pkg/uiws"
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

func decodeDomainList(t *testing.T, resp uiws.WSMessage) uiws.DomainListPayload {
	t.Helper()
	if resp.Type != uiws.MsgDomainList {
		t.Fatalf("type = %s, want DOMAIN_LIST (payload=%s)", resp.Type, resp.Payload)
	}
	var out uiws.DomainListPayload
	if err := json.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatalf("unmarshal DOMAIN_LIST: %v", err)
	}
	return out
}

// TestWSUI_DomainCRUD: create → generate consent → list → update (TTL) → delete, through the ws
// ops. The 2026-06-20 model: a domain is created with NO consent; the operator GENERATES a
// plaintext consent value that is returned + listed (operator-readable, intentionally).
func TestWSUI_DomainCRUD(t *testing.T) {
	conn := newOAuthManager(t, "", realOAuthStore(t))

	// CREATE with TTL, no consent.
	resp := roundTrip(t, conn, uiws.MsgDomainCreate,
		`{"domain":"connector.example","access_ttl_seconds":3600,"refresh_ttl_seconds":86400}`)
	if resp.Type != uiws.MsgDomainCreate {
		t.Fatalf("create type = %s (%s)", resp.Type, resp.Payload)
	}
	var created uiws.DomainInfo
	_ = json.Unmarshal(resp.Payload, &created)
	if created.Domain != "connector.example" || created.AccessTTLSeconds != 3600 || created.RefreshTTLSeconds != 86400 || created.Consent != "" {
		t.Fatalf("created (should have no consent yet): %+v", created)
	}
	id := created.ID

	// GENERATE CONSENT — returns a plaintext value (operator-readable).
	resp = roundTrip(t, conn, uiws.MsgDomainGenerateConsent, fmt.Sprintf(`{"id":%q}`, id))
	if resp.Type != uiws.MsgDomainGenerateConsent {
		t.Fatalf("generate type = %s (%s)", resp.Type, resp.Payload)
	}
	var gen uiws.DomainGenerateConsentPayload
	_ = json.Unmarshal(resp.Payload, &gen)
	if gen.ID != id || gen.Consent == "" {
		t.Fatalf("generate must return a plaintext consent value: %+v", gen)
	}
	consent := gen.Consent

	// Regenerate yields a DIFFERENT value (re-roll).
	resp = roundTrip(t, conn, uiws.MsgDomainGenerateConsent, fmt.Sprintf(`{"id":%q}`, id))
	var gen2 uiws.DomainGenerateConsentPayload
	_ = json.Unmarshal(resp.Payload, &gen2)
	if gen2.Consent == "" || gen2.Consent == consent {
		t.Fatalf("regenerate must re-roll the value: %q vs %q", consent, gen2.Consent)
	}

	// LIST shows the domain WITH its plaintext consent value (the whole point — readable).
	list := decodeDomainList(t, roundTrip(t, conn, uiws.MsgDomainList, `{}`))
	if len(list.Domains) != 1 || list.Domains[0].ID != id || list.Domains[0].Consent != gen2.Consent {
		t.Fatalf("list must carry the plaintext consent value: %+v", list.Domains)
	}

	// UPDATE: change the access TTL, leave refresh unset (0 ⇒ global default); consent untouched.
	resp = roundTrip(t, conn, uiws.MsgDomainUpdate,
		fmt.Sprintf(`{"id":%q,"access_ttl_seconds":7200,"refresh_ttl_seconds":0}`, id))
	if resp.Type != uiws.MsgDomainUpdate {
		t.Fatalf("update type = %s (%s)", resp.Type, resp.Payload)
	}
	var updated uiws.DomainInfo
	_ = json.Unmarshal(resp.Payload, &updated)
	if updated.AccessTTLSeconds != 7200 || updated.RefreshTTLSeconds != 0 || updated.Consent != gen2.Consent {
		t.Fatalf("updated (consent must survive a TTL edit): %+v", updated)
	}

	// DELETE.
	resp = roundTrip(t, conn, uiws.MsgDomainDelete, fmt.Sprintf(`{"id":%q}`, id))
	if resp.Type != uiws.MsgDomainDelete {
		t.Fatalf("delete type = %s (%s)", resp.Type, resp.Payload)
	}
	if got := decodeDomainList(t, roundTrip(t, conn, uiws.MsgDomainList, `{}`)); len(got.Domains) != 0 {
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
	resp := roundTrip(t, conn, uiws.MsgDomainDelete, fmt.Sprintf(`{"id":%q}`, dom.ID))
	var del uiws.DomainDeletePayload
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
	list := decodeOAuthList(t, roundTrip(t, conn, uiws.MsgOAuthList, `{}`))
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
	for _, mt := range []MessageType{uiws.MsgDomainList, uiws.MsgDomainCreate, uiws.MsgDomainUpdate, uiws.MsgDomainDelete, uiws.MsgDomainGenerateConsent} {
		resp := roundTrip(t, conn, mt, `{}`)
		if resp.Type != uiws.MsgPermissionDenied {
			t.Fatalf("%s by a non-admin: type = %s, want PERMISSION_DENIED", mt, resp.Type)
		}
	}
}
