package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// B-58 — strict config decoding. Before B-58, yaml.Unmarshal silently dropped any
// unknown or misplaced key, so a typo'd / wrongly-nested setting (the B-57 root cause:
// server.debug.dump_http landing nowhere) took effect with NO error, discoverable only
// by restart-and-trial-connect. Load now decodes with KnownFields(true): an unrecognised
// or misplaced key is a hard load error naming the offending key + line, while every
// currently-valid config (incl. shoka.example.yaml) still decodes cleanly.

// loadYAML writes content to a temp file and runs the real Load path.
func loadYAML(t *testing.T, content string) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "shoka.yaml")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return Load(path)
}

func TestLoad_StrictRejectsUnknownTopLevelKey(t *testing.T) {
	_, err := loadYAML(t, `
server:
  http:
    listen: ":8080"
  mcp:
    plain:
      listen: ":8081"
storage:
  base_dir: "/tmp/shoka"
storagee:            # typo for "storage"
  base_dir: "/tmp/shoka"
`)
	require.Error(t, err, "an unknown top-level key must fail the load")
	assert.Contains(t, err.Error(), "storagee", "the error must name the offending key")
}

func TestLoad_StrictRejectsMisplacedKey(t *testing.T) {
	// dump_http is a real key, but it belongs under server.debug — here it is one level
	// too high (directly under server). Strict decoding must reject it by name; this is
	// exactly the class of mistake (B-57) that previously took effect nowhere, silently.
	_, err := loadYAML(t, `
server:
  http:
    listen: ":8080"
  mcp:
    plain:
      listen: ":8081"
  dump_http: true     # misplaced: belongs under server.debug
storage:
  base_dir: "/tmp/shoka"
`)
	require.Error(t, err, "a known key in the wrong block must fail the load")
	assert.Contains(t, err.Error(), "dump_http", "the error must name the misplaced key")
}

func TestLoad_StrictAcceptsExampleYAML(t *testing.T) {
	// The canonical annotated config must decode cleanly under strict mode — the change
	// rejects INVALID configs, never tightens valid ones.
	path := filepath.Join("..", "..", "shoka.example.yaml")
	cfg, err := Load(path)
	require.NoError(t, err, "shoka.example.yaml must pass strict decoding")
	require.NotNil(t, cfg)
}

func TestLoad_StrictAcceptsFullValidConfig(t *testing.T) {
	// A representative config exercising the full legitimate key set — both MCP
	// transports incl. the relocated OAuth fields + durations, server.debug.dump_http
	// IN ITS CORRECT PLACE, the storage workers (pointer-bool + duration), the write
	// pipeline tunables, metrics, identity, and a webhook — all decode without a
	// strict-mode rejection.
	cfg, err := loadYAML(t, `
server:
  http:
    listen: ":8080"
    external_url: "http://localhost:8080"
    tls:
      enabled: false
  mcp:
    plain:
      listen: ":8081"
      external_url: "http://localhost:8081"
      bearer_auth: true
    oauth:
      listen: ":8082"
      external_url: "https://public.example"
      consent_credential: "a-secret"
      trusted_client_metadata_domains:
        - "connector.example"
      access_token_ttl: "1h"
      refresh_token_ttl: "720h"
      authorization_code_ttl: "1m"
  auth:
    enabled: true
    tokens:
      - "tok"
    allowed_origins:
      - "http://localhost:8080"
  log:
    level: "debug"
    format: "json"
  debug:
    dump_http: true
identity:
  user:
    name: "Op"
    email: "op@example.local"
  agent_default:
    name: "agent"
    worker: ""
storage:
  base_dir: "/tmp/shoka"
  drift_scan:
    on_startup: false
    interval: "5m"
  lost_found:
    enabled: true
    interval: "5m"
  index:
    enabled: true
    interval: "5m"
filelock:
  max_lease_duration: "5m"
  reaper_interval: "1s"
wal:
  max_entries: 1000
wal_worker:
  min_workers: 1
  max_workers: 8
  idle_timeout: "30s"
  scan_interval: "100ms"
  backoff_initial: "100ms"
  backoff_max: "30s"
notify:
  max_entries: 1000
metrics:
  addr: ""
catalog: {}
webhooks:
  - name: hook
    url: "https://example.com/h"
    events: [file_written]
    secret: "s"
`)
	require.NoError(t, err, "a full valid config must decode under strict mode")
	require.NotNil(t, cfg)
	assert.True(t, cfg.Server.Debug.DumpHTTP)
	assert.Equal(t, ":8082", cfg.Server.MCP.OAuth.Listen)
	assert.True(t, cfg.Server.MCP.Plain.BearerAuth)
}
