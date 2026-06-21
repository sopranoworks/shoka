package ui

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/pkg/userstore"

	"github.com/sopranoworks/shoka/pkg/uiws"
)

// accountTestManager wires a real user store into a fresh manager; tests apply
// withScope("...@example.com") per server to set the session identity.
func accountTestManager(t *testing.T) (*Manager, *userstore.Store) {
	t.Helper()
	m, _, _ := newSharedCenterManager(t)
	us := testUserStore(t)
	m.SetUserStore(us)
	return m, us
}

// TestWSUI_AccountGet_ReturnsSelfNoSecret: ACCOUNT_GET returns the SESSION user's own
// info (name, email, role) — and the wire payload type carries no password hash.
func TestWSUI_AccountGet_ReturnsSelfNoSecret(t *testing.T) {
	m, us := accountTestManager(t)
	ph, _ := userstore.HashPassword("hunter2hunter2")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "u@example.com", DisplayName: "Me", PasswordHash: ph})

	srv := httptest.NewServer(withScope("*", m))
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()

	sendWS(t, c, uiws.MsgAccountGet, struct{}{})
	var info uiws.AccountInfoPayload
	readUntil(t, c, uiws.MsgAccountGet, &info, 2*time.Second)
	if info.Email != "u@example.com" || info.DisplayName != "Me" {
		t.Fatalf("ACCOUNT_GET must return the session user's own info, got %+v", info)
	}
	if !info.IsAdmin {
		t.Fatalf("the first admin's ACCOUNT_GET should report IsAdmin: %+v", info)
	}
}

// TestWSUI_Account_RequiresSession: with no session principal (the no-lockout path)
// there is no account to act on — the account ops return a clear error, not data.
func TestWSUI_Account_RequiresSession(t *testing.T) {
	m, _ := accountTestManager(t)
	srv := httptest.NewServer(m) // no withScope ⇒ no principal
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()

	sendWS(t, c, uiws.MsgAccountGet, struct{}{})
	if ft := firstFrameType(t, c); ft != uiws.Error {
		t.Fatalf("ACCOUNT_GET without a session must error, got %s", ft)
	}
}

// TestWSUI_AccountSetName_Persists: a non-admin user changes their own name and it
// persists (a reload would read the new name).
func TestWSUI_AccountSetName_Persists(t *testing.T) {
	m, us := accountTestManager(t)
	ph, _ := userstore.HashPassword("hunter2hunter2")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "admin@example.com", PasswordHash: ph})
	// u@example.com is a NON-admin, read-only user (the session identity withScope injects).
	_ = us.CreateUser(&userstore.UserRecord{Email: "u@example.com", DisplayName: "Old", PasswordHash: ph, Scope: "namespace:foo:r"})

	srv := httptest.NewServer(withScope("namespace:foo:r", m))
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()

	sendWS(t, c, uiws.MsgAccountSetName, uiws.AccountSetNameRequest{DisplayName: "New Name"})
	var info uiws.AccountInfoPayload
	readUntil(t, c, uiws.MsgAccountSetName, &info, 2*time.Second)
	if info.DisplayName != "New Name" {
		t.Fatalf("response should echo the new name, got %+v", info)
	}
	got, err := us.GetUser("u@example.com")
	if err != nil || got.DisplayName != "New Name" {
		t.Fatalf("name must persist, got %+v err=%v", got, err)
	}
}

// TestWSUI_AccountSetName_EmptyRejected: an empty/whitespace name is rejected.
func TestWSUI_AccountSetName_EmptyRejected(t *testing.T) {
	m, us := accountTestManager(t)
	ph, _ := userstore.HashPassword("hunter2hunter2")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "u@example.com", DisplayName: "Keep", PasswordHash: ph})

	srv := httptest.NewServer(withScope("*", m))
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()

	sendWS(t, c, uiws.MsgAccountSetName, uiws.AccountSetNameRequest{DisplayName: "   "})
	if ft := firstFrameType(t, c); ft != uiws.Error {
		t.Fatalf("empty name must be refused with ERROR, got %s", ft)
	}
	got, _ := us.GetUser("u@example.com")
	if got.DisplayName != "Keep" {
		t.Fatalf("name must be unchanged after a rejected empty set, got %q", got.DisplayName)
	}
}

