package main

import (
	"context"

	"github.com/sopranoworks/shoka/internal/libstatus"
	"github.com/sopranoworks/shoka/internal/uisettings"
	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

// newLibrarianBaseURLSetter builds the WebUI action that changes the librarian's
// base URL at runtime: persist to the settings store, build a new LLM client
// with the updated URL, health-check, and swap on success.
func newLibrarianBaseURLSetter(
	configPath string,
	lib clientSwapper,
	checker statusApplier,
	store *uisettings.Store,
	deps reloadDeps,
) func(context.Context, string) libstatus.Snapshot {
	return func(ctx context.Context, baseURL string) libstatus.Snapshot {
		if err := store.SetBaseURL(baseURL); err != nil {
			return libstatus.SnapshotFor(llm.LLMConfig{}, llm.HealthResult{
				Kind:   llm.HealthMisconfigured,
				Detail: "failed to persist base URL: " + err.Error(),
			})
		}

		cfg, err := deps.loadConfig(configPath)
		if err != nil {
			return libstatus.SnapshotFor(llm.LLMConfig{}, llm.HealthResult{
				Kind:   llm.HealthMisconfigured,
				Detail: "config load failed: " + err.Error(),
			})
		}
		lc := llmConfig(cfg)
		if !lc.IsConfigured() {
			return libstatus.SnapshotFor(lc, llm.HealthResult{
				Kind:   llm.HealthMisconfigured,
				Detail: "the config has no librarian (llm provider+model) block",
			})
		}
		lc.BaseURL = baseURL

		// Apply persisted max_steps so it isn't lost.
		s := store.Get()
		if s.MaxSteps != nil {
			lc.MaxSteps = *s.MaxSteps
		}

		res := deps.checkHealth(ctx, lc)
		if res.Kind != llm.HealthReady {
			return checker.Apply(lc, res)
		}
		newClient, err := deps.newClient(lc)
		if err != nil {
			return libstatus.SnapshotFor(lc, llm.HealthResult{
				Kind:   llm.HealthMisconfigured,
				Detail: "cannot build LLM client: " + err.Error(),
			})
		}
		lib.SetClient(newClient)
		lib.SetMaxSteps(lc.MaxSteps)
		return checker.Apply(lc, res)
	}
}
