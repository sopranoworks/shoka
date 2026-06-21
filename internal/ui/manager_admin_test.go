package ui

import (
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/pkg/oauthstore"
	"github.com/sopranoworks/shoka/pkg/userstore"
)

func testUserStore(t *testing.T) *userstore.Store {
	t.Helper()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	s, err := userstore.Open(filepath.Join(t.TempDir(), "users.db"), key)
	if err != nil {
		t.Fatalf("open userstore: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestWSUI_AdminOps_GatedAndSelfOmitted: ADMIN_LIST_USERS is super-user-only (a
// scoped principal is denied by the stage-2 gate), the list omits the calling admin
// (self), and the self-guard refuses removing one's own account.
func TestWSUI_AdminOps_GatedAndSelfOmitted(t *testing.T) {
	m, _, _ := newSharedCenterManager(t)
	us := testUserStore(t)
	m.SetUserStore(us)
	ph, _ := userstore.HashPassword("pw")
	// withScope injects principal email "u@example.com"; make that the admin (self),
	// plus another user that SHOULD appear.
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "u@example.com", DisplayName: "Me", PasswordHash: ph})
	_ = us.CreateUser(&userstore.UserRecord{Email: "bob@x.com", DisplayName: "Bob", PasswordHash: ph, Scope: "namespace:foo:r"})

	// Non-super-user → ADMIN_LIST_USERS denied by the gate.
	srvScoped := httptest.NewServer(withScope("namespace:foo:r", m))
	defer srvScoped.Close()
	c1 := dialWS(t, srvScoped.URL)
	defer c1.Close()
	sendWS(t, c1, MsgAdminListUsers, struct{}{})
	readUntil(t, c1, MsgPermissionDenied, nil, 2*time.Second)

	// Super-user → list returns users with SELF omitted.
	srv := httptest.NewServer(withScope("*", m))
	defer srv.Close()
	c2 := dialWS(t, srv.URL)
	defer c2.Close()
	sendWS(t, c2, MsgAdminListUsers, struct{}{})
	var users AdminUsersPayload
	readUntil(t, c2, MsgAdminListUsers, &users, 2*time.Second)
	for _, u := range users.Users {
		if u.Email == "u@example.com" {
			t.Fatal("the calling admin (self) must be omitted from the user list")
		}
	}
	found := false
	for _, u := range users.Users {
		if u.Email == "bob@x.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("bob should be in the list: %+v", users.Users)
	}

	// Self-guard: removing one's own account is refused (ERROR), not performed.
	sendWS(t, c2, MsgAdminRemoveUser, adminRemoveUserRequest{Email: "u@example.com"})
	if ft := firstFrameType(t, c2); ft != Error {
		t.Fatalf("self-removal must be refused with ERROR, got %s", ft)
	}
	if _, err := us.GetUser("u@example.com"); err != nil {
		t.Fatal("self account must still exist after refused self-removal")
	}
}

// TestWSUI_AdminSetUserEnabled_SelfGuard: an admin cannot disable their own account
// (the server-side self-guard, defence in depth on top of self being omitted from the list).
func TestWSUI_AdminSetUserEnabled_SelfGuard(t *testing.T) {
	m, _, _ := newSharedCenterManager(t)
	us := testUserStore(t)
	m.SetUserStore(us)
	ph, _ := userstore.HashPassword("pw")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "u@example.com", PasswordHash: ph})

	srv := httptest.NewServer(withScope("*", m))
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()

	sendWS(t, c, MsgAdminSetUserEnabled, adminSetEnabledRequest{Email: "u@example.com", Enabled: false})
	if ft := firstFrameType(t, c); ft != Error {
		t.Fatalf("self-disable must be refused with ERROR, got %s", ft)
	}
	u, err := us.GetUser("u@example.com")
	if err != nil || u.Disabled {
		t.Fatalf("self account must not be disabled after refused self-disable: %+v err=%v", u, err)
	}
}

// TestWSUI_AdminSetUserEnabled_DisableRevokesSessionsAndOAuth: disabling a user flips the
// flag, drops their live UI sessions, and revokes their OAuth/MCP token series — leaving
// an unrelated user's tokens untouched (the immediate-lockout decision, B-28).
func TestWSUI_AdminSetUserEnabled_DisableRevokesSessionsAndOAuth(t *testing.T) {
	m, _, _ := newSharedCenterManager(t)
	us := testUserStore(t)
	m.SetUserStore(us)
	oauth := &fakeOAuthStore{series: []oauthstore.SeriesInfo{
		{SeriesID: "s-bob", Principal: oauthstore.Principal{Email: "bob@x.com"}},
		{SeriesID: "s-carol", Principal: oauthstore.Principal{Email: "carol@x.com"}},
	}}
	m.SetOAuthStore(oauth)
	ph, _ := userstore.HashPassword("pw")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "u@example.com", PasswordHash: ph})
	_ = us.CreateUser(&userstore.UserRecord{Email: "bob@x.com", PasswordHash: ph, Scope: "namespace:foo:rw"})
	sess, err := us.CreateSession("bob@x.com", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(withScope("*", m))
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()

	sendWS(t, c, MsgAdminSetUserEnabled, adminSetEnabledRequest{Email: "bob@x.com", Enabled: false})
	var ack AdminAckPayload
	readUntil(t, c, MsgAdminSetUserEnabled, &ack, 2*time.Second)

	u, err := us.GetUser("bob@x.com")
	if err != nil || !u.Disabled {
		t.Fatalf("bob must be disabled: %+v err=%v", u, err)
	}
	if _, err := us.LookupSession(sess.ID, time.Now()); err == nil {
		t.Fatal("bob's live session must be dropped on disable")
	}
	revoked := oauth.revokedIDs()
	if !slices.Contains(revoked, "s-bob") {
		t.Fatalf("bob's oauth series must be revoked: %v", revoked)
	}
	if slices.Contains(revoked, "s-carol") {
		t.Fatalf("an unrelated user's series must NOT be revoked: %v", revoked)
	}
}

