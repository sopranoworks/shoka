package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B-71 Stage 5: the OAuth cleaner has NO grace (removed entirely), and the OAuth token
// TTL defaults are finite & GitHub-informed with a no-indefinite floor.

func TestOAuthCleanerConfig_Defaults(t *testing.T) {
	c := baseValidConfig()
	c.applyDefaults()

	if !c.Storage.OAuthCleaner.IsEnabled() {
		t.Error("oauth cleaner must be ENABLED by default")
	}
	if got := c.Storage.OAuthCleaner.Interval.Std(); got != time.Hour {
		t.Errorf("default interval = %v, want 1h", got)
	}
}

func TestOAuthCleanerConfig_ExplicitDisableKept(t *testing.T) {
	c := baseValidConfig()
	off := false
	c.Storage.OAuthCleaner.Enabled = &off
	c.applyDefaults()
	if c.Storage.OAuthCleaner.IsEnabled() {
		t.Error("explicit enabled:false must be kept")
	}
	// Interval still gets its default even when disabled.
	if c.Storage.OAuthCleaner.Interval.Std() != time.Hour {
		t.Error("interval default should still apply when disabled")
	}
}

// TestOAuthTokenTTLDefaults_Finite: with the TTLs unset, applyDefaults resolves them to
// the finite GitHub-informed defaults (access 1h, refresh 90d, code 1m) — never the old
// un-reviewed 30d, never 0/infinite.
func TestOAuthTokenTTLDefaults_Finite(t *testing.T) {
	c := baseValidConfig()
	c.applyDefaults()
	oc := c.Server.MCP.OAuth
	assert.Equal(t, time.Hour, oc.AccessTokenTTL.Std(), "access TTL default")
	assert.Equal(t, 90*24*time.Hour, oc.RefreshTokenTTL.Std(), "refresh TTL default (replaces un-reviewed 30d)")
	assert.Equal(t, time.Minute, oc.AuthorizationCodeTTL.Std(), "code TTL default")
}

// TestOAuthTokenTTL_NoIndefiniteFloor: a 0 or negative TTL is NOT "forever" — it resolves
// UP to the finite default, so no application path (the AS /token path OR the self-issued
// OAUTH_ISSUE_SELF path, both reading these config values) can mint an unbounded expiry.
// RED proof: remove the `<= 0` floor in applyDefaults and a 0/negative TTL is left as-is
// (assert.Equal to the finite default then fails).
func TestOAuthTokenTTL_NoIndefiniteFloor(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  Duration
	}{
		{"zero", 0},
		{"negative", Duration(-time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := baseValidConfig()
			c.Server.MCP.OAuth.AccessTokenTTL = tc.set
			c.Server.MCP.OAuth.RefreshTokenTTL = tc.set
			c.Server.MCP.OAuth.AuthorizationCodeTTL = tc.set
			c.applyDefaults()
			oc := c.Server.MCP.OAuth
			assert.Equal(t, time.Hour, oc.AccessTokenTTL.Std())
			assert.Equal(t, 90*24*time.Hour, oc.RefreshTokenTTL.Std())
			assert.Equal(t, time.Minute, oc.AuthorizationCodeTTL.Std())
			// Belt and braces: nothing is <= 0 (no unbounded/immediate expiry).
			assert.Greater(t, oc.AccessTokenTTL.Std(), time.Duration(0))
			assert.Greater(t, oc.RefreshTokenTTL.Std(), time.Duration(0))
			assert.Greater(t, oc.AuthorizationCodeTTL.Std(), time.Duration(0))
		})
	}
}

// TestOAuthCleaner_GraceKeyRemoved: the former oauth_cleaner.grace key no longer exists,
// so strict-KnownFields load REJECTS a config still carrying it (the B-71 Stage 5 breaking
// config change). A config without it loads and the cleaner runs grace-free.
func TestOAuthCleaner_GraceKeyRemoved(t *testing.T) {
	withGrace := minimalServerStorage + `  oauth_cleaner:
    grace: 24h
`
	_, err := Load(writeConfig(t, withGrace))
	require.Error(t, err, "a config carrying the removed oauth_cleaner.grace key must fail to load")

	withoutGrace := minimalServerStorage + `  oauth_cleaner:
    interval: 30m
`
	cfg, err := Load(writeConfig(t, withoutGrace))
	require.NoError(t, err)
	assert.True(t, cfg.Storage.OAuthCleaner.IsEnabled())
	assert.Equal(t, 30*time.Minute, cfg.Storage.OAuthCleaner.Interval.Std())
}
