package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
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

// LogConfig configures structured server logging. It controls WHERE log output
// goes (the destination), not just its level/format. An absent block means
// level "info", format "text", and the stderr destination — byte-for-byte the
// historical behaviour. Empty level/format are resolved to defaults by
// internal/logging at startup; an empty/"stderr" Output keeps stderr.
type LogConfig struct {
	Level  string `yaml:"level"`  // error | warn | info | debug ("" = info)
	Format string `yaml:"format"` // text | json ("" = text)
	// Output selects the log destination: "stderr" (default, unchanged) or
	// "file". slog already writes to a configurable io.Writer; this is the knob
	// that selects which one. Room is left for further destinations later.
	Output string        `yaml:"output"` // stderr (default) | file
	File   LogFileConfig `yaml:"file"`   // used only when Output == "file"
}

// LogFileConfig configures the "file" log destination (B-66). The on-disk
// footprint is BOUNDED by lumberjack: the active file rotates on size, and old
// backups are pruned by count and age. Zero values resolve to bounded defaults
// (see internal/logging) so a file destination can never grow without limit —
// lumberjack itself keeps every backup when MaxBackups and MaxAge are both zero,
// which we deliberately avoid. By default the file is also rotated at least once
// per day even with no size pressure (lumberjack rotates on size alone).
type LogFileConfig struct {
	Path       string `yaml:"path"`         // required when output: file
	MaxSizeMB  int    `yaml:"max_size_mb"`  // rotate when the active file exceeds this (0 => 100)
	MaxBackups int    `yaml:"max_backups"`  // rotated backups to retain (0 => 7)
	MaxAgeDays int    `yaml:"max_age_days"` // days to retain rotated backups (0 => 30)
	Compress   bool   `yaml:"compress"`     // gzip rotated backups (default false)
	// RotateDaily drives a once-per-day Rotate() so the active file cycles at
	// least daily regardless of size. Unset (nil) => true (the default must
	// rotate at-least-daily); set false for size-only rotation.
	RotateDaily *bool `yaml:"rotate_daily"`
}

type ServerSettings struct {
	Listen      string    `yaml:"listen"`
	ExternalURL string    `yaml:"external_url"`
	TLS         TLSConfig `yaml:"tls"`
}

// DebugConfig gates verbose, operator-only diagnostics. DumpHTTP (B-56) enables a
// verbatim dump of every HTTP request and response on the three listeners
// (secrets redacted to a fixed marker). It is OFF by default: when off, behaviour
// and the existing logs are unchanged; when on, the dump is complete (no field
// selection) so no future connect failure can hide in an un-logged field.
type DebugConfig struct {
	DumpHTTP bool `yaml:"dump_http"`
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
	// RegistrationMode selects which client-registration posture the OAuth AS
	// metadata advertises (B-63 §0.1). "cimd" (default, empty) advertises the CIMD
	// signal (client_id_metadata_document_supported:true) and NO registration_endpoint;
	// "dcr" advertises registration_endpoint (RFC 7591) and WITHHOLDS the CIMD signal.
	// The two cannot both be advertised if DCR is to be reachable: Claude's selection
	// rule skips Dynamic Client Registration whenever the CIMD signal is present, so
	// claude.ai would keep choosing CIMD and never call /register. Both modes keep
	// token_endpoint_auth_methods_supported:["none"] (public client). Both
	// client-resolution code paths stay in the binary and /register stays mounted in
	// either mode — only the advertised metadata differs, so the operator flips this
	// switch and re-tests the live claude.ai connect each way with no logic rebuild.
	RegistrationMode string `yaml:"registration_mode"`
}

// RegistrationModeOrDefault returns the effective registration posture, mapping an
// empty value to "cimd" (today's behaviour). Validate() guarantees the stored value
// is one of "", "cimd", "dcr".
func (o OAuthTransportConfig) RegistrationModeOrDefault() string {
	if o.RegistrationMode == "" {
		return "cimd"
	}
	return o.RegistrationMode
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
		HTTP  ServerSettings `yaml:"http"`
		MCP   MCPConfig      `yaml:"mcp"`
		Auth  AuthConfig     `yaml:"auth"`
		Log   LogConfig      `yaml:"log"`
		Debug DebugConfig    `yaml:"debug"`
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
	// B-66 log destination: stderr (default) or file. A file destination needs a
	// path, and its bound knobs must be non-negative. The writer is not opened
	// here — validation is side-effect-free (B-58 --config-check binds nothing).
	switch c.Server.Log.Output {
	case "", "stderr":
	case "file":
		if c.Server.Log.File.Path == "" {
			return errors.New("server.log.file.path is required when server.log.output is \"file\"")
		}
	default:
		return fmt.Errorf("server.log.output must be one of stderr|file, got %q", c.Server.Log.Output)
	}
	if c.Server.Log.File.MaxSizeMB < 0 || c.Server.Log.File.MaxBackups < 0 || c.Server.Log.File.MaxAgeDays < 0 {
		return errors.New("server.log.file max_size_mb, max_backups and max_age_days must be >= 0 (0 = bounded default)")
	}
	// B-63 §2.3: the registration posture is cimd (default) or dcr; any other value
	// is a config error so a typo cannot silently leave the AS advertising neither.
	switch c.Server.MCP.OAuth.RegistrationMode {
	case "", "cimd", "dcr":
	default:
		return fmt.Errorf("server.mcp.oauth.registration_mode must be one of cimd|dcr, got %q", c.Server.MCP.OAuth.RegistrationMode)
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
	// Strict decoding (B-58): an unknown or misplaced key is a HARD error naming the
	// offending key + line, not silently dropped. Before B-58 yaml.Unmarshal ignored
	// unrecognised keys, so a typo'd / wrongly-nested setting (e.g. dump_http outside
	// server.debug) took effect nowhere with no error — discoverable only by restart
	// and trial-connect (the B-57 debug cycle). KnownFields(true) turns that into a
	// load failure that names the offending key. The full legitimate key set is the
	// yaml tags on Config and its nested structs; shoka.example.yaml decodes cleanly.
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		// io.EOF means an empty document (no keys); that is an all-defaults config, so
		// Validate below reports the missing required fields exactly as it did before.
		return nil, fmt.Errorf("failed to parse config YAML: %w", err)
	}

	cfg.applyDefaults()

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &cfg, nil
}
