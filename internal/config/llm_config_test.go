package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// llmBaseYAML is the minimum a config needs to pass Validate, so the librarian
// tests below exercise only the librarian block.
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
// line in the librarian block is now a strict-decode error — the guardrail telling the
// operator the secret belongs in the environment, not the config.
func TestLLM_APIKeyLineRejected(t *testing.T) {
	_, err := loadYAML(t, llmBaseYAML+`
librarian:
  provider: anthropic
  model: claude-3-5-haiku-latest
  api_key: "sk-should-not-be-here"
`)
	require.Error(t, err, "an api_key line must fail strict decode")
	assert.Contains(t, err.Error(), "api_key", "the error must name the offending key")
}

// TestLLM_ConfiguredLoads: a valid librarian block (provider + model, no key, no
// base_url) loads cleanly and is configured.
func TestLLM_ConfiguredLoads(t *testing.T) {
	cfg, err := loadYAML(t, llmBaseYAML+`
librarian:
  provider: openai
  model: gpt-4o
`)
	require.NoError(t, err)
	assert.True(t, cfg.Librarian.IsConfigured())
	assert.Equal(t, "openai", cfg.Librarian.Provider)
	assert.Equal(t, 8, cfg.Librarian.MaxSteps, "max_steps defaults to 8 when configured")
}

// TestLLM_ModelRequired: provider set but model empty is a config error.
func TestLLM_ModelRequired(t *testing.T) {
	_, err := loadYAML(t, llmBaseYAML+`
librarian:
  provider: anthropic
`)
	require.Error(t, err, "provider set without a model must fail validation")
	assert.Contains(t, err.Error(), "model")
}

// TestLLM_UnknownProvider: an unknown provider is a config error.
func TestLLM_UnknownProvider(t *testing.T) {
	_, err := loadYAML(t, llmBaseYAML+`
librarian:
  provider: cohere
  model: command-r
`)
	require.Error(t, err, "an unknown provider must fail validation")
	assert.Contains(t, err.Error(), "provider")
}

// TestLLM_GeminiProviderLoads: gemini is a known provider and validates.
func TestLLM_GeminiProviderLoads(t *testing.T) {
	cfg, err := loadYAML(t, llmBaseYAML+`
librarian:
  provider: gemini
  model: gemini-2.5-flash
`)
	require.NoError(t, err, "gemini provider + model must validate")
	assert.True(t, cfg.Librarian.IsConfigured())
	assert.Equal(t, "gemini", cfg.Librarian.Provider)
}

// TestLLM_AbsentIsNotConfigured: no librarian block ⇒ not configured, loads fine.
func TestLLM_AbsentIsNotConfigured(t *testing.T) {
	cfg, err := loadYAML(t, llmBaseYAML)
	require.NoError(t, err)
	assert.False(t, cfg.Librarian.IsConfigured())
}
