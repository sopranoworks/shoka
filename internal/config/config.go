package config

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that unmarshals from a YAML scalar written in
// Go's duration syntax ("5m", "100ms", "30s") or as a bare 0. An empty/absent
// value decodes to 0; callers treat 0 as "use the baked-in default" except for
// fields where 0 is itself meaningful (e.g. storage.drift_scan.interval, where
// 0 disables periodic re-scan).
type Duration time.Duration

// UnmarshalYAML parses the node's scalar value with time.ParseDuration.
func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	raw := value.Value
	if raw == "" || raw == "0" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", raw, err)
	}
	*d = Duration(parsed)
	return nil
}

// Std returns the value as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// LogConfig configures structured server logging (written to stderr). An absent
// block means level "info" and format "text"; empty values are resolved to those
// defaults by internal/logging at startup.
type LogConfig struct {
	Level  string `yaml:"level"`  // error | warn | info | debug ("" = info)
	Format string `yaml:"format"` // text | json ("" = text)
}

type ServerSettings struct {
	Listen      string    `yaml:"listen"`
	ExternalURL string    `yaml:"external_url"`
	TLS         TLSConfig `yaml:"tls"`
}

// AuthConfig configures static Bearer-token authentication and the WebSocket
// origin policy. As of B-50 this is NO LONGER a global MCP gate: MCP access is
// decided per transport by which port a request arrives on (see MCPConfig).
// AuthConfig now serves two roles: (1) the API-Token source the plain MCP
// transport validates against when server.mcp.plain.bearer_auth is on, and
// (2) the token + origin policy for the Web UI / non-MCP endpoints
// (/ws/ui, /drafts, /api). When Enabled is false (the default) those non-MCP
// endpoints require no token and accept all WebSocket origins, preserving
// single-operator local behaviour.
type AuthConfig struct {
	Enabled        bool     `yaml:"enabled"`
	Tokens         []string `yaml:"tokens"`
	AllowedOrigins []string `yaml:"allowed_origins"`
}

// MCPConfig configures the MCP transport surface (B-50). Shoka runs the MCP
// transport as up to two instances over one shared worker layer, selected by
// PRESENCE of a listen address:
//
//   - Plain.Listen set  → open the plain (normal/internal) transport;
//   - OAuth.Listen set  → open the OAuth (external) transport;
//   - both set          → open both (the external-OAuth + internal split);
//   - neither set        → startup error (a Shoka with no MCP transport is invalid).
//
// "Is OAuth active" reduces to "is OAuth.Listen configured" — there is no
// separate enable flag. Neither block carries TLS: Shoka terminates no TLS by
// design; both ports sit behind an external TLS-terminating reverse proxy.
type MCPConfig struct {
	Plain PlainTransportConfig `yaml:"plain"`
	OAuth OAuthTransportConfig `yaml:"oauth"`
}

// PlainTransportConfig is the normal/internal MCP transport. With BearerAuth off
// it is unauthenticated (loopback/internal use — the internal client connects
// directly). With BearerAuth on it requires a static API-Token presented as
// `Authorization: Bearer <token>`, validated against server.auth.tokens — for a
// non-loopback internal host that must be behind a TLS-terminating proxy (the
// token rides in cleartext otherwise).
type PlainTransportConfig struct {
	Listen      string `yaml:"listen"`
	ExternalURL string `yaml:"external_url"`
	BearerAuth  bool   `yaml:"bearer_auth"`
}

