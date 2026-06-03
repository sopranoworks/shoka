package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoad(t *testing.T) {
	yamlContent := `
server:
  http:
    listen: ":8080"
    external_url: "http://localhost:8080"
    tls:
      enabled: true
      cert_file: "cert.pem"
      key_file: "key.pem"
  mcp:
    listen: ":8081"
    external_url: "http://localhost:8081"
    tls:
      enabled: false
storage:
  base_dir: "/tmp/shoka"
services:
  google_cloud:
    project_id: "my-project"
`
	tmpFile, err := os.CreateTemp("", "config*.yaml")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())

	_, err = tmpFile.WriteString(yamlContent)
	require.NoError(t, err)
	err = tmpFile.Close()
	require.NoError(t, err)

	cfg, err := Load(tmpFile.Name())
	require.NoError(t, err)

	assert.Equal(t, ":8080", cfg.Server.HTTP.Listen)
	assert.Equal(t, "http://localhost:8080", cfg.Server.HTTP.ExternalURL)
	assert.True(t, cfg.Server.HTTP.TLS.Enabled)
	assert.Equal(t, "cert.pem", cfg.Server.HTTP.TLS.CertFile)
	assert.Equal(t, "key.pem", cfg.Server.HTTP.TLS.KeyFile)

	assert.Equal(t, ":8081", cfg.Server.MCP.Listen)
	assert.False(t, cfg.Server.MCP.TLS.Enabled)

	assert.Equal(t, "/tmp/shoka", cfg.Storage.BaseDir)
	assert.Equal(t, "my-project", cfg.Services.GoogleCloud.ProjectID)
}

func TestLoad_Auth(t *testing.T) {
	yamlContent := `
server:
  http:
    listen: ":8080"
  mcp:
    listen: ":8081"
  auth:
    enabled: true
    tokens:
      - "tok-a"
      - "tok-b"
    allowed_origins:
      - "https://app.example.com"
storage:
  base_dir: "/tmp/shoka"
`
	tmpFile, err := os.CreateTemp("", "config-auth*.yaml")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())
	_, err = tmpFile.WriteString(yamlContent)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	cfg, err := Load(tmpFile.Name())
	require.NoError(t, err)

	assert.True(t, cfg.Server.Auth.Enabled)
	assert.Equal(t, []string{"tok-a", "tok-b"}, cfg.Server.Auth.Tokens)
	assert.Equal(t, []string{"https://app.example.com"}, cfg.Server.Auth.AllowedOrigins)
}

func TestLoad_OAuthDiscovery(t *testing.T) {
	yamlContent := `
server:
  http:
    listen: ":8080"
  mcp:
    listen: ":8081"
    external_url: "https://public.example"
  auth:
    oauth:
      enabled: true
storage:
  base_dir: "/tmp/shoka"
`
	tmpFile, err := os.CreateTemp("", "config-oauth*.yaml")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())
	_, err = tmpFile.WriteString(yamlContent)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	cfg, err := Load(tmpFile.Name())
	require.NoError(t, err)

	// OAuth discovery is a distinct switch from the static-bearer auth.enabled.
	assert.True(t, cfg.Server.Auth.OAuth.Enabled)
	assert.False(t, cfg.Server.Auth.Enabled)
	assert.Equal(t, "https://public.example", cfg.Server.MCP.ExternalURL)
}

func TestLoad_OAuthDiscoveryDefaultsDisabled(t *testing.T) {
	yamlContent := `
server:
  http:
    listen: ":8080"
  mcp:
    listen: ":8081"
storage:
  base_dir: "/tmp/shoka"
`
	f, err := os.CreateTemp(t.TempDir(), "config-nooauth*.yaml")
	require.NoError(t, err)
	_, err = f.WriteString(yamlContent)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	cfg, err := Load(f.Name())
	require.NoError(t, err)
	assert.False(t, cfg.Server.Auth.OAuth.Enabled)
}

func TestLoad_AuthDefaultsDisabled(t *testing.T) {
	yamlContent := `
server:
  http:
    listen: ":8080"
  mcp:
    listen: ":8081"
storage:
  base_dir: "/tmp/shoka"
`
	tmpFile, err := os.CreateTemp("", "config-noauth*.yaml")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name())
	_, err = tmpFile.WriteString(yamlContent)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())

	cfg, err := Load(tmpFile.Name())
	require.NoError(t, err)
	assert.False(t, cfg.Server.Auth.Enabled)
}

