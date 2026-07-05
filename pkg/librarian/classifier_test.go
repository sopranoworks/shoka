package librarian_test

import (
	"context"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/sopranoworks/shoka/pkg/librarian"
	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

type stubEmbedder struct {
	model string
	dims  int
	calls int
}

func (s *stubEmbedder) Embed(_ context.Context, text string) (*llm.EmbeddingVector, error) {
	s.calls++
	vec := make([]float64, s.dims)
	for i := range vec {
		if i < len(text) {
			vec[i] = float64(text[i]) / 255.0
		}
	}
	return &llm.EmbeddingVector{
		Model:      s.model,
		Dimensions: s.dims,
		Values:     vec,
	}, nil
}

func TestLibrarian_ClassifierNil(t *testing.T) {
	lib := librarian.New(nil, 0)
	if lib.Classifier() != nil {
		t.Fatal("expected nil classifier when not configured")
	}
}

func TestLibrarian_ClassifierAttached(t *testing.T) {
	db := openTestDB(t)
	embedder := &stubEmbedder{model: "test", dims: 4}
	cl := librarian.NewClassifier(embedder, db, "test")

	lib := librarian.New(nil, 0)
	lib.WithClassifier(cl)

	if lib.Classifier() == nil {
		t.Fatal("expected non-nil classifier")
	}
}

func TestClassifier_StoreAndFindSimilar(t *testing.T) {
	db := openTestDB(t)
	embedder := &stubEmbedder{model: "test", dims: 8}
	cl := librarian.NewClassifier(embedder, db, "test")

	ctx := context.Background()

	if err := cl.Store(ctx, "hello", "hello"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := cl.Store(ctx, "help", "help"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if err := cl.Store(ctx, "xyz", "xyz!!"); err != nil {
		t.Fatalf("Store: %v", err)
	}

	results, err := cl.FindSimilar(ctx, "hello", 2)
	if err != nil {
		t.Fatalf("FindSimilar: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Key != "hello" {
		t.Fatalf("expected top result 'hello', got %q", results[0].Key)
	}
}

func TestClassifier_Embed(t *testing.T) {
	db := openTestDB(t)
	embedder := &stubEmbedder{model: "test", dims: 4}
	cl := librarian.NewClassifier(embedder, db, "test")

	vec, err := cl.Embed(context.Background(), "test text")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if vec.Model != "test" || vec.Dimensions != 4 {
		t.Fatalf("unexpected vector: model=%q dims=%d", vec.Model, vec.Dimensions)
	}
}

func openTestDB(t *testing.T) *bolt.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}