// OAuthTransportConfig is the external MCP transport: PURELY OAuth (B-39 —
// authorize/token/PKCE/consent/CIMD). Static bearer auth is intentionally NOT
// mixed in (an accident risk on the external port, forbidden by design). It
// carries the OAuth fields relocated from the former server.auth.oauth block; the
// `enabled` flag is gone (presence of Listen is the switch). ExternalURL is the
// public origin behind the TLS proxy, used to compose the discovery / AS URLs
// (or the proxy's X-Forwarded-* headers when empty); see internal/serverurl.
type OAuthTransportConfig struct {
	Listen      string `yaml:"listen"`
	ExternalURL string `yaml:"external_url"`
	// ConsentCredential gates the /authorize approval in single-user mode (the
	// pluggable principal-auth seam; B-39 (b)). The operator sets a secret; an
	// empty value denies all consent (a safe default — consent cannot be granted
	// until configured). Multi-user enablement later replaces this seam with
	// per-user authentication.
	ConsentCredential string `yaml:"consent_credential"`
	// TrustedClientMetadataDomains is the allowlist of client (CIMD) metadata
	// domains Shoka will fetch and trust (a host or any subdomain of it). It is
	// default-deny: empty means no client can connect, so the operator MUST list
	// the legitimate connector domain(s) here. The value lives ONLY in config,
	// never in source — confidentiality and flexibility.
	TrustedClientMetadataDomains []string `yaml:"trusted_client_metadata_domains"`
	// Token lifetimes (0 = built-in defaults: access 1h, refresh 30d, code 1m).
	AccessTokenTTL       Duration `yaml:"access_token_ttl"`
	RefreshTokenTTL      Duration `yaml:"refresh_token_ttl"`
	AuthorizationCodeTTL Duration `yaml:"authorization_code_ttl"`
}

// WebhookConfig describes one outbound webhook subscription. Events is any of
// "file_written", "file_deleted", "project_created".
type WebhookConfig struct {
	Name   string   `yaml:"name"`
	URL    string   `yaml:"url"`
	Events []string `yaml:"events"`
	Secret string   `yaml:"secret"`
}

// DriftScanConfig controls the per-project working-tree-vs-git drift detection
// added by the storage redesign. OnStartup runs a scan once at startup; a
// non-zero Interval enables periodic re-scans (0 = disabled). OnStartup is a
// pointer so an absent block defaults to true while an explicit false is kept.
type DriftScanConfig struct {
	OnStartup *bool    `yaml:"on_startup"`
	Interval  Duration `yaml:"interval"`
}

// OnStartupEnabled reports the effective on_startup value (default true).
func (d DriftScanConfig) OnStartupEnabled() bool {
	return d.OnStartup == nil || *d.OnStartup
}

// LostFoundConfig controls the lost+found worker (the 2026-06-02 directive): a
// periodic sweep that deletes untracked files matching shoka.disposable and
// moves the rest to a per-project lost+found area, restoring the tracked-only
// invariant. Enabled is a pointer so an absent block defaults to true while an
// explicit false is kept; Interval defaults to 5m (applied in applyDefaults).
// Set enabled:false to disable for dev/debug.
type LostFoundConfig struct {
	Enabled  *bool    `yaml:"enabled"`
	Interval Duration `yaml:"interval"`
}

// IsEnabled reports the effective enabled value (default true).
func (l LostFoundConfig) IsEnabled() bool {
	return l.Enabled == nil || *l.Enabled
}

// IndexConfig controls the per-project derivative index repair worker (the
// 2026-06-04 I1 directive): a periodic sweep that reconciles each project's
// index.db with HEAD, rebuilding it wholesale from working-tree bytes when stale,
// missing, or corrupt. Enabled is a pointer so an absent block defaults to true
// while an explicit false is kept; Interval defaults to 5m (applied in
// applyDefaults), mirroring the lost+found cadence. Set enabled:false to disable
// for dev/debug.
type IndexConfig struct {
	Enabled  *bool    `yaml:"enabled"`
	Interval Duration `yaml:"interval"`
}

// IsEnabled reports the effective enabled value (default true).
func (i IndexConfig) IsEnabled() bool {
	return i.Enabled == nil || *i.Enabled
}

// FileLockConfig configures the per-file lock manager (internal/storage/filelock).
type FileLockConfig struct {
	MaxLeaseDuration Duration `yaml:"max_lease_duration"` // default 5m
	ReaperInterval   Duration `yaml:"reaper_interval"`    // default 1s
}

// WALConfig configures the write-ahead log (internal/storage/wal).
type WALConfig struct {
	MaxEntries int `yaml:"max_entries"` // write-disabled threshold; default 1000
}

