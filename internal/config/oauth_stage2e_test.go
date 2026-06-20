package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B-71 Stage 2e retires the static OAuth keys trusted_client_metadata_domains and
// consent_credential as the place-of-configuration (the dynamic "domain" store, managed via the
// web UI, is the sole runtime source). The keys are NOT removed from the struct: they stay
// PARSEABLE so a not-yet-seeded deployment can still be migrated by the one-time, marker-guarded
// startup seed (Option A — cannot strand an upgrading operator). The strict-KnownFields decision
// is therefore option (i): a config still carrying the keys LOADS (not a hard error), with a
// startup deprecation warning (emitted in cmd/shoka, not here).
//
// This is the DELIBERATE OPPOSITE of the Stage 5 oauth_cleaner.grace removal
// (TestOAuthCleaner_GraceKeyRemoved), which hard-fails strict-KnownFields — justified there
// because grace had no migration dependency, whereas these keys ARE the migration source.
//
// RED proof: remove the struct fields (Option B) and this load fails with an unknown-field error
// — which is exactly the stranding hazard Option A avoids, so the test encodes the choice.
func TestOAuth_DeprecatedMigrationKeysStillParse(t *testing.T) {
	withKeys := `server:
  http:
    listen: ":8080"
  mcp:
    oauth:
      listen: ":8082"
      consent_credential: "migrate-me"
      trusted_client_metadata_domains:
        - "connector.example"
storage:
  base_dir: "/tmp/shoka"
`
	cfg, err := Load(writeConfig(t, withKeys))
	require.NoError(t, err, "a config still carrying the deprecated migration keys must LOAD (Stage 2e option i: parseable-but-ignored)")

	// The values remain readable so the one-time migration seed can consume them.
	assert.Equal(t, "migrate-me", cfg.Server.MCP.OAuth.ConsentCredential)
	assert.Equal(t, []string{"connector.example"}, cfg.Server.MCP.OAuth.TrustedClientMetadataDomains)

	// A config WITHOUT the keys is the post-migration norm and also loads cleanly.
	withoutKeys := `server:
  http:
    listen: ":8080"
  mcp:
    oauth:
      listen: ":8082"
storage:
  base_dir: "/tmp/shoka"
`
	cfg2, err := Load(writeConfig(t, withoutKeys))
	require.NoError(t, err)
	assert.Empty(t, cfg2.Server.MCP.OAuth.ConsentCredential)
	assert.Empty(t, cfg2.Server.MCP.OAuth.TrustedClientMetadataDomains)
}
