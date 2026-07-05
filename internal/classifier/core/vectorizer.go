package core

import "context"

type Vector struct {
	Model      string
	Dimensions int
	Values     []float64
}

type Vectorizer interface {
	Embed(ctx context.Context, text string) (*Vector, error)
}