func TestLoad_LogConfig(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		f, err := os.CreateTemp(t.TempDir(), "cfg-*.yaml")
		require.NoError(t, err)
		_, err = f.WriteString(body)
		require.NoError(t, err)
		require.NoError(t, f.Close())
		return f.Name()
	}
	const base = `
storage:
  base_dir: /tmp/shoka
server:
  http:
    listen: ":8080"
  mcp:
    listen: ":8081"
`
	t.Run("absent log block defaults to empty (info/text applied later)", func(t *testing.T) {
		cfg, err := Load(write(t, base))
		require.NoError(t, err)
		assert.Equal(t, "", cfg.Server.Log.Level)
		assert.Equal(t, "", cfg.Server.Log.Format)
	})
	t.Run("explicit log block parses", func(t *testing.T) {
		cfg, err := Load(write(t, base+"  log:\n    level: debug\n    format: json\n"))
		require.NoError(t, err)
		assert.Equal(t, "debug", cfg.Server.Log.Level)
		assert.Equal(t, "json", cfg.Server.Log.Format)
	})
	t.Run("invalid level rejected", func(t *testing.T) {
		_, err := Load(write(t, base+"  log:\n    level: verbose\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "log.level")
	})
	t.Run("invalid format rejected", func(t *testing.T) {
		_, err := Load(write(t, base+"  log:\n    format: xml\n"))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "log.format")
	})
}

func TestLoad_Errors(t *testing.T) {
	t.Run("missing file", func(t *testing.T) {
		_, err := Load("non-existent-file.yaml")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to read config file")
	})

	t.Run("invalid yaml", func(t *testing.T) {
		tmpFile, err := os.CreateTemp("", "invalid*.yaml")
		require.NoError(t, err)
		defer os.Remove(tmpFile.Name())

		_, err = tmpFile.WriteString("invalid: yaml: :")
		require.NoError(t, err)
		tmpFile.Close()

		_, err = Load(tmpFile.Name())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to parse config YAML")
	})

	t.Run("validation failure - missing storage.base_dir", func(t *testing.T) {
		yamlContent := `
server:
  http:
    listen: ":8080"
  mcp:
    listen: ":8081"
`
		tmpFile, err := os.CreateTemp("", "config*.yaml")
		require.NoError(t, err)
		defer os.Remove(tmpFile.Name())

		_, err = tmpFile.WriteString(yamlContent)
		require.NoError(t, err)
		tmpFile.Close()

		_, err = Load(tmpFile.Name())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid configuration")
		assert.Contains(t, err.Error(), "storage.base_dir is required")
	})

	t.Run("validation failure - missing server.http.listen", func(t *testing.T) {
		yamlContent := `
storage:
  base_dir: "/tmp/shoka"
server:
  mcp:
    listen: ":8081"
`
		tmpFile, err := os.CreateTemp("", "config*.yaml")
		require.NoError(t, err)
		defer os.Remove(tmpFile.Name())

		_, err = tmpFile.WriteString(yamlContent)
		require.NoError(t, err)
		tmpFile.Close()

		_, err = Load(tmpFile.Name())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid configuration")
		assert.Contains(t, err.Error(), "server.http.listen is required")
	})

	t.Run("validation failure - missing server.mcp.listen", func(t *testing.T) {
		yamlContent := `
storage:
  base_dir: "/tmp/shoka"
server:
  http:
    listen: ":8080"
`
		tmpFile, err := os.CreateTemp("", "config*.yaml")
		require.NoError(t, err)
		defer os.Remove(tmpFile.Name())

		_, err = tmpFile.WriteString(yamlContent)
		require.NoError(t, err)
		tmpFile.Close()

		_, err = Load(tmpFile.Name())
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid configuration")
		assert.Contains(t, err.Error(), "server.mcp.listen is required")
	})
}

// writeConfig is a small helper for the storage-redesign config tests.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	tmpFile, err := os.CreateTemp("", "config*.yaml")
	require.NoError(t, err)
	t.Cleanup(func() { os.Remove(tmpFile.Name()) })
	_, err = tmpFile.WriteString(body)
	require.NoError(t, err)
	require.NoError(t, tmpFile.Close())
	return tmpFile.Name()
}

