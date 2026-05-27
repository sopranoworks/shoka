package config

import (
	"errors"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type ServerSettings struct {
	Listen      string    `yaml:"listen"`
	ExternalURL string    `yaml:"external_url"`
	TLS         TLSConfig `yaml:"tls"`
}

// AuthConfig configures optional Bearer-token authentication. When Enabled is
// false (the default) no authentication is performed and all WebSocket origins
// are accepted, preserving single-operator local behaviour.
type AuthConfig struct {
	Enabled        bool     `yaml:"enabled"`
	Tokens         []string `yaml:"tokens"`
	AllowedOrigins []string `yaml:"allowed_origins"`
}

// WebhookConfig describes one outbound webhook subscription. Events is any of
// "file_written", "file_deleted", "project_created".
type WebhookConfig struct {
	Name   string   `yaml:"name"`
	URL    string   `yaml:"url"`
	Events []string `yaml:"events"`
	Secret string   `yaml:"secret"`
}

type Config struct {
	Server struct {
		HTTP ServerSettings `yaml:"http"`
		MCP  ServerSettings `yaml:"mcp"`
		Auth AuthConfig     `yaml:"auth"`
	} `yaml:"server"`
	Storage struct {
		BaseDir string `yaml:"base_dir"`
	} `yaml:"storage"`
	Services struct {
		GoogleCloud struct {
			ProjectID string `yaml:"project_id"`
		} `yaml:"google_cloud"`
	} `yaml:"services"`
	Webhooks []WebhookConfig `yaml:"webhooks"`
}

func (c *Config) Validate() error {
	if c.Storage.BaseDir == "" {
		return errors.New("storage.base_dir is required")
	}
	if c.Server.HTTP.Listen == "" {
		return errors.New("server.http.listen is required")
	}
	if c.Server.MCP.Listen == "" {
		return errors.New("server.mcp.listen is required")
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

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	return &cfg, nil
}
