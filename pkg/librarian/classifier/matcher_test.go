package classifier_test

import (
	"context"
	"math"
	"testing"

	"github.com/sopranoworks/shoka/pkg/librarian/classifier"
	"github.com/sopranoworks/shoka/pkg/librarian/classifier/util"
	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

func TestCosineSimilarity_Identical(t *testing.T) {
	a := []float64{1, 2, 3}
	sim := classifier.CosineSimilarity(a, a)
	if math.Abs(sim-1.0) > 1e-9 {
		t.Fatalf("expected 1.0, got %f", sim)
	}
}

func TestCosineSimilarity_Orthogonal(t *testing.T) {
	a := []float64{1, 0, 0}
	b := []float64{0, 1, 0}
	sim := classifier.CosineSimilarity(a, b)
	if math.Abs(sim) > 1e-9 {
		t.Fatalf("expected 0.0, got %f", sim)
	}
}

func TestCosineSimilarity_Opposite(t *testing.T) {
	a := []float64{1, 2, 3}
	b := []float64{-1, -2, -3}
	sim := classifier.CosineSimilarity(a, b)
	if math.Abs(sim+1.0) > 1e-9 {
		t.Fatalf("expected -1.0, got %f", sim)
	}
}

func TestCosineSimilarity_KnownVectors(t *testing.T) {
	a := []float64{1, 0}
	b := []float64{1, 1}
	sim := classifier.CosineSimilarity(a, b)
	expected := 1.0 / math.Sqrt(2)
	if math.Abs(sim-expected) > 1e-9 {
		t.Fatalf("expected %f, got %f", expected, sim)
	}
}

func TestMatcher_FindTopN(t *testing.T) {
	store := util.NewMemoryStore("test-model", 3)
	store.Write("a", []float64{1, 0, 0})
	store.Write("b", []float64{0.9, 0.1, 0})
	store.Write("c", []float64{0, 1, 0})
	store.Write("d", []float64{0, 0, 1})

	target := &llm.EmbeddingVector{
		Model:      "test-model",
		Dimensions: 3,
		Values:     []float64{1, 0, 0},
	}

	m := &classifier.Matcher{}
	results, err := m.FindTopN(context.Background(), target, store.Iterator(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Key != "a" {
		t.Fatalf("expected top result key 'a', got %q", results[0].Key)
	}
	if results[1].Key != "b" {
		t.Fatalf("expected second result key 'b', got %q", results[1].Key)
	}
	if math.Abs(results[0].Similarity-1.0) > 1e-9 {
		t.Fatalf("expected similarity 1.0 for 'a', got %f", results[0].Similarity)
	}
}

func TestMatcher_DimensionMismatch(t *testing.T) {
	store := util.NewMemoryStore("test-model", 3)
	store.Write("a", []float64{1, 0, 0})

	target := &llm.EmbeddingVector{
		Model:      "test-model",
		Dimensions: 2,
		Values:     []float64{1, 0},
	}

	m := &classifier.Matcher{}
	_, err := m.FindTopN(context.Background(), target, store.Iterator(), 1)
	if err == nil {
		t.Fatal("expected dimension mismatch error")
	}
}

func TestMatcher_ModelMismatch(t *testing.T) {
	store := util.NewMemoryStore("model-a", 3)
	store.Write("a", []float64{1, 0, 0})

	target := &llm.EmbeddingVector{
		Model:      "model-b",
		Dimensions: 3,
		Values:     []float64{1, 0, 0},
	}

	m := &classifier.Matcher{}
	_, err := m.FindTopN(context.Background(), target, store.Iterator(), 1)
	if err == nil {
		t.Fatal("expected model mismatch error")
	}
}
