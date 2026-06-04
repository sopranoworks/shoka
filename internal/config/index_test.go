package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_IndexDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalServerStorage))
	require.NoError(t, err)

	// Index repair worker defaults: enabled, 5m interval (I1, mirroring lost+found).
	assert.True(t, cfg.Storage.Index.IsEnabled())
	assert.Equal(t, 5*time.Minute, cfg.Storage.Index.Interval.Std())
}

func TestLoad_IndexDisabledOverride(t *testing.T) {
	body := minimalServerStorage + `  index:
    enabled: false
    interval: 10m
`
	cfg, err := Load(writeConfig(t, body))
	require.NoError(t, err)

	assert.False(t, cfg.Storage.Index.IsEnabled())
	assert.Equal(t, 10*time.Minute, cfg.Storage.Index.Interval.Std())
}
