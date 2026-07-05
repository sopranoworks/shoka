package classifier_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/sopranoworks/shoka/pkg/librarian/classifier"
	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

func TestVectorizer_Integration(t *testing.T) {
	resp, err := http.Get("http://localhost:1234/v1/models")
	if err != nil {
		t.Fatalf("LM Studio connection failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("LM Studio returned status %d", resp.StatusCode)
	}

	embedder, err := llm.NewEmbedder(llm.LLMConfig{
		Provider: llm.ProviderOpenAI,
		BaseURL:  "http://localhost:1234/v1",
		Model:    "text-embedding-nomic-embed-text-v1.5",
	})
	if err != nil {
		t.Fatalf("NewEmbedder: %v", err)
	}

	v := classifier.NewVectorizer(embedder)
	ctx := context.Background()

	vec, err := v.Embed(ctx, "The quick brown fox jumps over the lazy dog")
	if err != nil {
		t.Fatalf("Embed failed: %v", err)
	}
	if vec.Dimensions == 0 {
		t.Fatal("expected non-zero dimensions")
	}
	if len(vec.Values) != vec.Dimensions {
		t.Fatalf("values length %d != dimensions %d", len(vec.Values), vec.Dimensions)
	}
	t.Logf("model=%s dimensions=%d", vec.Model, vec.Dimensions)

	vec1, err := v.Embed(ctx, "A fast dark-colored fox leaps over a sleepy hound")
	if err != nil {
		t.Fatalf("Embed similar text failed: %v", err)
	}

	vec2, err := v.Embed(ctx, "Quantum computing uses superposition and entanglement")
	if err != nil {
		t.Fatalf("Embed unrelated text failed: %v", err)
	}

	simSimilar := classifier.CosineSimilarity(vec.Values, vec1.Values)
	simUnrelated := classifier.CosineSimilarity(vec.Values, vec2.Values)

	t.Logf("similar pair cosine=%.4f, unrelated pair cosine=%.4f", simSimilar, simUnrelated)

	if simSimilar < 0.5 {
		t.Errorf("similar texts cosine similarity %f < 0.5", simSimilar)
	}
	if simUnrelated >= simSimilar {
		t.Errorf("unrelated similarity %f >= similar similarity %f", simUnrelated, simSimilar)
	}
}