// TestWSUI_AdminRemoveUser_RevokesOAuth: deleting a user (the existing path) now also
// revokes their OAuth/MCP token series — the cross-store gap the investigation found.
func TestWSUI_AdminRemoveUser_RevokesOAuth(t *testing.T) {
	m, _, _ := newSharedCenterManager(t)
	us := testUserStore(t)
	m.SetUserStore(us)
	oauth := &fakeOAuthStore{series: []oauthstore.SeriesInfo{
		{SeriesID: "s-bob", Principal: oauthstore.Principal{Email: "bob@x.com"}},
		{SeriesID: "s-carol", Principal: oauthstore.Principal{Email: "carol@x.com"}},
	}}
	m.SetOAuthStore(oauth)
	ph, _ := userstore.HashPassword("pw")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "u@example.com", PasswordHash: ph})
	_ = us.CreateUser(&userstore.UserRecord{Email: "bob@x.com", PasswordHash: ph, Scope: "namespace:foo:rw"})

	srv := httptest.NewServer(withScope("*", m))
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()

	sendWS(t, c, MsgAdminRemoveUser, adminRemoveUserRequest{Email: "bob@x.com"})
	var ack AdminAckPayload
	readUntil(t, c, MsgAdminRemoveUser, &ack, 2*time.Second)

	if _, err := us.GetUser("bob@x.com"); err == nil {
		t.Fatal("bob must be removed")
	}
	revoked := oauth.revokedIDs()
	if !slices.Contains(revoked, "s-bob") {
		t.Fatalf("bob's oauth series must be revoked on delete: %v", revoked)
	}
	if slices.Contains(revoked, "s-carol") {
		t.Fatalf("unrelated series must NOT be revoked: %v", revoked)
	}
}

// TestWSUI_AdminInviteLifecycle: a super-user creates an invite (code returned) and
// it appears in the list; revoke removes it.
func TestWSUI_AdminInviteLifecycle(t *testing.T) {
	m, _, _ := newSharedCenterManager(t)
	us := testUserStore(t)
	m.SetUserStore(us)
	ph, _ := userstore.HashPassword("pw")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "u@example.com", PasswordHash: ph})

	srv := httptest.NewServer(withScope("*", m))
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()

	sendWS(t, c, MsgAdminCreateInvite, adminCreateInviteRequest{Email: "new@x.com", Scope: "namespace:foo:rw"})
	var created AdminInviteCreatedPayload
	readUntil(t, c, MsgAdminCreateInvite, &created, 2*time.Second)
	if created.Code == "" || created.Email != "new@x.com" || created.CodeHash == "" {
		t.Fatalf("invite create payload = %+v", created)
	}

	sendWS(t, c, MsgAdminListInvites, struct{}{})
	var list AdminInvitesPayload
	readUntil(t, c, MsgAdminListInvites, &list, 2*time.Second)
	if len(list.Invites) != 1 || list.Invites[0].Email != "new@x.com" {
		t.Fatalf("invite list = %+v", list.Invites)
	}

	sendWS(t, c, MsgAdminRevokeInvite, adminRevokeInviteRequest{CodeHash: created.CodeHash})
	var ack AdminAckPayload
	readUntil(t, c, MsgAdminRevokeInvite, &ack, 2*time.Second)
	sendWS(t, c, MsgAdminListInvites, struct{}{})
	var list2 AdminInvitesPayload
	readUntil(t, c, MsgAdminListInvites, &list2, 2*time.Second)
	if len(list2.Invites) != 0 {
		t.Fatalf("invite should be revoked, got %+v", list2.Invites)
	}
}

// TestWSUI_GetProjects_FilteredByScope: GET_PROJECTS is filtered to the principal's
// granted namespaces; a super-user sees all.
func TestWSUI_GetProjects_FilteredByScope(t *testing.T) {
	m, s, _ := newSharedCenterManager(t)
	if err := s.CreateProject("foo", "p1"); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateProject("bar", "p2"); err != nil {
		t.Fatal(err)
	}

	// Scoped to foo: sees only foo.
	srv := httptest.NewServer(withScope("namespace:foo:r", m))
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()
	sendWS(t, c, GetProjects, struct{}{})
	var infos []ProjectInfo
	readUntil(t, c, GetProjects, &infos, 2*time.Second)
	for _, p := range infos {
		if p.Namespace != "foo" {
			t.Fatalf("foo:r principal must see only foo, saw %q", p.Namespace)
		}
	}
	if len(infos) != 1 {
		t.Fatalf("foo:r should see exactly foo/p1, got %+v", infos)
	}

	// Super-user sees both namespaces.
	srv2 := httptest.NewServer(withScope("*", m))
	defer srv2.Close()
	c2 := dialWS(t, srv2.URL)
	defer c2.Close()
	sendWS(t, c2, GetProjects, struct{}{})
	var all []ProjectInfo
	readUntil(t, c2, GetProjects, &all, 2*time.Second)
	if len(all) != 2 {
		t.Fatalf("super-user should see both projects, got %+v", all)
	}
}