// TestWSUI_AccountSetName_IgnoresCallerSuppliedTarget is the STRUCTURAL self-access
// proof: even when the caller smuggles a foreign "email" into the payload, the op acts
// on the SESSION identity only — the session user's name changes and the victim's does
// NOT. (The request struct has no email field, so a target id cannot be honoured.)
// RED proof: temporarily making the handler act on a payload "email" makes this fail
// (user A edits user B); the structural impl keeps it green.
func TestWSUI_AccountSetName_IgnoresCallerSuppliedTarget(t *testing.T) {
	m, us := accountTestManager(t)
	ph, _ := userstore.HashPassword("hunter2hunter2")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "u@example.com", DisplayName: "Session", PasswordHash: ph})
	_ = us.CreateUser(&userstore.UserRecord{Email: "victim@example.com", DisplayName: "Victim", PasswordHash: ph, Scope: "namespace:foo:r"})

	srv := httptest.NewServer(withScope("*", m))
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()

	// Smuggle a foreign target email alongside the new name.
	sendWS(t, c, uiws.MsgAccountSetName, map[string]any{
		"display_name": "Pwned",
		"email":        "victim@example.com",
	})
	var info uiws.AccountInfoPayload
	readUntil(t, c, uiws.MsgAccountSetName, &info, 2*time.Second)

	if info.Email != "u@example.com" {
		t.Fatalf("the op must act on the SESSION user, got %s", info.Email)
	}
	victim, _ := us.GetUser("victim@example.com")
	if victim.DisplayName != "Victim" {
		t.Fatalf("self-access violated: another user's name was changed to %q", victim.DisplayName)
	}
	self, _ := us.GetUser("u@example.com")
	if self.DisplayName != "Pwned" {
		t.Fatalf("the session user's own name should have changed, got %q", self.DisplayName)
	}
}

// TestWSUI_AccountSetPassword_RequiresCurrentAndRehashes: a wrong current password is
// rejected (hash untouched); a correct current password + valid new password re-hashes
// argon2id so the NEW password verifies and the OLD no longer does.
func TestWSUI_AccountSetPassword_RequiresCurrentAndRehashes(t *testing.T) {
	m, us := accountTestManager(t)
	oldHash, _ := userstore.HashPassword("oldpassword1")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "u@example.com", DisplayName: "Me", PasswordHash: oldHash})

	srv := httptest.NewServer(withScope("*", m))
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()

	// Wrong current password → rejected, hash unchanged.
	sendWS(t, c, uiws.MsgAccountSetPassword, uiws.AccountSetPasswordRequest{CurrentPassword: "wrongpassword", NewPassword: "newpassword1"})
	if ft := firstFrameType(t, c); ft != uiws.Error {
		t.Fatalf("wrong current password must be refused with ERROR, got %s", ft)
	}
	after, _ := us.GetUser("u@example.com")
	if after.PasswordHash != oldHash {
		t.Fatal("hash must be unchanged after a rejected reset")
	}

	// Correct current password → re-hash; new verifies, old does not.
	sendWS(t, c, uiws.MsgAccountSetPassword, uiws.AccountSetPasswordRequest{CurrentPassword: "oldpassword1", NewPassword: "newpassword1"})
	var ack uiws.AdminAckPayload
	readUntil(t, c, uiws.MsgAccountSetPassword, &ack, 2*time.Second)
	if ack.Status != "ok" {
		t.Fatalf("expected ok ack, got %+v", ack)
	}
	got, _ := us.GetUser("u@example.com")
	if okNew, _ := userstore.VerifyPassword("newpassword1", got.PasswordHash); !okNew {
		t.Fatal("the new password must verify after the reset")
	}
	if okOld, _ := userstore.VerifyPassword("oldpassword1", got.PasswordHash); okOld {
		t.Fatal("the old password must NOT verify after the reset")
	}
}

// TestWSUI_AccountSetPassword_PolicyEnforced: a new password shorter than the policy
// floor is rejected even with the correct current password.
func TestWSUI_AccountSetPassword_PolicyEnforced(t *testing.T) {
	m, us := accountTestManager(t)
	oldHash, _ := userstore.HashPassword("oldpassword1")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "u@example.com", PasswordHash: oldHash})

	srv := httptest.NewServer(withScope("*", m))
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()

	sendWS(t, c, uiws.MsgAccountSetPassword, uiws.AccountSetPasswordRequest{CurrentPassword: "oldpassword1", NewPassword: "short"})
	if ft := firstFrameType(t, c); ft != uiws.Error {
		t.Fatalf("a too-short new password must be refused with ERROR, got %s", ft)
	}
	got, _ := us.GetUser("u@example.com")
	if got.PasswordHash != oldHash {
		t.Fatal("hash must be unchanged after a policy-rejected reset")
	}
}
