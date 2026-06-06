// Package clientconfig is the reader/writer for the Shoka CLI's per-environment
// client configuration (B-46b). It is greenfield: no client-side config existed
// before the CLI. The config is deliberately SEPARATE from the server's startup
// config (shoka.example.yaml) — the server config never carries a client token,
// and this client config is the ONLY place the operator's access token lives, on
// the local disk with restrictive permissions, never in any committed artefact.
//
// Path: os.UserConfigDir()/shoka/<environment>/config.yaml (XDG-correct — Go's
// os.UserConfigDir honours $XDG_CONFIG_HOME, falling back to ~/.config on Linux
// and ~/Library/Application Support on macOS). <environment> selects the instance
// (its endpoint and that environment's token), so one operator can hold creds for
// several Shoka deployments side by side.
//
// This package carries NO Shoka-specific judgement — it only persists and loads a
// small struct. All ingest/catalog/format logic stays server-side (the thin-client
// principle the whole CLI line follows).
package clientconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultEnvironment is the environment selected when the operator names none.
const DefaultEnvironment = "default"

// Config is one environment's client configuration. Token is the only secret and
// is local-only — it is never logged, printed, or written anywhere but this file.
type Config struct {
	// Endpoint is the Shoka MCP endpoint URL the CLI connects to (operator-set,
	// abstract — no deployment topology is baked into source or any artefact).
	Endpoint string `yaml:"endpoint"`
	// Token is the OAuth access token (the display-once token-to-self credential).
	// THE ONLY SECRET. Local-only; never committed, never logged.
	Token string `yaml:"token"`
	// DefaultNamespace / DefaultProject are optional ergonomics so subcommands can
	// omit the namespace/project when the operator works mostly in one place.
	DefaultNamespace string `yaml:"default_namespace,omitempty"`
	DefaultProject   string `yaml:"default_project,omitempty"`
}

// dirPerm/filePerm keep the config tree operator-private: the directory is
// owner-only (0700) and the file owner-read/write (0600), because the file holds
// the access token.
const (
	dirPerm  os.FileMode = 0o700
	filePerm os.FileMode = 0o600
)

// normalizeEnvironment validates and defaults the environment name. An empty name
// becomes DefaultEnvironment; a name containing a path separator is rejected so it
// can never escape the shoka/ config subtree.
func normalizeEnvironment(environment string) (string, error) {
	env := strings.TrimSpace(environment)
	if env == "" {
		return DefaultEnvironment, nil
	}
	if strings.ContainsAny(env, `/\`) || env == "." || env == ".." {
		return "", fmt.Errorf("clientconfig: invalid environment name %q", environment)
	}
	return env, nil
}

// Dir returns the config directory for an environment:
// os.UserConfigDir()/shoka/<environment>.
func Dir(environment string) (string, error) {
	env, err := normalizeEnvironment(environment)
	if err != nil {
		return "", err
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("clientconfig: locate user config dir: %w", err)
	}
	return filepath.Join(base, "shoka", env), nil
}

// Path returns the config.yaml path for an environment.
func Path(environment string) (string, error) {
	dir, err := Dir(environment)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.yaml"), nil
}

// Load reads and parses the config for an environment. A missing file is reported
// as a wrapped os.ErrNotExist so callers can tell "not authenticated yet" from a
// real read/parse failure.
func Load(environment string) (*Config, error) {
	path, err := Path(environment)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("clientconfig: no config for environment %q at %s: %w", environment, path, os.ErrNotExist)
		}
		return nil, fmt.Errorf("clientconfig: read %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("clientconfig: parse %s: %w", path, err)
	}
	return &cfg, nil
}

// Save writes cfg for an environment, creating the directory (0700) and file
// (0600) with restrictive permissions. The write is atomic (temp file in the same
// directory, then rename) so a crash mid-write never leaves a truncated config —
// and the temp file is created 0600 so the token is never briefly world-readable.
func Save(environment string, cfg *Config) error {
	dir, err := Dir(environment)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("clientconfig: create %s: %w", dir, err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("clientconfig: marshal config: %w", err)
	}
	final := filepath.Join(dir, "config.yaml")
	tmp, err := os.CreateTemp(dir, ".config-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("clientconfig: create temp config: %w", err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename.
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(filePerm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("clientconfig: chmod temp config: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("clientconfig: write temp config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("clientconfig: close temp config: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("clientconfig: install config %s: %w", final, err)
	}
	return nil
}
