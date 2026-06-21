package ui

import (
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/storage/oauthstore"
	"github.com/sopranoworks/shoka/internal/storage/userstore"
)

// B-28 password recovery case 1: ADMIN_SET_USER_PASSWORD — an admin resets another
// user's password. Admin-gated by the dispatch gate; argon2id re-hash; the target's
// sessions are dropped and their OAuth revoked; a non-admin caller is refused.

// TestWSUI_AdminSetUserPassword_ResetsAndCascades: a super-user resets bob → bob's old
// password no longer verifies and the new one does; bob's live session is dropped and
// his OAuth series revoked, while an unrelated user's series is untouched.
func TestWSUI_AdminSetUserPassword_ResetsAndCascades(t *testing.T) {
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
	oldHash, _ := userstore.HashPassword("boboldpassword")
	_ = us.CreateUser(&userstore.UserRecord{Email: "bob@x.com", PasswordHash: oldHash, Scope: "namespace:foo:rw"})
	sess, err := us.CreateSession("bob@x.com", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(withScope("*", m))
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()

	sendWS(t, c, MsgAdminSetUserPassword, adminSetPasswordRequest{Email: "bob@x.com", Password: "bobnewpassword"})
	var ack AdminAckPayload
	readUntil(t, c, MsgAdminSetUserPassword, &ack, 2*time.Second)
	if ack.Status != "ok" {
		t.Fatalf("expected ok ack, got %+v", ack)
	}

	bob, _ := us.GetUser("bob@x.com")
	if ok, _ := userstore.VerifyPassword("bobnewpassword", bob.PasswordHash); !ok {
		t.Fatal("the new password must verify after the admin reset")
	}
	if ok, _ := userstore.VerifyPassword("boboldpassword", bob.PasswordHash); ok {
		t.Fatal("the old password must NOT verify after the admin reset")
	}
	if _, err := us.LookupSession(sess.ID, time.Now()); err == nil {
		t.Fatal("bob's live session must be dropped on a password reset")
	}
	revoked := oauth.revokedIDs()
	if !slices.Contains(revoked, "s-bob") {
		t.Fatalf("bob's oauth series must be revoked: %v", revoked)
	}
	if slices.Contains(revoked, "s-carol") {
		t.Fatalf("an unrelated user's series must NOT be revoked: %v", revoked)
	}
}

// TestWSUI_AdminSetUserPassword_NonAdminRefused is the admin-gate proof: a non-super-user
// caller is refused by the dispatch gate (PERMISSION_DENIED), never reaching the handler.
// RED proof: lowering MsgAdminSetUserPassword from LevelAdmin in wsLevels lets the
// non-admin through → this fails.
func TestWSUI_AdminSetUserPassword_NonAdminRefused(t *testing.T) {
	m, _, _ := newSharedCenterManager(t)
	us := testUserStore(t)
	m.SetUserStore(us)
	ph, _ := userstore.HashPassword("pw")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "admin@example.com", PasswordHash: ph})
	_ = us.CreateUser(&userstore.UserRecord{Email: "bob@x.com", PasswordHash: ph, Scope: "namespace:foo:r"})

	srv := httptest.NewServer(withScope("namespace:foo:rw", m)) // a write-scoped non-admin
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()

	sendWS(t, c, MsgAdminSetUserPassword, adminSetPasswordRequest{Email: "bob@x.com", Password: "hijackpassword"})
	readUntil(t, c, MsgPermissionDenied, nil, 2*time.Second)
	// bob's password is unchanged (the handler never ran).
	bob, _ := us.GetUser("bob@x.com")
	if ok, _ := userstore.VerifyPassword("hijackpassword", bob.PasswordHash); ok {
		t.Fatal("a non-admin must NOT be able to set another user's password")
	}
}

// TestWSUI_AdminSetUserPassword_PolicyEnforced: a too-short new password is rejected and
// the target's hash is unchanged.
func TestWSUI_AdminSetUserPassword_PolicyEnforced(t *testing.T) {
	m, _, _ := newSharedCenterManager(t)
	us := testUserStore(t)
	m.SetUserStore(us)
	ph, _ := userstore.HashPassword("pw")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "u@example.com", PasswordHash: ph})
	oldHash, _ := userstore.HashPassword("boboldpassword")
	_ = us.CreateUser(&userstore.UserRecord{Email: "bob@x.com", PasswordHash: oldHash, Scope: "namespace:foo:r"})

	srv := httptest.NewServer(withScope("*", m))
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()

	sendWS(t, c, MsgAdminSetUserPassword, adminSetPasswordRequest{Email: "bob@x.com", Password: "short"})
	if ft := firstFrameType(t, c); ft != Error {
		t.Fatalf("a too-short password must be refused with ERROR, got %s", ft)
	}
	bob, _ := us.GetUser("bob@x.com")
	if ok, _ := userstore.VerifyPassword("boboldpassword", bob.PasswordHash); !ok {
		t.Fatal("the target's password must be unchanged after a policy-rejected reset")
	}
}
