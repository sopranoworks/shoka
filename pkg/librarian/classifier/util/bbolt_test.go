package util_test

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"

	"github.com/sopranoworks/shoka/pkg/librarian/classifier"
	"github.com/sopranoworks/shoka/pkg/librarian/classifier/util"
	"github.com/sopranoworks/shoka/pkg/librarian/llm"
)

func tempDB(t *testing.T) (*bolt.DB, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

// 1. Write N vectors → iterate → all N returned in order
func TestBbolt_WriteAndIterate(t *testing.T) {
	db, _ := tempDB(t)
	w := util.NewBboltWriter(db, "test-model", 3)
	keys := []string{"alpha", "beta", "gamma"}
	for _, k := range keys {
		if err := w.Write(k, []float64{1, 2, 3}); err != nil {
			t.Fatal(err)
		}
	}

	iter := util.NewBboltIterator(db, "test-model", 3)
	var got []string
	for {
		kv, err := iter.Read()
		if err == util.ErrIteratorExhausted {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, kv.Key)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 vectors, got %d", len(got))
	}
	// bbolt stores keys in sorted order
	expected := []string{"alpha", "beta", "gamma"}
	for i, k := range expected {
		if got[i] != k {
			t.Fatalf("position %d: expected %q, got %q", i, k, got[i])
		}
	}
}

// 2. Write → close DB → reopen → iterate → data persists
func TestBbolt_Persistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "persist.db")

	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := util.NewBboltWriter(db, "m", 2)
	w.Write("k1", []float64{1.5, 2.5})
	w.Write("k2", []float64{3.5, 4.5})
	db.Close()

	db2, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	iter := util.NewBboltIterator(db2, "m", 2)
	kv, err := iter.Read()
	if err != nil {
		t.Fatal(err)
	}
	if kv.Key != "k1" || kv.Vector[0] != 1.5 || kv.Vector[1] != 2.5 {
		t.Fatalf("unexpected first entry: %+v", kv)
	}
	kv, err = iter.Read()
	if err != nil {
		t.Fatal(err)
	}
	if kv.Key != "k2" {
		t.Fatalf("expected k2, got %q", kv.Key)
	}
}

// 3. Write with wrong dimensions → error
func TestBbolt_WriteDimensionMismatch(t *testing.T) {
	db, _ := tempDB(t)
	w := util.NewBboltWriter(db, "m", 3)
	err := w.Write("k", []float64{1, 2})
	if err == nil {
		t.Fatal("expected dimension mismatch error")
	}
}

// 4. Iterate empty bucket → immediate EOF
func TestBbolt_IterateEmpty(t *testing.T) {
	db, _ := tempDB(t)
	iter := util.NewBboltIterator(db, "m", 3)
	_, err := iter.Read()
	if err != util.ErrIteratorExhausted {
		t.Fatalf("expected ErrIteratorExhausted, got %v", err)
	}
}

