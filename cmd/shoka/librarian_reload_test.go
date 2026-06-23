package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sopranoworks/shoka/internal/config"
	"github.com/sopranoworks/shoka/internal/libstatus"
	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

// --- fakes (no real config file, no cloud call) ---

type fakeSwapper struct {
	set    int
	client llm.Client
}

func (f *fakeSwapper) SetClient(c llm.Client) { f.set++; f.client = c }

type fakeApplier struct {
	applied int
	cfg     llm.LLMConfig
}

func (f *fakeApplier) Apply(cfg llm.LLMConfig, res llm.HealthResult) libstatus.Snapshot {
	f.applied++
	f.cfg = cfg
	return libstatus.SnapshotFor(cfg, res)
}

type nopClient struct{}

func (nopClient) CreateMessage(context.Context, llm.CreateMessageParams) (llm.Message, error) {
	return llm.Message{}, nil
}

func cfgWith(provider, model string) *config.Config {
	c := &config.Config{}
	c.Librarian = config.LLMConfig{Provider: provider, Model: model}
	return c
}

// A ready reload swaps the live client and commits the new config to the checker.
func TestReloader_GoodModel_Swaps(t *testing.T) {
	swapper := &fakeSwapper{}
	applier := &fakeApplier{}
	newC := nopClient{}
	reload := newLibrarianReloader("ignored.yaml", swapper, applier, reloadDeps{
		loadConfig:  func(string) (*config.Config, error) { return cfgWith("openai", "gpt-new"), nil },
		checkHealth: func(context.Context, llm.LLMConfig) llm.HealthResult { return llm.HealthResult{Kind: llm.HealthReady} },
		newClient:   func(llm.LLMConfig) (llm.Client, error) { return newC, nil },
	})

	snap := reload(context.Background())
	if swapper.set != 1 || swapper.client != newC {
		t.Errorf("expected the live client swapped once to the new client; set=%d", swapper.set)
	}
	if applier.applied != 1 || applier.cfg.Model != "gpt-new" {
		t.Errorf("expected the checker to commit the new config; applied=%d model=%q", applier.applied, applier.cfg.Model)
	}
	if snap.Kind != string(llm.HealthReady) || snap.Model != "gpt-new" {
		t.Errorf("snapshot = %+v, want ready with model gpt-new", snap)
	}
}

// A non-ready reload (e.g. the new model name is also wrong) keeps the OLD client
// and reports the typed detail — applies nothing.
func TestReloader_BadModel_KeepsOld(t *testing.T) {
	swapper := &fakeSwapper{}
	applier := &fakeApplier{}
	reload := newLibrarianReloader("ignored.yaml", swapper, applier, reloadDeps{
		loadConfig: func(string) (*config.Config, error) { return cfgWith("openai", "gpt-typo"), nil },
		checkHealth: func(context.Context, llm.LLMConfig) llm.HealthResult {
			return llm.HealthResult{Kind: llm.HealthModelNotFound, Detail: `the model "gpt-typo" does not exist`}
		},
		newClient: func(llm.LLMConfig) (llm.Client, error) {
			t.Fatal("newClient must not be called on a non-ready reload")
			return nil, nil
		},
	})

	snap := reload(context.Background())
	if swapper.set != 0 || applier.applied != 0 {
		t.Errorf("a failed reload must apply NOTHING: swaps=%d applies=%d", swapper.set, applier.applied)
	}
	if snap.Kind != string(llm.HealthModelNotFound) || !strings.Contains(snap.Detail, "gpt-typo") {
		t.Errorf("snapshot = %+v, want model_not_found with the typed detail", snap)
	}
	if snap.Model != "gpt-typo" {
		t.Errorf("snapshot model = %q, want the attempted model surfaced", snap.Model)
	}
}

// A config load/validation error fails the reload cleanly — nothing applied, the
// cause reported.
func TestReloader_LoadError_KeepsOld(t *testing.T) {
	swapper := &fakeSwapper{}
	applier := &fakeApplier{}
	reload := newLibrarianReloader("ignored.yaml", swapper, applier, reloadDeps{
		loadConfig: func(string) (*config.Config, error) {
			return nil, errors.New("invalid configuration: bad key at line 9")
		},
		checkHealth: func(context.Context, llm.LLMConfig) llm.HealthResult {
			t.Fatal("checkHealth must not run on a load error")
			return llm.HealthResult{}
		},
		newClient: func(llm.LLMConfig) (llm.Client, error) { return nil, nil },
	})

	snap := reload(context.Background())
	if swapper.set != 0 || applier.applied != 0 {
		t.Errorf("a load error must apply nothing: swaps=%d applies=%d", swapper.set, applier.applied)
	}
	if snap.Kind != string(llm.HealthMisconfigured) || !strings.Contains(snap.Detail, "config reload failed") {
		t.Errorf("snapshot = %+v, want misconfigured naming the load failure", snap)
	}
}

// A reloaded file with no librarian block reports that, applies nothing.
func TestReloader_NotConfigured_KeepsOld(t *testing.T) {
	swapper := &fakeSwapper{}
	applier := &fakeApplier{}
	reload := newLibrarianReloader("ignored.yaml", swapper, applier, reloadDeps{
		loadConfig: func(string) (*config.Config, error) { return &config.Config{}, nil }, // empty llm block
		checkHealth: func(context.Context, llm.LLMConfig) llm.HealthResult {
			t.Fatal("checkHealth must not run when unconfigured")
			return llm.HealthResult{}
		},
		newClient: func(llm.LLMConfig) (llm.Client, error) { return nil, nil },
	})

	snap := reload(context.Background())
	if swapper.set != 0 || applier.applied != 0 {
		t.Errorf("an unconfigured reload must apply nothing: swaps=%d applies=%d", swapper.set, applier.applied)
	}
	if snap.Kind != string(llm.HealthMisconfigured) || !strings.Contains(snap.Detail, "no librarian") {
		t.Errorf("snapshot = %+v, want misconfigured naming the missing llm block", snap)
	}
}
