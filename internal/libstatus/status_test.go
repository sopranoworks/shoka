package libstatus

import (
	"context"
	"sync"
	"testing"

	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

func TestNew_States(t *testing.T) {
	// Unconfigured: no provider/model.
	u := New(llm.LLMConfig{})
	if s := u.Get(); s.Configured || s.Kind != "unconfigured" {
		t.Errorf("unconfigured snapshot = %+v, want {Configured:false, Kind:unconfigured}", s)
	}
	// Configured but not yet checked.
	c := New(llm.LLMConfig{Provider: "anthropic", Model: "claude-x"})
	if s := c.Get(); !s.Configured || s.Kind != "unknown" || s.Provider != "anthropic" || s.Model != "claude-x" {
		t.Errorf("configured snapshot = %+v, want {Configured:true, Kind:unknown, provider/model set}", s)
	}
}

func TestRefresh_UnconfiguredIsNoNetwork(t *testing.T) {
	// Refresh on an unconfigured checker must NOT make a call; it returns the
	// cached unconfigured snapshot.
	u := New(llm.LLMConfig{})
	got := u.Refresh(context.Background())
	if got.Configured || got.Kind != "unconfigured" {
		t.Errorf("Refresh(unconfigured) = %+v, want unconfigured", got)
	}
}

// Apply commits a new config + the just-observed health result: the cached
// snapshot reflects the new provider/model/kind/detail, and a later Refresh on an
// unconfigured Apply makes no call.
func TestApply_CommitsNewConfig(t *testing.T) {
	c := New(llm.LLMConfig{Provider: "anthropic", Model: "claude-old"})

	snap := c.Apply(
		llm.LLMConfig{Provider: "gemini", Model: "gemini-2.5-flash"},
		llm.HealthResult{Kind: llm.HealthReady},
	)
	if snap.Provider != "gemini" || snap.Model != "gemini-2.5-flash" || snap.Kind != string(llm.HealthReady) {
		t.Errorf("Apply snapshot = %+v, want gemini/gemini-2.5-flash/ready", snap)
	}
	// Get reflects the applied config.
	if got := c.Get(); got.Model != "gemini-2.5-flash" || got.Kind != string(llm.HealthReady) {
		t.Errorf("Get after Apply = %+v, want the committed config", got)
	}
	// Applying an unconfigured config flips configured off, so Refresh is a no-op.
	c.Apply(llm.LLMConfig{}, llm.HealthResult{Kind: llm.HealthMisconfigured, Detail: "x"})
	if got := c.Refresh(context.Background()); got.Configured {
		t.Errorf("Refresh after unconfigured Apply = %+v, want not configured (no call)", got)
	}
}

// SnapshotFor is a pure builder (no storing): provider/model from the config,
// kind/detail from the result, and it never carries a key.
func TestSnapshotFor(t *testing.T) {
	s := SnapshotFor(
		llm.LLMConfig{Provider: "openai", Model: "gpt-x"},
		llm.HealthResult{Kind: llm.HealthAuthFailed, Detail: "OPENAI_API_KEY is empty or unset"},
	)
	if !s.Configured || s.Provider != "openai" || s.Model != "gpt-x" {
		t.Errorf("snapshot = %+v, want configured openai/gpt-x", s)
	}
	if s.Kind != string(llm.HealthAuthFailed) || s.Detail == "" || s.CheckedAt == "" {
		t.Errorf("snapshot = %+v, want auth_failed with detail + checkedAt", s)
	}
}

// Concurrent Apply/Refresh/Get must be race-free (Refresh reads the config under
// the lock now that Apply can swap it). Under -race this fails on any unguarded
// access.
func TestChecker_ConcurrentApplyRefreshGet(t *testing.T) {
	c := New(llm.LLMConfig{}) // unconfigured ⇒ Refresh makes no network call
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Apply(llm.LLMConfig{}, llm.HealthResult{Kind: llm.HealthMisconfigured, Detail: "x"})
			}
		}()
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.Refresh(context.Background())
				_ = c.Get()
			}
		}()
	}
	wg.Wait()
}
