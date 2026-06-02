package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad_LostFoundDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalServerStorage))
	require.NoError(t, err)

	// Lost+found worker defaults: enabled, 5m interval (the 2026-06-02 directive §3.7).
	assert.True(t, cfg.Storage.LostFound.IsEnabled())
	assert.Equal(t, 5*time.Minute, cfg.Storage.LostFound.Interval.Std())
}

func TestLoad_LostFoundDisabledOverride(t *testing.T) {
	body := minimalServerStorage + `  lost_found:
    enabled: false
    interval: 10m
`
	cfg, err := Load(writeConfig(t, body))
	require.NoError(t, err)

	// Explicit enabled:false must be honoured; interval override kept.
	assert.False(t, cfg.Storage.LostFound.IsEnabled())
	assert.Equal(t, 10*time.Minute, cfg.Storage.LostFound.Interval.Std())
}