const minimalServerStorage = `
server:
  http:
    listen: ":8080"
  mcp:
    listen: ":8081"
storage:
  base_dir: "/tmp/shoka"
`

func TestLoad_StorageRedesignDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalServerStorage))
	require.NoError(t, err)

	// §12 baked-in defaults applied when the blocks are absent.
	assert.Equal(t, 5*time.Minute, cfg.FileLock.MaxLeaseDuration.Std())
	assert.Equal(t, 1*time.Second, cfg.FileLock.ReaperInterval.Std())
	assert.Equal(t, 1000, cfg.WAL.MaxEntries)
	assert.Equal(t, 1, cfg.WALWorker.MinWorkers)
	assert.Equal(t, 8, cfg.WALWorker.MaxWorkers)
	assert.Equal(t, 30*time.Second, cfg.WALWorker.IdleTimeout.Std())
	assert.Equal(t, 100*time.Millisecond, cfg.WALWorker.ScanInterval.Std())
	assert.Equal(t, 100*time.Millisecond, cfg.WALWorker.BackoffInitial.Std())
	assert.Equal(t, 30*time.Second, cfg.WALWorker.BackoffMax.Std())

	// drift_scan.on_startup defaults to true; interval defaults to 0 (disabled).
	assert.True(t, cfg.Storage.DriftScan.OnStartupEnabled())
	assert.Equal(t, time.Duration(0), cfg.Storage.DriftScan.Interval.Std())

	// metrics endpoint is off by default.
	assert.Equal(t, "", cfg.Metrics.Addr)

	// notification center ring buffer defaults to 1000.
	assert.Equal(t, 1000, cfg.Notify.MaxEntries)
}

func TestLoad_StorageRedesignOverrides(t *testing.T) {
	body := minimalServerStorage + `  drift_scan:
    on_startup: false
    interval: 5m
filelock:
  max_lease_duration: 2m
  reaper_interval: 500ms
wal:
  max_entries: 50
wal_worker:
  min_workers: 2
  max_workers: 4
  idle_timeout: 10s
  scan_interval: 250ms
  backoff_initial: 50ms
  backoff_max: 1m
notify:
  max_entries: 25
metrics:
  addr: "localhost:9090"
`
	cfg, err := Load(writeConfig(t, body))
	require.NoError(t, err)

	// Explicit on_startup:false must be honoured (not overwritten by the default).
	assert.False(t, cfg.Storage.DriftScan.OnStartupEnabled())
	assert.Equal(t, 5*time.Minute, cfg.Storage.DriftScan.Interval.Std())
	assert.Equal(t, 2*time.Minute, cfg.FileLock.MaxLeaseDuration.Std())
	assert.Equal(t, 500*time.Millisecond, cfg.FileLock.ReaperInterval.Std())
	assert.Equal(t, 50, cfg.WAL.MaxEntries)
	assert.Equal(t, 2, cfg.WALWorker.MinWorkers)
	assert.Equal(t, 4, cfg.WALWorker.MaxWorkers)
	assert.Equal(t, 10*time.Second, cfg.WALWorker.IdleTimeout.Std())
	assert.Equal(t, 250*time.Millisecond, cfg.WALWorker.ScanInterval.Std())
	assert.Equal(t, 50*time.Millisecond, cfg.WALWorker.BackoffInitial.Std())
	assert.Equal(t, 1*time.Minute, cfg.WALWorker.BackoffMax.Std())
	assert.Equal(t, 25, cfg.Notify.MaxEntries)
	assert.Equal(t, "localhost:9090", cfg.Metrics.Addr)
}

func TestLoad_StorageRedesignErrors(t *testing.T) {
	t.Run("invalid duration", func(t *testing.T) {
		_, err := Load(writeConfig(t, minimalServerStorage+`filelock:
  max_lease_duration: "not-a-duration"
`))
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid duration")
	})

	t.Run("min_workers exceeds max_workers", func(t *testing.T) {
		_, err := Load(writeConfig(t, minimalServerStorage+`wal_worker:
  min_workers: 9
  max_workers: 4
`))
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "must not exceed max_workers")
	})
}
