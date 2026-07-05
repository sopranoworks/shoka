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

	// Classifier fields — populated when the classifier is wired.
	Classifier *ClassifierStatus `json:"classifier,omitempty"`
}

// ClassifierStatus is the JSON-safe view of classifier health for the WebUI.
type ClassifierStatus struct {
	Enabled      bool   `json:"enabled"`
	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
	BaseURL      string `json:"baseUrl,omitempty"`
	DBPath       string `json:"dbPath,omitempty"`
	ProjectCount int    `json:"projectCount"` // projects with vector indices
	Error        string `json:"error,omitempty"`
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
// returns the new snapshot. It never blocks longer than the context allows. The
// config is read under the lock (Apply may swap it concurrently after a reload),
// but the health-check runs WITHOUT the lock so it never spans a round-trip.
func (c *Checker) Refresh(ctx context.Context) Snapshot {
	c.mu.Lock()
	cfg := c.cfg
	configured := c.configured
	c.mu.Unlock()
	if !configured {
		return c.Get()
	}
	res := llm.CheckHealth(ctx, cfg)
	snap := SnapshotFor(cfg, res)
	c.mu.Lock()
	c.snap = snap
	c.mu.Unlock()
	return snap
}

// Apply points the checker at a NEW config and records the health result just
// observed for it — used by the librarian config-reload op, which has already run
// CheckHealth, so the status card reflects the new model without a second call.
// Subsequent Refreshes check the new config. Returns the stored snapshot.
func (c *Checker) Apply(cfg llm.LLMConfig, res llm.HealthResult) Snapshot {
	snap := SnapshotFor(cfg, res)
	c.mu.Lock()
	c.cfg = cfg
	c.configured = cfg.IsConfigured()
	c.snap = snap
	c.mu.Unlock()
	return snap
}

// SnapshotFor builds (without storing) the snapshot for a config + health result:
// provider/model from the config (never the key), kind/detail from the result,
// stamped now. Callers use it to report a reload outcome that should NOT be
// committed (a failed reload keeps the old config but still reports the cause).
func SnapshotFor(cfg llm.LLMConfig, res llm.HealthResult) Snapshot {
	return Snapshot{
		Configured: cfg.IsConfigured(),
		Provider:   cfg.Provider,
		Model:      cfg.Model,
		Kind:       string(res.Kind),
		Detail:     res.Detail,
		CheckedAt:  time.Now().UTC().Format(time.RFC3339),
	}
}

// Get returns the last cached snapshot without running a check.
func (c *Checker) Get() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snap
}
