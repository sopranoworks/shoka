package main

import (
	"context"

	"github.com/sopranoworks/shoka/internal/config"
	"github.com/sopranoworks/shoka/internal/libstatus"
	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

// clientSwapper is the subset of *librarian.Librarian the reloader drives: it
// swaps the live LLM client. (An interface so the reloader is testable without a
// real Librarian or a cloud call.)
type clientSwapper interface {
	SetClient(llm.Client)
}

// statusApplier is the subset of *libstatus.Checker the reloader drives: it
// commits a new config + the health result just observed for it.
type statusApplier interface {
	Apply(llm.LLMConfig, llm.HealthResult) libstatus.Snapshot
}

// reloadDeps are the reloader's collaborators — real in production (config.Load /
// llm.CheckHealth / llm.NewClient), fakes in tests so no config file or cloud call
// is needed.
type reloadDeps struct {
	loadConfig  func(string) (*config.Config, error)
	checkHealth func(context.Context, llm.LLMConfig) llm.HealthResult
	newClient   func(llm.LLMConfig) (llm.Client, error)
}

// newLibrarianReloader builds the B-73 Option-2 reload action: an llm-block-only,
// manual config reload. It re-reads the config FILE, takes only the llm block,
// runs the one-call connection test, and on a ready outcome swaps the live LLM
// client (the model is captured at construction, so this installs a NEW client)
// and commits the new config to the status checker. On ANY failure — a config
// load/validation error, no llm block, a non-ready health kind, or a client-build
// error — the OLD client is kept and the typed cause is reported; nothing is
// applied. Shoka NEVER writes config: persistence is the operator's own edit to
// the authoritative YAML, which this re-reads.
func newLibrarianReloader(configPath string, lib clientSwapper, checker statusApplier, deps reloadDeps) func(context.Context) libstatus.Snapshot {
	return func(ctx context.Context) libstatus.Snapshot {
		cfg, err := deps.loadConfig(configPath)
		if err != nil {
			// The whole file is re-validated on load; a foreign error anywhere fails
			// the reload cleanly (nothing changes).
			return libstatus.SnapshotFor(llm.LLMConfig{}, llm.HealthResult{
				Kind:   llm.HealthMisconfigured,
				Detail: "config reload failed: " + err.Error(),
			})
		}
		lc := llmConfig(cfg)
		if !lc.IsConfigured() {
			return libstatus.SnapshotFor(lc, llm.HealthResult{
				Kind:   llm.HealthMisconfigured,
				Detail: "the reloaded config has no librarian (llm provider+model) block",
			})
		}
		res := deps.checkHealth(ctx, lc)
		if res.Kind != llm.HealthReady {
			// Keep the OLD client; report the attempted config + the typed cause.
			return libstatus.SnapshotFor(lc, res)
		}
		newClient, err := deps.newClient(lc)
		if err != nil {
			return libstatus.SnapshotFor(lc, llm.HealthResult{
				Kind:   llm.HealthMisconfigured,
				Detail: "cannot build LLM client: " + err.Error(),
			})
		}
		lib.SetClient(newClient)
		return checker.Apply(lc, res)
	}
}
