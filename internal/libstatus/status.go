// Package libstatus holds the cached health of the ask_the_librarian LLM config
// (B-73): the result of a one-call check, shared between the startup check and
// the WebUI status surface (with a manual refresh). It deliberately exposes only
// validity + a kind/detail — NEVER the API key or any secret.
package libstatus

import (
	"context"
	"sync"
	"time"

	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

// Snapshot is the JSON-safe view of the librarian's health for the WebUI.
type Snapshot struct {
	Configured bool   `json:"configured"`          // an llm block is present (provider+model)
	Provider   string `json:"provider,omitempty"`  // the configured provider (not a secret)
	Model      string `json:"model,omitempty"`     // the configured model (not a secret)
	Kind       string `json:"kind"`                // "ready"/"model_not_found"/… or "unconfigured" / "unknown"
	Detail     string `json:"detail,omitempty"`    // short, secret-free explanation
	CheckedAt  string `json:"checkedAt,omitempty"` // RFC3339 UTC of the last check, "" if never run
}

// Checker runs and caches the librarian health-check. It is safe for concurrent
// use. When the LLM is not configured, Refresh is a no-op that records an
// "unconfigured" snapshot.
type Checker struct {
	cfg        llm.LLMConfig
	configured bool

	mu   sync.Mutex
	snap Snapshot
}

// New builds a Checker for the given config. The initial snapshot reflects only
// whether the librarian is configured; Kind is "unconfigured" or "unknown"
// (configured-but-not-yet-checked) until Refresh runs.
func New(cfg llm.LLMConfig) *Checker {
	configured := cfg.IsConfigured()
	kind := "unconfigured"
	if configured {
		kind = "unknown"
	}
	return &Checker{
		cfg:        cfg,
		configured: configured,
		snap: Snapshot{
			Configured: configured,
			Provider:   cfg.Provider,
			Model:      cfg.Model,
			Kind:       kind,
		},
	}
}

// Refresh runs one health-check (when configured) and stores the result. It
// returns the new snapshot. It never blocks longer than the context allows.
func (c *Checker) Refresh(ctx context.Context) Snapshot {
	if !c.configured {
		return c.Get()
	}
	res := llm.CheckHealth(ctx, c.cfg)
	snap := Snapshot{
		Configured: true,
		Provider:   c.cfg.Provider,
		Model:      c.cfg.Model,
		Kind:       string(res.Kind),
		Detail:     res.Detail,
		CheckedAt:  time.Now().UTC().Format(time.RFC3339),
	}
	c.mu.Lock()
	c.snap = snap
	c.mu.Unlock()
	return snap
}

// Get returns the last cached snapshot without running a check.
func (c *Checker) Get() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snap
}
