package authapi

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/pkg/userstore"
)

// B-28 stage 3: the unauthenticated invite-redeem HTTP flow. /auth/invite/info shows
// the fixed email/scope; /auth/invite/redeem creates the scoped account, establishes
// the session, and is single-use.
func TestInviteRedeem_Flow(t *testing.T) {
	h, us := newTestHandler(t, true)
	ph, _ := userstore.HashPassword("pw")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "admin@x.com", PasswordHash: ph})
	code, _, err := us.CreateInvite("invitee@x.com", "namespace:foo:rw", "admin@x.com", time.Now(), time.Hour)
	if err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	// info shows the fixed email/scope without consuming.
	rec := postJSON(t, h, "/auth/invite/info", inviteInfoRequest{Code: code})
	if rec.Code != http.StatusOK {
		t.Fatalf("invite/info: %d %s", rec.Code, rec.Body.String())
	}
	var info inviteInfoResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &info)
	if info.Email != "invitee@x.com" || info.Scope != "namespace:foo:rw" {
		t.Fatalf("invite/info = %+v", info)
	}

	// redeem creates the scoped account + session.
	rec = postJSON(t, h, "/auth/invite/redeem", inviteRedeemRequest{Code: code, DisplayName: "Invitee", Password: "inviteepw1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("redeem: %d %s", rec.Code, rec.Body.String())
	}
	if sessionCookie(rec) == nil {
		t.Fatal("redeem must establish a session")
	}
	u, err := us.GetUser("invitee@x.com")
	if err != nil {
		t.Fatalf("redeemed user missing: %v", err)
	}
	if u.Scope != "namespace:foo:rw" {
		t.Fatalf("redeemed scope = %q (must come from the invite)", u.Scope)
	}

	// single-use: a second redeem fails.
	rec = postJSON(t, h, "/auth/invite/redeem", inviteRedeemRequest{Code: code, DisplayName: "Again", Password: "anotherpw1"})
	if rec.Code == http.StatusOK {
		t.Fatal("a used invite must not redeem a second time")
	}
}

func TestInviteRedeem_InvalidCode(t *testing.T) {
	h, _ := newTestHandler(t, true)
	rec := postJSON(t, h, "/auth/invite/redeem", inviteRedeemRequest{Code: "nope", DisplayName: "X", Password: "passw0rd1"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid invite: want 400, got %d", rec.Code)
	}
}
