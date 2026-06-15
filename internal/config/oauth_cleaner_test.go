package config

import (
	"testing"
	"time"
)

// The 2026-06-15 authz/lifecycle foundation: the OAuth dead-series cleaner config
// defaults — ON by default (the operator's intent: dead tokens are not worth
// keeping), a 1h tick, and a 24h grace past refresh-expiry.

func TestOAuthCleanerConfig_Defaults(t *testing.T) {
	c := baseValidConfig()
	c.applyDefaults()

	if !c.Storage.OAuthCleaner.IsEnabled() {
		t.Error("oauth cleaner must be ENABLED by default")
	}
	if got := c.Storage.OAuthCleaner.Interval.Std(); got != time.Hour {
		t.Errorf("default interval = %v, want 1h", got)
	}
	if got := c.Storage.OAuthCleaner.Grace.Std(); got != 24*time.Hour {
		t.Errorf("default grace = %v, want 24h", got)
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
	// Interval/grace still get their defaults even when disabled.
	if c.Storage.OAuthCleaner.Interval.Std() != time.Hour {
		t.Error("interval default should still apply when disabled")
	}
}
