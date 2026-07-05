package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sopranoworks/shoka/internal/classifier/core"
)

type EmbedderConfig struct {
	BaseURL string
	Model   string
	APIKey  string
}

type Embedder struct {
	config EmbedderConfig
	client *http.Client
}

func NewEmbedder(cfg EmbedderConfig) *Embedder {
	return &Embedder{
		config: cfg,
		client: &http.Client{},
	}
}

type embeddingRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
}

type embeddingResponse struct {
	Data  []embeddingData `json:"data"`
	Model string          `json:"model"`
}

type embeddingData struct {
	Embedding []float64 `json:"embedding"`
	Index     int       `json:"index"`
}

func (e *Embedder) Embed(ctx context.Context, text string) (*core.Vector, error) {
	reqBody := embeddingRequest{
		Model: e.config.Model,
		Input: text,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	url := strings.TrimRight(e.config.BaseURL, "/") + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if e.config.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.config.APIKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embedding API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if len(result.Data) == 0 {
		return nil, fmt.Errorf("embedding response contained no data")
	}

	embedding := result.Data[0].Embedding
	return &core.Vector{
		Model:      result.Model,
		Dimensions: len(embedding),
		Values:     embedding,
	}, nil
}
