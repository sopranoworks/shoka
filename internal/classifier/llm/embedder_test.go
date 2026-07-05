package llm_test

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sopranoworks/shoka/internal/classifier/llm"
)

func TestEmbedder_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method: %s", r.Method)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}

		var req struct {
			Model string `json:"model"`
			Input string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "test-model" {
			t.Fatalf("unexpected model in request: %s", req.Model)
		}

		resp := map[string]any{
			"model": "test-model",
			"data": []map[string]any{
				{"embedding": []float64{0.1, 0.2, 0.3}, "index": 0},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	e := llm.NewEmbedder(llm.EmbedderConfig{
		BaseURL: server.URL,
		Model:   "test-model",
		APIKey:  "test-key",
	})

	vec, err := e.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatal(err)
	}
	if vec.Model != "test-model" {
		t.Fatalf("expected model 'test-model', got %q", vec.Model)
	}
	if vec.Dimensions != 3 {
		t.Fatalf("expected 3 dimensions, got %d", vec.Dimensions)
	}
	if vec.Values[0] != 0.1 || vec.Values[1] != 0.2 || vec.Values[2] != 0.3 {
		t.Fatalf("unexpected values: %v", vec.Values)
	}
}

func TestEmbedder_ErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer server.Close()

	e := llm.NewEmbedder(llm.EmbedderConfig{
		BaseURL: server.URL,
		Model:   "test-model",
	})

	_, err := e.Embed(context.Background(), "hello")
	if err == nil {
		t.Fatal("expected error on 500 response")
	}
}

func TestEmbedder_NoAPIKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Fatal("expected no auth header when no API key configured")
		}
		resp := map[string]any{
			"model": "m",
			"data":  []map[string]any{{"embedding": []float64{1.0}, "index": 0}},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	e := llm.NewEmbedder(llm.EmbedderConfig{
		BaseURL: server.URL,
		Model:   "m",
	})

	vec, err := e.Embed(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if vec.Dimensions != 1 {
		t.Fatalf("expected 1 dimension, got %d", vec.Dimensions)
	}
}

func TestEmbedder_Integration(t *testing.T) {
	resp, err := http.Get("http://localhost:1234/v1/models")
	if err != nil {
		t.Skip("LM Studio not available")
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skip("LM Studio not available")
	}

	e := llm.NewEmbedder(llm.EmbedderConfig{
		BaseURL: "http://localhost:1234/v1",
		Model:   "text-embedding-nomic-embed-text-v1.5",
	})

	ctx := context.Background()

	vec, err := e.Embed(ctx, "The quick brown fox jumps over the lazy dog")
	if err != nil {
		t.Fatalf("embed failed: %v", err)
	}
	if vec.Dimensions == 0 {
		t.Fatal("expected non-zero dimensions")
	}
	if len(vec.Values) != vec.Dimensions {
		t.Fatalf("values length %d != dimensions %d", len(vec.Values), vec.Dimensions)
	}

	vec1, err := e.Embed(ctx, "A fast dark-colored fox leaps over a sleepy hound")
	if err != nil {
		t.Fatalf("embed similar text failed: %v", err)
	}

	vec2, err := e.Embed(ctx, "Quantum computing uses superposition and entanglement")
	if err != nil {
		t.Fatalf("embed unrelated text failed: %v", err)
	}

	sim1 := cosineSim(vec.Values, vec1.Values)
	sim2 := cosineSim(vec.Values, vec2.Values)

	if sim1 < 0.8 {
		t.Errorf("similar texts cosine similarity %f < 0.8", sim1)
	}
	if sim2 > 0.5 {
		t.Errorf("unrelated texts cosine similarity %f > 0.5", sim2)
	}
}

func cosineSim(a, b []float64) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += a[i] * b[i]
		normA += a[i] * a[i]
		normB += b[i] * b[i]
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	return dot / denom
}
