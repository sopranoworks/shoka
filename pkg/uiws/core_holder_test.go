package uiws

// Holder-without-StorageService proof (the 2026-06-21 core-handler extraction, step b;
// moved to pkg/uiws by the ui-split step). It now proves the capability from pkg/.
//
// This file deliberately imports NO internal/storage (the document store): it proves the
// auth/user/OAuth slice — CoreHandlers — is constructible and operable with ONLY a user
// store + an OAuth store, never a document storage.StorageService. A second program
// (GitYard, a feature-reduced Shoka with no document store) can therefore mount these
// handlers on its OWN ws manager. serveCoreOnly below IS that second ws manager, in
// miniature: it upgrades the socket, attaches the session principal, runs the SAME
// shared gate (Client.Gate over uiws.CoreLevels), and dispatches a few core ops to the
// holder — with no Manager and no StorageService anywhere in scope.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sopranoworks/shoka/pkg/userstore"
)

// serveCoreOnly is a minimal /ws/ui manager built on *CoreHandlers ALONE — the proof
// that the core slice needs no document store. It mirrors Manager.ServeHTTP's gate +
// dispatch for the core ops only; there is no storage.StorageService, no drafts, no
// notify in this closure.
func serveCoreOnly(core *CoreHandlers) http.Handler {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		client := NewClient(conn, "ws-core-test", r)
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg WSMessage
			if err := json.Unmarshal(message, &msg); err != nil {
				client.SendError("Invalid message format")
				continue
			}
			// The SAME shared gate Shoka uses, reached through the holder (not a Manager):
			// Client.Gate over the core level table, with no super-user ops (the core
			// contributes none).
			if !client.Gate(msg.Type, msg.Payload, CoreLevels, nil) {
				continue
			}
			switch msg.Type {
			case MsgAccountGet:
				core.handleAccountGet(client)
			case MsgAdminListUsers:
				core.handleAdminListUsers(client)
			case MsgDomainList:
				core.handleDomainList(client)
			default:
				client.SendError("Unknown message type")
			}
		}
	})
}

// dialCore connects a ws client to a serveCoreOnly handler carrying the given session
// scope (via the shared withScope middleware). An empty scope ⇒ no principal.
func dialCore(t *testing.T, core *CoreHandlers, scope string) *websocket.Conn {
	t.Helper()
	var h http.Handler = serveCoreOnly(core)
	if scope != "" {
		h = withScope(scope, h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return dialWS(t, srv.URL)
}

// TestCoreHandlers_OperateWithoutStorageService is the directive's binding proof: a
// CoreHandlers built with ONLY a user store + an OAuth store (NO StorageService) serves
// an ACCOUNT op (session identity), an ADMIN op (admin-gated), and an OAUTH/DOMAIN op
// (admin-gated), with the same gating as on the full Manager.
func TestCoreHandlers_OperateWithoutStorageService(t *testing.T) {
	us := testUserStore(t)
	ph, _ := userstore.HashPassword("hunter2hunter2")
	// withScope injects principal email "u@example.com"; make that user the first admin.
	if err := us.CreateFirstAdmin(&userstore.UserRecord{Email: "u@example.com", DisplayName: "Me", PasswordHash: ph}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if err := us.CreateUser(&userstore.UserRecord{Email: "bob@x.com", DisplayName: "Bob", PasswordHash: ph, Scope: "namespace:foo:r"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	// The holder: user + OAuth stores ONLY. No StorageService is constructed anywhere in
	// this test — if the core slice still required one, this would not compile/run.
	core := &CoreHandlers{users: us, oauth: &fakeOAuthStore{}}

	// ACCOUNT_GET — any authenticated user, acting on the session identity.
	t.Run("account op (session identity)", func(t *testing.T) {
		c := dialCore(t, core, "*")
		defer c.Close()
		sendWS(t, c, MsgAccountGet, struct{}{})
		var info AccountInfoPayload
		readUntil(t, c, MsgAccountGet, &info, 2*time.Second)
		if info.Email != "u@example.com" || info.DisplayName != "Me" {
			t.Fatalf("ACCOUNT_GET through the holder must return the session user's own info, got %+v", info)
		}
	})

	// ADMIN_LIST_USERS — admin-gated. Admin scope succeeds (self omitted).
	t.Run("admin op allowed for super-user", func(t *testing.T) {
		c := dialCore(t, core, "*")
		defer c.Close()
		sendWS(t, c, MsgAdminListUsers, struct{}{})
		var users AdminUsersPayload
		readUntil(t, c, MsgAdminListUsers, &users, 2*time.Second)
		for _, u := range users.Users {
			if u.Email == "u@example.com" {
				t.Fatal("self must be omitted from the admin user list")
			}
		}
	})

	// RED-prove the gate still bites through the holder: a non-super-user is REFUSED the
	// SAME admin op. If the gate were dropped, this op would proceed (returning
	// ADMIN_LIST_USERS data) instead of PERMISSION_DENIED — the assertion below would then
	// fail, so the gate is what makes this pass.
	t.Run("admin op denied for non-admin (gate bites)", func(t *testing.T) {
		c := dialCore(t, core, "namespace:foo:r")
		defer c.Close()
		sendWS(t, c, MsgAdminListUsers, struct{}{})
		if ft := firstFrameType(t, c); ft != MsgPermissionDenied {
			t.Fatalf("non-admin ADMIN_LIST_USERS through the holder must be PERMISSION_DENIED, got %s", ft)
		}
	})

	// DOMAIN_LIST — an OAUTH/DOMAIN op, admin-gated, served through the holder's OAuth
	// store (no document store involved).
	t.Run("oauth/domain op allowed for super-user", func(t *testing.T) {
		c := dialCore(t, core, "*")
		defer c.Close()
		sendWS(t, c, MsgDomainList, struct{}{})
		var got DomainListPayload
		readUntil(t, c, MsgDomainList, &got, 2*time.Second)
	})
}