// 5. Multiple datasets (different model/dimensions) → isolated
func TestBbolt_Isolation(t *testing.T) {
	db, _ := tempDB(t)

	w1 := util.NewBboltWriter(db, "model-a", 2)
	w2 := util.NewBboltWriter(db, "model-b", 3)
	w1.Write("k1", []float64{1, 2})
	w2.Write("k2", []float64{3, 4, 5})
	w1.Write("k3", []float64{6, 7})

	iter1 := util.NewBboltIterator(db, "model-a", 2)
	var keys1 []string
	for {
		kv, err := iter1.Read()
		if err == util.ErrIteratorExhausted {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		keys1 = append(keys1, kv.Key)
		if len(kv.Vector) != 2 {
			t.Fatalf("expected 2 dims for model-a, got %d", len(kv.Vector))
		}
	}
	if len(keys1) != 2 {
		t.Fatalf("expected 2 entries for model-a, got %d: %v", len(keys1), keys1)
	}

	iter2 := util.NewBboltIterator(db, "model-b", 3)
	count := 0
	for {
		kv, err := iter2.Read()
		if err == util.ErrIteratorExhausted {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
		if len(kv.Vector) != 3 {
			t.Fatalf("expected 3 dims for model-b, got %d", len(kv.Vector))
		}
	}
	if count != 1 {
		t.Fatalf("expected 1 entry for model-b, got %d", count)
	}
}

// 6. Round-trip encoding: write vector → read back → values identical
func TestBbolt_RoundTrip(t *testing.T) {
	db, _ := tempDB(t)
	w := util.NewBboltWriter(db, "m", 5)
	original := []float64{math.Pi, math.E, -0.0, math.Inf(1), math.SmallestNonzeroFloat64}
	w.Write("precise", original)

	iter := util.NewBboltIterator(db, "m", 5)
	kv, err := iter.Read()
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range original {
		got := kv.Vector[i]
		if math.IsInf(v, 0) {
			if !math.IsInf(got, int(math.Copysign(1, v))) {
				t.Fatalf("index %d: expected %v, got %v", i, v, got)
			}
			continue
		}
		if math.Float64bits(v) != math.Float64bits(got) {
			t.Fatalf("index %d: expected bits %x, got bits %x", i, math.Float64bits(v), math.Float64bits(got))
		}
	}
}

// 7. Large dataset (1000+ vectors) → correct count and values
func TestBbolt_LargeDataset(t *testing.T) {
	db, _ := tempDB(t)
	const n = 1500
	const dims = 16
	w := util.NewBboltWriter(db, "large", dims)

	for i := range n {
		vec := make([]float64, dims)
		for j := range vec {
			vec[j] = float64(i*dims + j)
		}
		if err := w.Write(fmt.Sprintf("key-%04d", i), vec); err != nil {
			t.Fatal(err)
		}
	}

	iter := util.NewBboltIterator(db, "large", dims)
	count := 0
	for {
		kv, err := iter.Read()
		if err == util.ErrIteratorExhausted {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(kv.Vector) != dims {
			t.Fatalf("expected %d dims, got %d", dims, len(kv.Vector))
		}
		count++
	}
	if count != n {
		t.Fatalf("expected %d vectors, got %d", n, count)
	}
}

// 8. Use with Matcher.FindTopN → correct top N results
func TestBbolt_MatcherIntegration(t *testing.T) {
	db, _ := tempDB(t)
	w := util.NewBboltWriter(db, "match", 3)
	w.Write("close", []float64{0.9, 0.1, 0})
	w.Write("exact", []float64{1, 0, 0})
	w.Write("orthogonal", []float64{0, 1, 0})
	w.Write("far", []float64{0, 0, 1})

	target := &llm.EmbeddingVector{
		Model:      "match",
		Dimensions: 3,
		Values:     []float64{1, 0, 0},
	}

	m := &classifier.Matcher{}
	iter := util.NewBboltIterator(db, "match", 3)
	results, err := m.FindTopN(context.Background(), target, iter, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Key != "exact" {
		t.Fatalf("expected top result 'exact', got %q", results[0].Key)
	}
	if results[1].Key != "close" {
		t.Fatalf("expected second result 'close', got %q", results[1].Key)
	}
	if math.Abs(results[0].Similarity-1.0) > 1e-9 {
		t.Fatalf("expected similarity 1.0, got %f", results[0].Similarity)
	}
}

// Verify that BboltWriter satisfies Writer interface and BboltIterator satisfies Iterator.
func TestBbolt_InterfaceCompliance(t *testing.T) {
	db, _ := tempDB(t)
	var _ util.Writer = util.NewBboltWriter(db, "m", 3)
	var _ util.Iterator = util.NewBboltIterator(db, "m", 3)

	// Also check via ParseBucketName
	model, dims, ok := util.ParseBucketName("vectors:my-model:768")
	if !ok || model != "my-model" || dims != 768 {
		t.Fatalf("ParseBucketName failed: model=%q dims=%d ok=%v", model, dims, ok)
	}
	_, _, ok = util.ParseBucketName("invalid")
	if ok {
		t.Fatal("expected ParseBucketName to fail on invalid input")
	}
}
