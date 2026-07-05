package core

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/sopranoworks/shoka/internal/classifier/util"
)

type MatchResult struct {
	Key        string
	Similarity float64
}

type Matcher struct{}

func (m *Matcher) FindTopN(ctx context.Context, target *Vector, iter util.Iterator, n int) ([]MatchResult, error) {
	if n <= 0 {
		return nil, nil
	}
	if target.Model != iter.Model() {
		return nil, fmt.Errorf("model mismatch: target %q vs iterator %q", target.Model, iter.Model())
	}
	if target.Dimensions != iter.Dimensions() {
		return nil, fmt.Errorf("dimension mismatch: target %d vs iterator %d", target.Dimensions, iter.Dimensions())
	}

	var results []MatchResult
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		kv, err := iter.Read()
		if errors.Is(err, util.ErrIteratorExhausted) {
			break
		}
		if err != nil {
			return nil, err
		}
		sim := CosineSimilarity(target.Values, kv.Vector)
		results = append(results, MatchResult{Key: kv.Key, Similarity: sim})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Similarity > results[j].Similarity
	})
	if len(results) > n {
		results = results[:n]
	}
	return results, nil
}

func CosineSimilarity(a, b []float64) float64 {
	if len(a) != len(b) || len(a) == 0 {
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
