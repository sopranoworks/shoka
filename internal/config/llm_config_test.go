package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// llmBaseYAML is the minimum a config needs to pass Validate, so the llm
// tests below exercise only the llm block.
const llmBaseYAML = `
server:
  http:
    listen: ":8080"
  mcp:
    plain:
      listen: ":8081"
storage:
  base_dir: "/tmp/shoka"
`

// TestLLM_APIKeyLineRejected: the api_key field was removed, so an `api_key:`
// line in the llm block is now a strict-decode error — the guardrail telling the
// operator the secret belongs in the environment, not the config.
func TestLLM_APIKeyLineRejected(t *testing.T) {
	_, err := loadYAML(t, llmBaseYAML+`
llm:
  provider: anthropic
  model: claude-3-5-haiku-latest
  api_key: "sk-should-not-be-here"
`)
	require.Error(t, err, "an api_key line must fail strict decode")
	assert.Contains(t, err.Error(), "api_key", "the error must name the offending key")
}

// TestLLM_ConfiguredLoads: a valid llm block (provider + model, no key, no
// base_url) loads cleanly and is configured.
func TestLLM_ConfiguredLoads(t *testing.T) {
	cfg, err := loadYAML(t, llmBaseYAML+`
llm:
  provider: openai
  model: gpt-4o
`)
	require.NoError(t, err)
	assert.True(t, cfg.LLM.IsConfigured())
	assert.Equal(t, "openai", cfg.LLM.Provider)
	assert.Equal(t, 8, cfg.LLM.MaxSteps, "max_steps defaults to 8 when configured")
}

// TestLLM_ModelRequired: provider set but model empty is a config error.
func TestLLM_ModelRequired(t *testing.T) {
	_, err := loadYAML(t, llmBaseYAML+`
llm:
  provider: anthropic
`)
	require.Error(t, err, "provider set without a model must fail validation")
	assert.Contains(t, err.Error(), "model")
}

// TestLLM_UnknownProvider: an unknown provider is a config error.
func TestLLM_UnknownProvider(t *testing.T) {
	_, err := loadYAML(t, llmBaseYAML+`
llm:
  provider: gemini
  model: gemini-1.5
`)
	require.Error(t, err, "an unknown provider must fail validation")
	assert.Contains(t, err.Error(), "provider")
}

// TestLLM_AbsentIsNotConfigured: no llm block ⇒ not configured, loads fine.
func TestLLM_AbsentIsNotConfigured(t *testing.T) {
	cfg, err := loadYAML(t, llmBaseYAML)
	require.NoError(t, err)
	assert.False(t, cfg.LLM.IsConfigured())
}
