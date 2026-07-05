package util_test

import (
	"errors"
	"testing"

	"github.com/sopranoworks/shoka/internal/classifier/util"
)

func TestMemoryStore_WriteAndIterate(t *testing.T) {
	store := util.NewMemoryStore("test-model", 3)
	if err := store.Write("a", []float64{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := store.Write("b", []float64{4, 5, 6}); err != nil {
		t.Fatal(err)
	}

	iter := store.Iterator()
	if iter.Model() != "test-model" {
		t.Fatalf("unexpected model: %s", iter.Model())
	}
	if iter.Dimensions() != 3 {
		t.Fatalf("unexpected dimensions: %d", iter.Dimensions())
	}

	kv, err := iter.Read()
	if err != nil {
		t.Fatal(err)
	}
	if kv.Key != "a" {
		t.Fatalf("expected key 'a', got %q", kv.Key)
	}

	kv, err = iter.Read()
	if err != nil {
		t.Fatal(err)
	}
	if kv.Key != "b" {
		t.Fatalf("expected key 'b', got %q", kv.Key)
	}

	_, err = iter.Read()
	if !errors.Is(err, util.ErrIteratorExhausted) {
		t.Fatalf("expected ErrIteratorExhausted, got %v", err)
	}
}

func TestMemoryStore_WriteDimensionMismatch(t *testing.T) {
	store := util.NewMemoryStore("test-model", 3)
	err := store.Write("a", []float64{1, 2})
	if err == nil {
		t.Fatal("expected dimension mismatch error")
	}
}

func TestMemoryStore_WriteModelExposed(t *testing.T) {
	store := util.NewMemoryStore("my-model", 2)
	if store.Model() != "my-model" {
		t.Fatalf("expected model 'my-model', got %q", store.Model())
	}
	if store.Dimensions() != 2 {
		t.Fatalf("expected dimensions 2, got %d", store.Dimensions())
	}
}

func TestMemoryStore_EmptyIterator(t *testing.T) {
	store := util.NewMemoryStore("m", 3)
	iter := store.Iterator()
	_, err := iter.Read()
	if !errors.Is(err, util.ErrIteratorExhausted) {
		t.Fatalf("expected ErrIteratorExhausted on empty store, got %v", err)
	}
}
