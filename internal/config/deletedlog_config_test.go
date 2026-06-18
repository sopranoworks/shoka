package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An existing config WITHOUT a storage.deleted_log block must still load under the
// strict KnownFields(true) decode, taking the defaults (enabled true, repair_depth
// 50, max_entries 1000). There is no interval field.
func TestDeletedLog_DefaultsWhenBlockAbsent(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalServerStorage))
	require.NoError(t, err)
	assert.True(t, cfg.Storage.DeletedLog.IsEnabled())
	assert.Equal(t, 50, cfg.Storage.DeletedLog.EffectiveRepairDepth())
	assert.Equal(t, 1000, cfg.Storage.DeletedLog.EffectiveMaxEntries())
}

// Explicit values are honoured, including enabled:false and an explicit 0 cap.
func TestDeletedLog_ExplicitValues(t *testing.T) {
	body := minimalServerStorage + `  deleted_log:
    enabled: false
    repair_depth: 25
    max_entries: 0
`
	cfg, err := Load(writeConfig(t, body))
	require.NoError(t, err)
	assert.False(t, cfg.Storage.DeletedLog.IsEnabled())
	assert.Equal(t, 25, cfg.Storage.DeletedLog.EffectiveRepairDepth())
	assert.Equal(t, 0, cfg.Storage.DeletedLog.EffectiveMaxEntries()) // explicit 0 honoured (unbounded)
}

// An unknown field under deleted_log fails strict decode (the format guard).
func TestDeletedLog_UnknownFieldRejected(t *testing.T) {
	body := minimalServerStorage + `  deleted_log:
    interval: 5m
`
	_, err := Load(writeConfig(t, body))
	require.Error(t, err) // there is deliberately no interval field
}