// NotifyConfig configures the in-process notification center
// (internal/notify): the ring-buffer size of recent storage-activity events.
type NotifyConfig struct {
	MaxEntries int `yaml:"max_entries"` // ring buffer size; default 1000
}

// WALWorkerConfig configures the background git-commit worker pool
// (internal/storage/walworker).
type WALWorkerConfig struct {
	MinWorkers     int      `yaml:"min_workers"`     // default 1
	MaxWorkers     int      `yaml:"max_workers"`     // default 8
	IdleTimeout    Duration `yaml:"idle_timeout"`    // default 30s
	ScanInterval   Duration `yaml:"scan_interval"`   // default 100ms
	BackoffInitial Duration `yaml:"backoff_initial"` // default 100ms
	BackoffMax     Duration `yaml:"backoff_max"`     // default 30s
}

// CatalogConfig configures the per-project catalog (the 2026-05-30 catalog
// directive). It currently exposes no tunable fields — bbolt defaults are used
// and the DB path is implicit (<base_dir>/<namespace>/<project>.db). The struct
// exists so a future directive can add knobs without changing the config schema.
type CatalogConfig struct{}

// MetricsConfig configures the optional Prometheus metrics endpoint. An empty
// Addr (the default) leaves the endpoint unregistered. A non-empty Addr is
// forced to a loopback host, mirroring the pprof endpoint's defaults.
type MetricsConfig struct {
	Addr string `yaml:"addr"`
}

// IdentityConfig configures who Shoka records as the author of the git commits
// it produces. PROVISIONAL: this is single-user mode — the floor of a larger
// authentication design (maintenance backlog B-28), NOT that design. There is no
// authentication here; User is the one configured operator, and a future
// multi-user auth mechanism substitutes a per-request authenticated user without
// changing this shape. Agents (MCP clients) declare their own name/worker at
// connect time (clientInfo + initialize _meta); AgentDefault is the fallback for
// clients that declare nothing.
type IdentityConfig struct {
	User struct {
		Name  string `yaml:"name"`
		Email string `yaml:"email"`
	} `yaml:"user"`
	AgentDefault struct {
		Name   string `yaml:"name"`
		Worker string `yaml:"worker"`
	} `yaml:"agent_default"`
}

type Config struct {
	Server struct {
		HTTP ServerSettings `yaml:"http"`
		MCP  MCPConfig      `yaml:"mcp"`
		Auth AuthConfig     `yaml:"auth"`
		Log  LogConfig      `yaml:"log"`
	} `yaml:"server"`
	Identity IdentityConfig `yaml:"identity"`
	Storage  struct {
		BaseDir   string          `yaml:"base_dir"`
		DriftScan DriftScanConfig `yaml:"drift_scan"`
		LostFound LostFoundConfig `yaml:"lost_found"`
		Index     IndexConfig     `yaml:"index"`
	} `yaml:"storage"`
	Services struct {
		GoogleCloud struct {
			ProjectID string `yaml:"project_id"`
		} `yaml:"google_cloud"`
	} `yaml:"services"`
	FileLock  FileLockConfig  `yaml:"filelock"`
	WAL       WALConfig       `yaml:"wal"`
	WALWorker WALWorkerConfig `yaml:"wal_worker"`
	Notify    NotifyConfig    `yaml:"notify"`
	Metrics   MetricsConfig   `yaml:"metrics"`
	Catalog   CatalogConfig   `yaml:"catalog"`
	Webhooks  []WebhookConfig `yaml:"webhooks"`
}

