package ui

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/sopranoworks/shoka/pkg/userstore"

	"github.com/sopranoworks/shoka/pkg/uiws"
)

func TestWSUI_AccountTOTPEnroll_ReturnsSecret(t *testing.T) {
	m, us := accountTestManager(t)
	m.SetRPName("Test RP")
	ph, _ := userstore.HashPassword("hunter2hunter2")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "u@example.com", DisplayName: "Me", PasswordHash: ph})

	srv := httptest.NewServer(withScope("*", m))
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()

	sendWS(t, c, uiws.MsgAccountTOTPEnroll, struct{}{})
	var resp uiws.AccountTOTPEnrollResponse
	readUntil(t, c, uiws.MsgAccountTOTPEnroll, &resp, 2*time.Second)
	if resp.Secret == "" {
		t.Fatal("ACCOUNT_TOTP_ENROLL must return a non-empty secret")
	}
	if resp.OtpauthURL == "" {
		t.Fatal("ACCOUNT_TOTP_ENROLL must return a non-empty otpauth URL")
	}

	rec, _ := us.GetUser("u@example.com")
	if rec.HasTOTP() {
		t.Fatal("ACCOUNT_TOTP_ENROLL must not persist the secret (side-effect-free)")
	}
}

func TestWSUI_AccountTOTPVerify_EnrollsAndPersists(t *testing.T) {
	m, us := accountTestManager(t)
	m.SetRPName("Test RP")
	ph, _ := userstore.HashPassword("hunter2hunter2")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "u@example.com", DisplayName: "Me", PasswordHash: ph})

	srv := httptest.NewServer(withScope("*", m))
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()

	sendWS(t, c, uiws.MsgAccountTOTPEnroll, struct{}{})
	var enrollResp uiws.AccountTOTPEnrollResponse
	readUntil(t, c, uiws.MsgAccountTOTPEnroll, &enrollResp, 2*time.Second)

	code := testTOTPCode(t, enrollResp.Secret)

	sendWS(t, c, uiws.MsgAccountTOTPVerify, uiws.AccountTOTPVerifyRequest{
		TOTPSecret: enrollResp.Secret,
		TOTPCode:   code,
	})
	var info uiws.AccountInfoPayload
	readUntil(t, c, uiws.MsgAccountTOTPVerify, &info, 2*time.Second)
	if !info.HasTOTP {
		t.Fatal("after verify, has_totp must be true")
	}

	rec, _ := us.GetUser("u@example.com")
	if !rec.HasTOTP() {
		t.Fatal("TOTP must be persisted on the user record")
	}
}

func TestWSUI_AccountTOTPVerify_WrongCodeRejected(t *testing.T) {
	m, us := accountTestManager(t)
	m.SetRPName("Test RP")
	ph, _ := userstore.HashPassword("hunter2hunter2")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "u@example.com", DisplayName: "Me", PasswordHash: ph})

	srv := httptest.NewServer(withScope("*", m))
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()

	sendWS(t, c, uiws.MsgAccountTOTPEnroll, struct{}{})
	var enrollResp uiws.AccountTOTPEnrollResponse
	readUntil(t, c, uiws.MsgAccountTOTPEnroll, &enrollResp, 2*time.Second)

	sendWS(t, c, uiws.MsgAccountTOTPVerify, uiws.AccountTOTPVerifyRequest{
		TOTPSecret: enrollResp.Secret,
		TOTPCode:   "000000",
	})
	if ft := firstFrameType(t, c); ft != uiws.Error {
		t.Fatalf("wrong TOTP code must be refused with ERROR, got %s", ft)
	}

	rec, _ := us.GetUser("u@example.com")
	if rec.HasTOTP() {
		t.Fatal("TOTP must not be enrolled after a failed verify")
	}
}

func TestWSUI_AccountTOTPDisable_ClearsSecret(t *testing.T) {
	m, us := accountTestManager(t)
	m.SetRPName("Test RP")
	ph, _ := userstore.HashPassword("hunter2hunter2")
	_ = us.CreateFirstAdmin(&userstore.UserRecord{Email: "u@example.com", DisplayName: "Me", PasswordHash: ph})

	srv := httptest.NewServer(withScope("*", m))
	defer srv.Close()
	c := dialWS(t, srv.URL)
	defer c.Close()

	sendWS(t, c, uiws.MsgAccountTOTPEnroll, struct{}{})
	var enrollResp uiws.AccountTOTPEnrollResponse
	readUntil(t, c, uiws.MsgAccountTOTPEnroll, &enrollResp, 2*time.Second)

	code := testTOTPCode(t, enrollResp.Secret)
	sendWS(t, c, uiws.MsgAccountTOTPVerify, uiws.AccountTOTPVerifyRequest{
		TOTPSecret: enrollResp.Secret,
		TOTPCode:   code,
	})
	var verifyInfo uiws.AccountInfoPayload
	readUntil(t, c, uiws.MsgAccountTOTPVerify, &verifyInfo, 2*time.Second)
	if !verifyInfo.HasTOTP {
		t.Fatal("setup: TOTP must be enrolled before testing disable")
	}

	sendWS(t, c, uiws.MsgAccountTOTPDisable, struct{}{})
	var disableInfo uiws.AccountInfoPayload
	readUntil(t, c, uiws.MsgAccountTOTPDisable, &disableInfo, 2*time.Second)
	if disableInfo.HasTOTP {
		t.Fatal("after disable, has_totp must be false")
	}

	rec, _ := us.GetUser("u@example.com")
	if rec.HasTOTP() {
		t.Fatal("TOTP must be removed from the user record after disable")
	}
}

func testTOTPCode(t *testing.T, secret string) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, time.Now(), totp.ValidateOpts{
		Period:    30,
		Skew:     1,
		Digits:   otp.DigitsSix,
		Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("generate TOTP code: %v", err)
	}
	return code
}
