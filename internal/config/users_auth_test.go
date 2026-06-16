package config

import (
	"testing"
	"time"
)

// B-28 stage 1: the WebUI multi-user login config — allow_first_run_admin defaults
// TRUE (zero-config first-run wizard, usable right away), an explicit false is kept
// (public deployments close the open-registration window), the session TTL defaults
// to 30 days, and passkeys are gated on a non-empty rp_id.

func TestUsersAuthConfig_FirstRunDefaultsTrue(t *testing.T) {
	c := baseValidConfig()
	c.applyDefaults()
	if !c.Server.Auth.Users.FirstRunAdminAllowed() {
		t.Error("allow_first_run_admin must default TRUE (usable right away)")
	}
	if got := c.Server.Auth.Users.SessionTTL.Std(); got != 720*time.Hour {
		t.Errorf("default session_ttl = %v, want 720h", got)
	}
}

func TestUsersAuthConfig_ExplicitDisableKept(t *testing.T) {
	c := baseValidConfig()
	off := false
	c.Server.Auth.Users.AllowFirstRunAdmin = &off
	c.applyDefaults()
	if c.Server.Auth.Users.FirstRunAdminAllowed() {
		t.Error("explicit allow_first_run_admin:false must be kept (closes the window)")
	}
}

func TestWebAuthnConfig_EnabledOnRPID(t *testing.T) {
	var w WebAuthnConfig
	if w.Enabled() {
		t.Error("empty rp_id must disable passkeys (the password/TOTP floor still works)")
	}
	w.RPID = "localhost"
	if !w.Enabled() {
		t.Error("a non-empty rp_id must enable passkeys")
	}
}
