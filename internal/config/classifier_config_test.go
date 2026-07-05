package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClassifier_AbsentIsDisabled(t *testing.T) {
	cfg, err := loadYAML(t, llmBaseYAML+`
librarian:
  provider: openai
  model: gpt-4o
`)
	require.NoError(t, err)
	assert.False(t, cfg.Librarian.Classifier.Enabled)
}

func TestClassifier_EnabledLoads(t *testing.T) {
	cfg, err := loadYAML(t, llmBaseYAML+`
librarian:
  provider: openai
  model: gpt-4o
  classifier:
    enabled: true
    embedding_model: text-embedding-nomic-embed-text-v1.5
    embedding_base_url: http://localhost:1234/v1
`)
	require.NoError(t, err)
	assert.True(t, cfg.Librarian.Classifier.Enabled)
	assert.Equal(t, "text-embedding-nomic-embed-text-v1.5", cfg.Librarian.Classifier.EmbeddingModel)
	assert.Equal(t, "http://localhost:1234/v1", cfg.Librarian.Classifier.EmbeddingBaseURL)
}

func TestClassifier_EnabledMissingEmbeddingModel(t *testing.T) {
	_, err := loadYAML(t, llmBaseYAML+`
librarian:
  provider: openai
  model: gpt-4o
  classifier:
    enabled: true
`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "embedding_model")
}

func TestClassifier_ProviderDefaultsToParent(t *testing.T) {
	cfg, err := loadYAML(t, llmBaseYAML+`
librarian:
  provider: openai
  model: gpt-4o
  classifier:
    enabled: true
    embedding_model: text-embedding-3-small
`)
	require.NoError(t, err)
	assert.Equal(t, "openai", cfg.Librarian.Classifier.ResolvedProvider(cfg.Librarian.Provider))
}

func TestClassifier_ExplicitProviderOverridesParent(t *testing.T) {
	cfg, err := loadYAML(t, llmBaseYAML+`
librarian:
  provider: gemini
  model: gemini-2.5-flash
  classifier:
    enabled: true
    provider: openai
    embedding_model: text-embedding-3-small
`)
	require.NoError(t, err)
	assert.Equal(t, "openai", cfg.Librarian.Classifier.ResolvedProvider(cfg.Librarian.Provider))
}

func TestClassifier_AnthropicProviderRejected(t *testing.T) {
	_, err := loadYAML(t, llmBaseYAML+`
librarian:
  provider: anthropic
  model: claude-3-5-haiku-latest
  classifier:
    enabled: true
    embedding_model: some-model
`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support embeddings")
}

func TestClassifier_DisabledSkipsValidation(t *testing.T) {
	cfg, err := loadYAML(t, llmBaseYAML+`
librarian:
  provider: openai
  model: gpt-4o
  classifier:
    enabled: false
`)
	require.NoError(t, err)
	assert.False(t, cfg.Librarian.Classifier.Enabled)
}
