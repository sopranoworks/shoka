package ui

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/pkg/oauthstore"
)

// B-71 Stage 3: the CLIENT_* ws ops (confidential-client management) against a REAL oauthstore.
// The raw secret crosses the wire ONCE (the issue response) and never in LIST; admin-gated.

// TestWSUI_ConfidentialCRUD: issue → list (no secret) → revoke, through the ws ops. The secret is
// returned ONCE on issue and never by LIST. RED proof: have LIST include the secret/hash → it
// appears in the LIST payload → this test fails.
func TestWSUI_ConfidentialCRUD(t *testing.T) {
	conn := newOAuthManager(t, "", realOAuthStore(t))

	// ISSUE — the raw secret is returned exactly once here.
	resp := roundTrip(t, conn, MsgClientIssue, `{"scope":"namespace:foo:rw","validity_seconds":3600}`)
	if resp.Type != MsgClientIssue {
		t.Fatalf("issue type = %s (%s)", resp.Type, resp.Payload)
	}
	var issued ConfidentialIssuePayload
	if err := json.Unmarshal(resp.Payload, &issued); err != nil {
		t.Fatalf("unmarshal issue: %v", err)
	}
	if issued.ClientSecret == "" {
		t.Fatal("the raw client secret must be returned once at issuance")
	}
	if issued.ClientID == "" || issued.Scope != "namespace:foo:rw" || issued.ExpiresAt == "" {
		t.Fatalf("issued shape wrong: %+v", issued.ConfidentialClientInfo)
	}
	secret := issued.ClientSecret
	id := issued.ID

	// LIST — the secret NEVER appears, and there is no client_secret field at all.
	resp = roundTrip(t, conn, MsgClientList, `{}`)
	if strings.Contains(string(resp.Payload), secret) {
		t.Fatal("CLIENT_LIST must NEVER carry the client secret")
	}
	if strings.Contains(string(resp.Payload), "client_secret") {
		t.Fatal("CLIENT_LIST must not carry a client_secret field at all")
	}
	var list ConfidentialListPayload
	_ = json.Unmarshal(resp.Payload, &list)
	if len(list.Clients) != 1 || list.Clients[0].ID != id || list.Clients[0].ClientID != issued.ClientID || list.Clients[0].Scope != "namespace:foo:rw" {
		t.Fatalf("list: %+v", list.Clients)
	}

	// REVOKE — then LIST is empty.
	resp = roundTrip(t, conn, MsgClientRevoke, fmt.Sprintf(`{"id":%q}`, id))
	if resp.Type != MsgClientRevoke {
		t.Fatalf("revoke type = %s (%s)", resp.Type, resp.Payload)
	}
	resp = roundTrip(t, conn, MsgClientList, `{}`)
	_ = json.Unmarshal(resp.Payload, &list)
	if len(list.Clients) != 0 {
		t.Fatalf("a revoked confidential client must be gone: %+v", list.Clients)
	}

	// Validation: a missing scope and a non-positive validity are rejected (no indefinite).
	if resp = roundTrip(t, conn, MsgClientIssue, `{"scope":"","validity_seconds":3600}`); resp.Type != Error {
		t.Fatalf("an empty scope must error, got %s", resp.Type)
	}
	if resp = roundTrip(t, conn, MsgClientIssue, `{"scope":"namespace:foo:rw","validity_seconds":0}`); resp.Type != Error {
		t.Fatalf("a non-positive validity must error, got %s", resp.Type)
	}
}

// TestWSUI_ConfidentialRevokeCascade: revoking a confidential client cuts the tokens it issued.
func TestWSUI_ConfidentialRevokeCascade(t *testing.T) {
	store := realOAuthStore(t)
	conn := newOAuthManager(t, "", store)

	resp := roundTrip(t, conn, MsgClientIssue, `{"scope":"*","validity_seconds":3600}`)
	var issued ConfidentialIssuePayload
	_ = json.Unmarshal(resp.Payload, &issued)
	// A live token issued to this confidential client.
	series, err := store.NewSeries(issued.ClientID, oauthstore.Principal{Name: "Op"}, "r", "*", time.Now(), time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("NewSeries: %v", err)
	}

	resp = roundTrip(t, conn, MsgClientRevoke, fmt.Sprintf(`{"id":%q}`, issued.ID))
	var rev ConfidentialRevokePayload
	_ = json.Unmarshal(resp.Payload, &rev)
	if rev.RevokedTokens != 1 || rev.Status != "ok" {
		t.Fatalf("revoke cascade: %+v", rev)
	}
	if _, err := store.Lookup(series.AccessToken, time.Now()); err == nil {
		t.Fatal("the confidential client's token must be revoked by the cascade")
	}
}

// TestWSUI_ConfidentialOpsAdminGated: a non-admin scope is refused the CLIENT_* ops by the
// dispatch authz gate (issuance/list/revoke are admin-only).
func TestWSUI_ConfidentialOpsAdminGated(t *testing.T) {
	conn := newOAuthManager(t, "namespace:foo:r", realOAuthStore(t))
	for _, mt := range []MessageType{MsgClientList, MsgClientIssue, MsgClientRevoke} {
		resp := roundTrip(t, conn, mt, `{}`)
		if resp.Type != MsgPermissionDenied {
			t.Fatalf("%s by a non-admin: type = %s, want PERMISSION_DENIED", mt, resp.Type)
		}
	}
}
