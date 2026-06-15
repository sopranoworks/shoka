package config

import (
	"strings"
	"testing"
	"time"
)

// baseValidConfig returns a Config that passes Validate, so a test can toggle a
// single backup field and assert its effect in isolation.
func baseValidConfig() *Config {
	c := &Config{}
	c.Storage.BaseDir = "/data"
	c.Server.HTTP.Listen = ":8080"
	c.Server.MCP.Plain.Listen = ":9090"
	return c
}

func TestBackupConfig_Defaults(t *testing.T) {
	c := baseValidConfig()
	c.applyDefaults()

	if got := c.Storage.Backup.Interval.Std(); got != 24*time.Hour {
		t.Errorf("default interval = %v, want 24h", got)
	}
	if c.Storage.Backup.Scope != "all" {
		t.Errorf("default scope = %q, want all", c.Storage.Backup.Scope)
	}
	if got := c.Storage.Backup.EffectiveRetentionCount(); got != 7 {
		t.Errorf("default retention_count = %d, want 7", got)
	}
	// Off by default.
	if c.Storage.Backup.IsEnabled() {
		t.Error("backup must be disabled by default")
	}
}

func TestBackupConfig_ExplicitZeroRetentionStays(t *testing.T) {
	c := baseValidConfig()
	zero := 0
	c.Storage.Backup.RetentionCount = &zero
	c.applyDefaults()
	if got := c.Storage.Backup.EffectiveRetentionCount(); got != 0 {
		t.Errorf("explicit retention_count 0 must stay 0 (count pruning off), got %d", got)
	}
}

func TestBackupConfig_Validate_OutputDirRequiredWhenEnabled(t *testing.T) {
	c := baseValidConfig()
	on := true
	c.Storage.Backup.Enabled = &on
	c.Storage.Backup.Scope = "all"
	c.applyDefaults()
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "output_dir") {
		t.Fatalf("enabled backup with no output_dir must fail on output_dir, got %v", err)
	}

	c.Storage.Backup.OutputDir = "/backups"
	if err := c.Validate(); err != nil {
		t.Fatalf("enabled backup with output_dir should validate, got %v", err)
	}
}

func TestBackupConfig_Validate_ScopeSyntax(t *testing.T) {
	for _, ok := range []string{"", "all", "namespace:shoka", "project:shoka/maintenance"} {
		c := baseValidConfig()
		c.Storage.Backup.Scope = ok
		if err := c.Validate(); err != nil {
			t.Errorf("scope %q should be valid, got %v", ok, err)
		}
	}
	for _, bad := range []string{"namespace:", "project:shoka", "project:/x", "nonsense"} {
		c := baseValidConfig()
		c.Storage.Backup.Scope = bad
		if err := c.Validate(); err == nil {
			t.Errorf("scope %q should be rejected", bad)
		}
	}
}
