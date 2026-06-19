package userstore

import (
	"testing"
	"time"
)

func seedAdmin(t *testing.T, s *Store) {
	t.Helper()
	ph, _ := HashPassword("pw")
	if err := s.CreateFirstAdmin(&UserRecord{Email: "admin@x.com", DisplayName: "Admin", PasswordHash: ph}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
}

func TestListUsers_NoSecretsSorted(t *testing.T) {
	s := openTestStore(t)
	seedAdmin(t, s)
	ph, _ := HashPassword("pw")
	_ = s.CreateUser(&UserRecord{Email: "zed@x.com", PasswordHash: ph, Scope: "namespace:foo:r"})
	_ = s.CreateUser(&UserRecord{Email: "bob@x.com", PasswordHash: ph, Scope: "namespace:bar:rw"})

	users, err := s.ListUsers()
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 3 {
		t.Fatalf("want 3 users, got %d", len(users))
	}
	if users[0].Email != "admin@x.com" || users[1].Email != "bob@x.com" || users[2].Email != "zed@x.com" {
		t.Fatalf("users not sorted by email: %+v", users)
	}
	// UserInfo carries no secret fields (struct has only email/display/scope).
	if users[2].Scope != "namespace:foo:r" {
		t.Fatalf("scope not surfaced: %+v", users[2])
	}
}

func TestUpdateUserScope(t *testing.T) {
	s := openTestStore(t)
	seedAdmin(t, s)
	ph, _ := HashPassword("pw")
	_ = s.CreateUser(&UserRecord{Email: "u@x.com", PasswordHash: ph, Scope: "namespace:foo:r"})

	if err := s.UpdateUserScope("u@x.com", "namespace:foo:rw,namespace:bar:r"); err != nil {
		t.Fatalf("UpdateUserScope: %v", err)
	}
	u, _ := s.GetUser("u@x.com")
	if u.Scope != "namespace:foo:rw,namespace:bar:r" {
		t.Fatalf("scope not updated: %q", u.Scope)
	}
	if err := s.UpdateUserScope("missing@x.com", "x"); err != ErrNotFound {
		t.Fatalf("update missing: want ErrNotFound, got %v", err)
	}
}

// TestSetUserDisabled: disabling sets the flag AND drops the user's live sessions
// (immediate lockout); re-enabling clears the flag; an unknown user is ErrNotFound.
func TestSetUserDisabled(t *testing.T) {
	s := openTestStore(t)
	seedAdmin(t, s)
	ph, _ := HashPassword("pw")
	_ = s.CreateUser(&UserRecord{Email: "u@x.com", PasswordHash: ph})
	sess, _ := s.CreateSession("u@x.com", time.Now(), time.Hour)

	if err := s.SetUserDisabled("u@x.com", true); err != nil {
		t.Fatalf("SetUserDisabled(true): %v", err)
	}
	u, _ := s.GetUser("u@x.com")
	if !u.Disabled {
		t.Fatal("user should be disabled")
	}
	if _, err := s.LookupSession(sess.ID, time.Now()); err != ErrNotFound {
		t.Fatalf("disabled user's session must be dropped, got %v", err)
	}
	// Re-enable clears the flag.
	if err := s.SetUserDisabled("u@x.com", false); err != nil {
		t.Fatalf("SetUserDisabled(false): %v", err)
	}
	u, _ = s.GetUser("u@x.com")
	if u.Disabled {
		t.Fatal("user should be re-enabled")
	}
	if err := s.SetUserDisabled("missing@x.com", true); err != ErrNotFound {
		t.Fatalf("disable missing: want ErrNotFound, got %v", err)
	}
}

func TestRemoveUser_DropsSessions(t *testing.T) {
	s := openTestStore(t)
	seedAdmin(t, s)
	ph, _ := HashPassword("pw")
	_ = s.CreateUser(&UserRecord{Email: "u@x.com", PasswordHash: ph})
	sess, _ := s.CreateSession("u@x.com", time.Now(), time.Hour)

	if err := s.RemoveUser("u@x.com"); err != nil {
		t.Fatalf("RemoveUser: %v", err)
	}
	if _, err := s.GetUser("u@x.com"); err != ErrNotFound {
		t.Fatalf("user still present: %v", err)
	}
	if _, err := s.LookupSession(sess.ID, time.Now()); err != ErrNotFound {
		t.Fatalf("removed user's session must be gone, got %v", err)
	}
	if err := s.RemoveUser("u@x.com"); err != ErrNotFound {
		t.Fatalf("remove missing: want ErrNotFound, got %v", err)
	}
}

func TestInvite_Lifecycle(t *testing.T) {
	s := openTestStore(t)
	seedAdmin(t, s)
	now := time.Now()

	code, rec, err := s.CreateInvite("invitee@x.com", "namespace:foo:rw", "admin@x.com", now, time.Hour)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
	if code == "" || rec.CodeHash == "" {
		t.Fatal("invite must yield a code and a stored hash")
	}
	// The plaintext code is NOT the stored key.
	if rec.CodeHash == code {
		t.Fatal("the code must be hashed at rest, not stored verbatim")
	}
	invs, _ := s.ListInvites()
	if len(invs) != 1 || invs[0].Email != "invitee@x.com" || invs[0].Used {
		t.Fatalf("ListInvites: %+v", invs)
	}

	// Redeem with the credentials the invitee supplies; email + scope come from the invite.
	ph, _ := HashPassword("inviteepw")
	u := &UserRecord{PasswordHash: ph, DisplayName: "Invitee", Email: "ignored@x.com", Scope: "ignored"}
	if err := s.RedeemInvite(code, now, u); err != nil {
		t.Fatalf("RedeemInvite: %v", err)
	}
	got, err := s.GetUser("invitee@x.com")
	if err != nil {
		t.Fatalf("redeemed user missing: %v", err)
	}
	if got.Scope != "namespace:foo:rw" {
		t.Fatalf("redeemed scope = %q, want from invite", got.Scope)
	}
	if got.Email != "invitee@x.com" {
		t.Fatalf("redeemed email must come from invite, got %q", got.Email)
	}

	// Single-use: a second redeem of the same code fails.
	if err := s.RedeemInvite(code, now, &UserRecord{PasswordHash: ph}); err != ErrInvalidInvite {
		t.Fatalf("second redeem must fail single-use, got %v", err)
	}
}

func TestInvite_ExpiredAndRevoked(t *testing.T) {
	s := openTestStore(t)
	seedAdmin(t, s)
	now := time.Now()
	ph, _ := HashPassword("pw")

	// Expired.
	code, _, _ := s.CreateInvite("a@x.com", "namespace:foo:rw", "admin@x.com", now, time.Minute)
	if err := s.RedeemInvite(code, now.Add(2*time.Minute), &UserRecord{PasswordHash: ph}); err != ErrInvalidInvite {
		t.Fatalf("expired redeem: want ErrInvalidInvite, got %v", err)
	}

	// Revoked.
	code2, rec2, _ := s.CreateInvite("b@x.com", "namespace:foo:rw", "admin@x.com", now, time.Hour)
	if err := s.RevokeInvite(rec2.CodeHash); err != nil {
		t.Fatalf("RevokeInvite: %v", err)
	}
	if err := s.RedeemInvite(code2, now, &UserRecord{PasswordHash: ph}); err != ErrInvalidInvite {
		t.Fatalf("revoked redeem: want ErrInvalidInvite, got %v", err)
	}
}

func TestInvite_DuplicateEmailRefused(t *testing.T) {
	s := openTestStore(t)
	seedAdmin(t, s)
	now := time.Now()
	ph, _ := HashPassword("pw")
	// Invite for an email that already exists → redeem must refuse.
	code, _, _ := s.CreateInvite("admin@x.com", "namespace:foo:rw", "admin@x.com", now, time.Hour)
	if err := s.RedeemInvite(code, now, &UserRecord{PasswordHash: ph}); err != ErrExists {
		t.Fatalf("redeem for existing email: want ErrExists, got %v", err)
	}
}
