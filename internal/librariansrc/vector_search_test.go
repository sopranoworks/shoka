package librariansrc

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sopranoworks/shoka/internal/storage"
	"github.com/sopranoworks/shoka/pkg/librarian/llm"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeVectorSearcher returns pre-configured results for testing.
type fakeVectorSearcher struct {
	results []storage.VectorSearchResult
	err     error
	called  bool
}

func (f *fakeVectorSearcher) VectorSearch(_ context.Context, _, _, _ string, _ int) ([]storage.VectorSearchResult, error) {
	f.called = true
	return f.results, f.err
}

func TestSearch_WithoutVector_FulltextOnly(t *testing.T) {
	s := newStore(t)
	project(t, s, "ns", "proj")
	write(t, s, "ns", "proj", "readme.md", "hello world from the readme file")
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL drain")
	}

	corpus := NewCorpus(s, "ns", "proj")
	// No vector searcher attached

	hits, err := corpus.Search(context.Background(), "hello world", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "readme.md", hits[0].Path)
}

func TestSearch_WithVector_BothRun_ResultsMerged(t *testing.T) {
	s := newStore(t)
	project(t, s, "ns", "proj")
	write(t, s, "ns", "proj", "a.md", "hello world exact match")
	write(t, s, "ns", "proj", "b.md", "totally different content no keyword overlap")
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL drain")
	}

	vec := &fakeVectorSearcher{
		results: []storage.VectorSearchResult{
			{Path: "b.md", Similarity: 0.85},
			{Path: "a.md", Similarity: 0.70},
		},
	}

	corpus := NewCorpus(s, "ns", "proj").WithVectorSearch(vec)
	hits, err := corpus.Search(context.Background(), "hello world", 10)
	require.NoError(t, err)
	assert.True(t, vec.called)

	// a.md found by fulltext (exact match), b.md found by vector only
	paths := make([]string, len(hits))
	for i, h := range hits {
		paths[i] = h.Path
	}
	assert.Contains(t, paths, "a.md")
	assert.Contains(t, paths, "b.md")
}

func TestSearch_Dedup_SameFileFromBoth(t *testing.T) {
	s := newStore(t)
	project(t, s, "ns", "proj")
	write(t, s, "ns", "proj", "doc.md", "important information about widgets")
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL drain")
	}

	vec := &fakeVectorSearcher{
		results: []storage.VectorSearchResult{
			{Path: "doc.md", Similarity: 0.95},
		},
	}

	corpus := NewCorpus(s, "ns", "proj").WithVectorSearch(vec)
	hits, err := corpus.Search(context.Background(), "important information", 10)
	require.NoError(t, err)

	// doc.md appears once, not twice
	count := 0
	for _, h := range hits {
		if h.Path == "doc.md" {
			count++
		}
	}
	assert.Equal(t, 1, count, "doc.md should appear exactly once (deduped)")
}

func TestSearch_VectorFailure_FallsBackToFulltext(t *testing.T) {
	s := newStore(t)
	project(t, s, "ns", "proj")
	write(t, s, "ns", "proj", "x.md", "findable content here")
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL drain")
	}

	vec := &fakeVectorSearcher{
		err: errors.New("embed API timeout"),
	}

	corpus := NewCorpus(s, "ns", "proj").WithVectorSearch(vec)
	hits, err := corpus.Search(context.Background(), "findable content", 10)
	require.NoError(t, err, "vector failure must not propagate")
	require.Len(t, hits, 1)
	assert.Equal(t, "x.md", hits[0].Path)
}

func TestSearch_ResultLimit(t *testing.T) {
	s := newStore(t)
	project(t, s, "ns", "proj")
	for i := 0; i < 5; i++ {
		write(t, s, "ns", "proj", fmt.Sprintf("file%d.md", i), "common keyword")
	}
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL drain")
	}

	vec := &fakeVectorSearcher{
		results: []storage.VectorSearchResult{
			{Path: "extra1.md", Similarity: 0.8},
			{Path: "extra2.md", Similarity: 0.7},
		},
	}

	corpus := NewCorpus(s, "ns", "proj").WithVectorSearch(vec)
	hits, err := corpus.Search(context.Background(), "common keyword", 3)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(hits), 3, "should not exceed limit")
}

// TestSearch_SemanticMatch_Integration tests with a real embedding service.
// Requires LM Studio running at localhost:1234 with an embedding model.
// Skipped when unavailable.
func TestSearch_SemanticMatch_Integration(t *testing.T) {
	embedCfg := llm.LLMConfig{
		Provider: "openai",
		BaseURL:  "http://localhost:1234/v1",
		Model:    "text-embedding-nomic-embed-text-v1.5",
	}
	embedder, err := llm.NewEmbedder(embedCfg)
	if err != nil {
		t.Skipf("cannot create embedder: %v", err)
	}

	// Test connectivity
	ctx := context.Background()
	_, embedErr := embedder.Embed(ctx, "test")
	if embedErr != nil {
		t.Skipf("LM Studio not available at localhost:1234: %v", embedErr)
	}

	// Set up storage with vector config
	s := newStore(t)
	project(t, s, "ns", "proj")

	// Write documents with specific semantic content
	write(t, s, "ns", "proj", "cooking.md", "Recipe for chocolate cake: mix flour, sugar, cocoa, eggs, and butter. Bake at 350F for 30 minutes.")
	write(t, s, "ns", "proj", "programming.md", "Go's goroutines provide lightweight concurrency. Use channels for communication between goroutines.")
	write(t, s, "ns", "proj", "gardening.md", "Plant tomatoes in spring after the last frost. Water regularly and provide full sunlight.")
	if !s.WaitForWAL(10 * time.Second) {
		t.Fatal("WAL drain")
	}

	// Configure vector index and embed all files
	s.SetVectorConfig(&storage.VectorIndexConfig{
		Embedder: embedder,
		Model:    "text-embedding-nomic-embed-text-v1.5",
	})
	s.StartVectorWorker(ctx, 0)

	// Write again to trigger vectorization (the first writes happened before config)
	write(t, s, "ns", "proj", "cooking.md", "Recipe for chocolate cake: mix flour, sugar, cocoa, eggs, and butter. Bake at 350F for 30 minutes.")
	write(t, s, "ns", "proj", "programming.md", "Go's goroutines provide lightweight concurrency. Use channels for communication between goroutines.")
	write(t, s, "ns", "proj", "gardening.md", "Plant tomatoes in spring after the last frost. Water regularly and provide full sunlight.")

	// Wait for embeddings to complete
	deadline := time.After(30 * time.Second)
	for {
		_, _, embedded, _, _, _ := s.VectorCounters()
		if embedded >= 3 {
			break
		}
		select {
		case <-deadline:
			_, _, emb, fail, _, _ := s.VectorCounters()
			t.Fatalf("timeout waiting for embeddings: embedded=%d failed=%d", emb, fail)
		default:
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Now search with semantic query (different words than document content)
	corpus := NewCorpus(s, "ns", "proj").WithVectorSearch(s)

	// "How to bake a dessert" should find cooking.md even though "dessert" and
	// "bake" (as used here) don't appear as exact substrings in the same phrase
	hits, err := corpus.Search(ctx, "how to bake a dessert", 10)
	require.NoError(t, err)

	// Vector search should find cooking.md as semantically relevant
	foundCooking := false
	for _, h := range hits {
		if h.Path == "cooking.md" {
			foundCooking = true
			break
		}
	}
	assert.True(t, foundCooking, "semantic search should find cooking.md for dessert query; hits: %+v", hits)
}

