package core_test

import (
	"context"
	"testing"

	"github.com/sopranoworks/shoka/internal/classifier/core"
)

type stubVectorizer struct {
	vec *core.Vector
	err error
}

func (s *stubVectorizer) Embed(_ context.Context, _ string) (*core.Vector, error) {
	return s.vec, s.err
}

func TestVectorizerInterface(t *testing.T) {
	v := &stubVectorizer{
		vec: &core.Vector{Model: "test", Dimensions: 3, Values: []float64{1, 2, 3}},
	}
	var _ core.Vectorizer = v

	got, err := v.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "test" || got.Dimensions != 3 {
		t.Fatalf("unexpected vector: %+v", got)
	}
}