// applyDefaults fills zero-valued storage-redesign tunables with the defaults
// from the directive (§12). These defaults also live in the component packages
// (so a zero value remains safe there); resolving them here keeps the wired
// configuration explicit and inspectable. Fields where 0 is a meaningful value
// (drift_scan.interval, metrics.addr) are left untouched.
func (c *Config) applyDefaults() {
	if c.FileLock.MaxLeaseDuration == 0 {
		c.FileLock.MaxLeaseDuration = Duration(5 * time.Minute)
	}
	if c.FileLock.ReaperInterval == 0 {
		c.FileLock.ReaperInterval = Duration(1 * time.Second)
	}
	if c.WAL.MaxEntries == 0 {
		c.WAL.MaxEntries = 1000
	}
	if c.Notify.MaxEntries == 0 {
		c.Notify.MaxEntries = 1000
	}
	if c.WALWorker.MinWorkers == 0 {
		c.WALWorker.MinWorkers = 1
	}
	if c.WALWorker.MaxWorkers == 0 {
		c.WALWorker.MaxWorkers = 8
	}
	if c.WALWorker.IdleTimeout == 0 {
		c.WALWorker.IdleTimeout = Duration(30 * time.Second)
	}
	if c.WALWorker.ScanInterval == 0 {
		c.WALWorker.ScanInterval = Duration(100 * time.Millisecond)
	}
	if c.WALWorker.BackoffInitial == 0 {
		c.WALWorker.BackoffInitial = Duration(100 * time.Millisecond)
	}
	if c.WALWorker.BackoffMax == 0 {
		c.WALWorker.BackoffMax = Duration(30 * time.Second)
	}
	// Identity defaults (single-user mode). Absent config still yields a valid,
	// intentional author rather than falling back to environmental git config.
	if c.Identity.User.Name == "" {
		c.Identity.User.Name = "Shoka Operator"
	}
	if c.Identity.User.Email == "" {
		c.Identity.User.Email = "operator@shoka.local"
	}
	if c.Identity.AgentDefault.Name == "" {
		c.Identity.AgentDefault.Name = "shoka-agent"
	}
	// Lost+found worker: default to a 5-minute sweep interval (the directive's
	// default). Enabled defaults to true via LostFoundConfig.IsEnabled.
	if c.Storage.LostFound.Interval == 0 {
		c.Storage.LostFound.Interval = Duration(5 * time.Minute)
	}
	// Index repair worker (I1): default to a 5-minute sweep interval, mirroring
	// lost+found. Enabled defaults to true via IndexConfig.IsEnabled.
	if c.Storage.Index.Interval == 0 {
		c.Storage.Index.Interval = Duration(5 * time.Minute)
	}
}

func (c *Config) Validate() error {
	if c.Storage.BaseDir == "" {
		return errors.New("storage.base_dir is required")
	}
	if c.Server.HTTP.Listen == "" {
		return errors.New("server.http.listen is required")
	}
	// B-50 presence semantics: at least one MCP transport must be configured.
	// Each opens iff its listen address is set; both is valid; neither is invalid
	// (a Shoka with no MCP transport serves nothing). This replaces the former
	// single `server.mcp.listen is required` check.
	if c.Server.MCP.Plain.Listen == "" && c.Server.MCP.OAuth.Listen == "" {
		return errors.New("at least one MCP transport must be configured: set server.mcp.plain.listen and/or server.mcp.oauth.listen")
	}
	switch c.Server.Log.Level {
	case "", "error", "warn", "info", "debug":
	default:
		return fmt.Errorf("server.log.level must be one of error|warn|info|debug, got %q", c.Server.Log.Level)
	}
	switch c.Server.Log.Format {
	case "", "text", "json":
	default:
		return fmt.Errorf("server.log.format must be one of text|json, got %q", c.Server.Log.Format)
	}
	if c.WALWorker.MinWorkers < 0 || c.WALWorker.MaxWorkers < 0 {
		return errors.New("wal_worker.min_workers and wal_worker.max_workers must be non-negative")
	}
	if c.WALWorker.MaxWorkers > 0 && c.WALWorker.MinWorkers > c.WALWorker.MaxWorkers {
		return fmt.Errorf("wal_worker.min_workers (%d) must not exceed max_workers (%d)", c.WALWorker.MinWorkers, c.WALWorker.MaxWorkers)
	}
	if c.WAL.MaxEntries < 0 {
		return errors.New("wal.max_entries must be non-negative")
	}
	return nil
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config YAML: %w", err)
	}

	cfg.applyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &cfg, nil
}
