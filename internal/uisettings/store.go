// Package uisettings persists WebUI-set librarian overrides (max_steps,
// base_url) to a JSON file so they survive server restarts. These values
// take precedence over the config-file defaults.
package uisettings

import (
	"encoding/json"
	"os"
	"sync"
)

// Settings holds the WebUI-persisted librarian overrides. Pointer fields
// distinguish "never set" (nil) from "set to zero/empty".
type Settings struct {
	MaxSteps *int    `json:"maxSteps,omitempty"`
	BaseURL  *string `json:"baseUrl,omitempty"`
}

// Store reads and writes the settings file. All methods are goroutine-safe.
type Store struct {
	path string
	mu   sync.Mutex
	data Settings
}

// New opens (or creates) the settings store at path. A missing file is not an
// error — it means no overrides have been saved yet.
func New(path string) (*Store, error) {
	s := &Store{path: path}
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s.data); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// Get returns the current overrides (never nil fields if never set).
func (s *Store) Get() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data
}

// SetMaxSteps persists a new max_steps override.
func (s *Store) SetMaxSteps(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.MaxSteps = &n
	return s.flush()
}

// SetBaseURL persists a new base_url override.
func (s *Store) SetBaseURL(url string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.BaseURL = &url
	return s.flush()
}

func (s *Store) flush() error {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, raw, 0o644)
}
